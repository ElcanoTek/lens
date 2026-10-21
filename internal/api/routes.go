// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ElcanoTek/lens/internal/apitoken"
)

// Routes returns the REST handler. It serves full paths under /api/v1, so
// the web server mounts it as mux.Handle("/api/v1/", api.Routes()) without
// stripping the prefix. Unknown paths get a JSON 404 in the API's shape.
func (a *API) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/whoami", a.handleWhoami)
	mux.HandleFunc("GET /api/v1/jobs", func(w http.ResponseWriter, r *http.Request) {
		// Query values arrive as strings and are validated by the API's own
		// layer (after the bearer check), so a bad ?limit= gets the
		// documented invalid_argument body.
		q := r.URL.Query()
		var status any
		if values, ok := q["status"]; ok {
			status = values[0]
		}
		a.run(w, r, apitoken.ScopeRead, http.StatusOK, func(*apitoken.Token) (Body, error) {
			return a.ListJobs(r, status, q.Get("limit"))
		})
	})
	mux.HandleFunc("POST /api/v1/jobs", func(w http.ResponseWriter, r *http.Request) {
		a.run(w, r, apitoken.ScopeRun, http.StatusCreated, func(token *apitoken.Token) (Body, error) {
			args, upload, err := a.createArgs(r)
			if err != nil {
				return nil, err
			}
			return a.CreateJob(r, token, args, upload)
		})
	})
	mux.HandleFunc("GET /api/v1/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		a.run(w, r, apitoken.ScopeRead, http.StatusOK, func(*apitoken.Token) (Body, error) {
			return a.GetJob(r, r.PathValue("id"))
		})
	})
	mux.HandleFunc("POST /api/v1/jobs/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		a.run(w, r, apitoken.ScopeRun, http.StatusOK, func(token *apitoken.Token) (Body, error) {
			return a.CancelJob(r, token, r.PathValue("id"))
		})
	})
	mux.HandleFunc("GET /api/v1/jobs/{id}/results", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		var columns any
		if raw := q.Get("columns"); raw != "" {
			picked := []string{}
			for _, c := range strings.Split(raw, ",") {
				if c = strings.TrimSpace(c); c != "" {
					picked = append(picked, c)
				}
			}
			columns = picked
		}
		var quality any
		if values, ok := q["quality"]; ok {
			quality = values[0]
		}
		a.run(w, r, apitoken.ScopeRead, http.StatusOK, func(*apitoken.Token) (Body, error) {
			return a.JobResults(r, r.PathValue("id"), ResultsQuery{
				Offset: q.Get("offset"), Limit: q.Get("limit"), Columns: columns, Quality: quality,
			})
		})
	})
	mux.HandleFunc("POST /api/v1/jobs/{id}/files/{kind}/link", func(w http.ResponseWriter, r *http.Request) {
		a.run(w, r, apitoken.ScopeRead, http.StatusOK, func(token *apitoken.Token) (Body, error) {
			return a.DownloadLink(r, token, r.PathValue("id"), r.PathValue("kind"))
		})
	})
	mux.HandleFunc("GET /api/v1/jobs/{id}/files/{kind}", a.handleFile)
	mux.HandleFunc("GET /api/v1/downloads/{ticket}", a.handleSignedDownload)
	mux.HandleFunc("GET /api/v1/inputs", func(w http.ResponseWriter, r *http.Request) {
		a.run(w, r, apitoken.ScopeRead, http.StatusOK, func(*apitoken.Token) (Body, error) {
			return a.ListInputFiles()
		})
	})
	mux.HandleFunc("GET /api/v1/options", func(w http.ResponseWriter, r *http.Request) {
		a.run(w, r, apitoken.ScopeRead, http.StatusOK, func(*apitoken.Token) (Body, error) {
			return a.GetOptions(r.Context())
		})
	})
	mux.HandleFunc("/api/v1/", func(w http.ResponseWriter, r *http.Request) {
		a.WriteError(w, r, apiErr(http.StatusNotFound, "not_found", "No such API route: %s %s", r.Method, r.URL.Path))
	})
	return mux
}

// WriteJSON writes body as a JSON response (no HTML escaping, as Starlette's
// JSONResponse).
func WriteJSON(w http.ResponseWriter, status int, body any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(body); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// InternalErrorBody is what a caller sees for an unexpected failure; the
// detail goes to the service log, never to the caller.
func InternalErrorBody() Body {
	return Body{"ok": false, "error": "Lens hit an internal error; see the Lens service log.", "code": "internal_error"}
}

// WriteError answers with err: an *Error in its own shape (401s carry
// WWW-Authenticate: Bearer), anything else as a logged 500.
func (a *API) WriteError(w http.ResponseWriter, r *http.Request, err error) {
	var e *Error
	if !errors.As(err, &e) {
		a.log.Error("API request " + r.Method + " " + r.URL.Path + " failed: " + err.Error())
		WriteJSON(w, http.StatusInternalServerError, InternalErrorBody())
		return
	}
	if e.Status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="lens"`)
	}
	WriteJSON(w, e.Status, e.Body())
}

func (a *API) run(w http.ResponseWriter, r *http.Request, scope string, okStatus int, op func(*apitoken.Token) (Body, error)) {
	token, err := a.Authenticate(r)
	if err == nil {
		err = RequireScope(token, scope)
	}
	if err != nil {
		a.WriteError(w, r, err)
		return
	}
	body, err := op(token)
	if err != nil {
		a.WriteError(w, r, err)
		return
	}
	WriteJSON(w, okStatus, body)
}

func (a *API) handleWhoami(w http.ResponseWriter, r *http.Request) {
	token, err := a.Authenticate(r)
	if err != nil {
		a.WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, Body{"label": token.Label, "scopes": token.Scopes, "token_prefix": token.Prefix})
}

// createArgs reads POST /jobs: a JSON object, or a multipart form with
// file, optional name/input_file and options (a JSON string of the rest).
func (a *API) createArgs(r *http.Request) (map[string]any, *Upload, error) {
	limit := a.maxUpload*4/3 + MultipartOverheadBytes
	if r.ContentLength > limit {
		// Refuse before parsing: neither a JSON body (base64) nor a form can
		// carry an acceptable file this large.
		return nil, nil, a.fileTooLarge()
	}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		return a.multipartArgs(r, limit)
	}
	raw, err := ReadBodyCapped(r, limit)
	if err != nil {
		return nil, nil, err
	}
	var body any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, nil, invalidArg("Send a JSON object or multipart/form-data")
	}
	args, ok := body.(map[string]any)
	if !ok {
		return nil, nil, invalidArg("The request body must be a JSON object")
	}
	return args, nil, nil
}

func badForm(detail string) *Error { return invalidArg("Bad multipart form: %s", detail) }

// multipartArgs parses the form while it streams in, through a reader that
// stops at limit, so a chunked upload with no Content-Length is refused
// before it can fill memory. The file part is buffered up to the upload cap
// (StoreUniqueInput needs the bytes); past it the rest is drained, so the
// whole-body cap still decides between too_large and file_too_large the way
// the Python release did (body cap during parsing, file cap after).
func (a *API) multipartArgs(r *http.Request, limit int64) (map[string]any, *Upload, error) {
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || params["boundary"] == "" {
		return nil, nil, badForm("missing boundary")
	}
	body := &cappedReader{r: r.Body, limit: limit}
	reader := multipart.NewReader(body, params["boundary"])
	fields := map[string]string{}
	var upload *Upload
	fileTooBig := false
	nFields, nFiles := 0, 0
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if body.tripped {
			return nil, nil, body.err()
		}
		if err != nil {
			return nil, nil, badForm(err.Error())
		}
		_, disposition, _ := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
		filename, isFile := disposition["filename"]
		name := disposition["name"]
		if isFile {
			nFiles++
			if nFiles > maxFormFiles {
				return nil, nil, badForm("Too many files. Maximum number of files is 1.")
			}
			payload, err := io.ReadAll(io.LimitReader(part, a.maxUpload+1))
			if err == nil && int64(len(payload)) > a.maxUpload {
				fileTooBig = true
				payload = nil
				_, err = io.Copy(io.Discard, part)
			}
			if body.tripped {
				return nil, nil, body.err()
			}
			if err != nil {
				return nil, nil, badForm(err.Error())
			}
			if name == "file" {
				upload = &Upload{Filename: filename, Payload: payload}
			}
			continue
		}
		nFields++
		if nFields > maxFormFields {
			return nil, nil, badForm("Too many fields. Maximum number of fields is 16.")
		}
		value, err := io.ReadAll(io.LimitReader(part, maxFormFieldBytes+1))
		if body.tripped {
			return nil, nil, body.err()
		}
		if err != nil {
			return nil, nil, badForm(err.Error())
		}
		if len(value) > maxFormFieldBytes {
			return nil, nil, badForm("Part exceeded maximum size of 1024KB.")
		}
		fields[name] = string(value)
	}
	args := map[string]any{}
	if raw := fields["options"]; raw != "" {
		var options any
		if err := json.Unmarshal([]byte(raw), &options); err != nil {
			return nil, nil, invalidArg("options must be a JSON object")
		}
		var ok bool
		if args, ok = options.(map[string]any); !ok {
			return nil, nil, invalidArg("options must be a JSON object")
		}
	}
	for _, key := range []string{"name", "input_file"} {
		if v := fields[key]; v != "" {
			args[key] = v
		}
	}
	if fileTooBig {
		return nil, nil, a.fileTooLarge()
	}
	if upload != nil && len(upload.Payload) == 0 {
		return nil, nil, apiErr(http.StatusBadRequest, "invalid_file", "The uploaded file is empty")
	}
	return args, upload, nil
}

var mediaTypes = map[string]string{
	"output":     "text/csv; charset=utf-8",
	"log":        "text/plain; charset=utf-8",
	"categories": "application/json",
}

// quoteFilename is Python's urllib.parse.quote (safe "/"), which Starlette
// uses to decide between filename= and filename*= in Content-Disposition.
func quoteFilename(name string) string {
	const unreserved = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_.-~/"
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		if strings.IndexByte(unreserved, c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// serveFile streams one of a job's files as an attachment.
func (a *API) serveFile(w http.ResponseWriter, r *http.Request, jobID, kind string) error {
	job, err := a.getJobOr404(jobID)
	if err != nil {
		return err
	}
	missing := apiErr(http.StatusNotFound, "file_missing", "Job %s has no %s file", job.ID, kind)
	path := a.filePath(job, kind)
	if !isFile(path) {
		return missing
	}
	f, err := os.Open(path) //nolint:gosec // path built from a validated job filename
	if err != nil {
		return missing
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return missing
	}
	name := filepath.Base(path)
	media := mediaTypes[kind]
	if media == "" {
		switch strings.ToLower(filepath.Ext(name)) {
		case ".csv":
			media = "text/csv; charset=utf-8"
		case ".xlsx":
			media = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
		default:
			media = "application/octet-stream"
		}
	}
	if quoted := quoteFilename(name); quoted != name {
		w.Header().Set("Content-Disposition", "attachment; filename*=utf-8''"+quoted)
	} else {
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	}
	w.Header().Set("Content-Type", media)
	http.ServeContent(w, r, name, st.ModTime(), f)
	return nil
}

func (a *API) handleFile(w http.ResponseWriter, r *http.Request) {
	token, err := a.Authenticate(r)
	if err == nil {
		err = RequireScope(token, apitoken.ScopeRead)
	}
	if err == nil {
		kind := r.PathValue("kind")
		if !slices.Contains(FileKinds, kind) {
			err = apiErr(http.StatusNotFound, "file_missing", "file must be one of %s", pyList(FileKinds))
		} else {
			err = a.serveFile(w, r, r.PathValue("id"), kind)
		}
	}
	if err != nil {
		a.WriteError(w, r, err)
	}
}

// handleSignedDownload serves a signed link. It needs no bearer token: the
// ticket is the credential, scoped to one file of one job.
func (a *API) handleSignedDownload(w http.ResponseWriter, r *http.Request) {
	_, jobID, kind, err := a.tokens.VerifyDownload(r.Context(), r.PathValue("ticket"))
	var te *apitoken.TokenError
	switch {
	case errors.As(err, &te):
		err = apiErr(http.StatusForbidden, "invalid_link", "%s", te.Error())
	case err != nil:
	case !slices.Contains(FileKinds, kind):
		err = apiErr(http.StatusForbidden, "invalid_link", "Invalid download link")
	default:
		err = a.serveFile(w, r, jobID, kind)
	}
	if err != nil {
		a.WriteError(w, r, err)
	}
}
