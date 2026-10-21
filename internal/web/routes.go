// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package web

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/ElcanoTek/lens/internal/catalog"
	"github.com/ElcanoTek/lens/internal/categories"
	"github.com/ElcanoTek/lens/internal/jobs"
)

// methods dispatches one path by HTTP method. GET routes answer HEAD too,
// and an unsupported method gets Starlette's JSON 405.
type methods map[string]http.HandlerFunc

func (m methods) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h, ok := m[r.Method]
	if !ok && r.Method == http.MethodHead {
		h, ok = m[http.MethodGet]
	}
	if !ok {
		allow := make([]string, 0, len(m))
		for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost} {
			if _, has := m[method]; has || (method == http.MethodHead && m[http.MethodGet] != nil) {
				allow = append(allow, method)
			}
		}
		w.Header().Set("Allow", strings.Join(allow, ", "))
		writeDetail(w, http.StatusMethodNotAllowed, "")
		return
	}
	h(w, r)
}

// form is a parsed request body (urlencoded or multipart, never the query
// string, like FastAPI's Form()).
type form url.Values

// maxFormBytes bounds a non-upload form body.
const maxFormBytes = 10 << 20

func parseForm(w http.ResponseWriter, r *http.Request) (form, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct == "multipart/form-data" {
		if err := r.ParseMultipartForm(maxFormBytes); err != nil {
			return nil, err
		}
		return form(r.MultipartForm.Value), nil
	}
	if err := r.ParseForm(); err != nil {
		return nil, err
	}
	return form(r.PostForm), nil
}

// get is a single form field: the last value sent (Starlette's FormData.get)
// with an empty string treated as absent, as FastAPI's Form() does.
func (f form) get(key string) (string, bool) {
	vals := f[key]
	if len(vals) == 0 || vals[len(vals)-1] == "" {
		return "", false
	}
	return vals[len(vals)-1], true
}

func (f form) value(key string) string {
	v, _ := f.get(key)
	return v
}

// list is a repeated form field (List[str] = Form(default=[])).
func (f form) list(key string) []string { return f[key] }

// missingField is FastAPI's 422 for an absent required form field.
func missingField(w http.ResponseWriter, names ...string) {
	detail := make([]map[string]any, 0, len(names))
	for _, name := range names {
		detail = append(detail, map[string]any{
			"type": "missing", "loc": []string{"body", name}, "msg": "Field required", "input": nil,
		})
	}
	writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"detail": detail})
}

// redirectWithParams is the 303 back to the dashboard with flash params, in
// the order given.
func redirectWithParams(w http.ResponseWriter, params ...param) {
	query := urlencode(params)
	if query == "" {
		seeOther(w, "/")
		return
	}
	seeOther(w, "/?"+query)
}

// requireAuth answers 401 for a signed-out browser on an action route.
func (s *Server) requireAuth(w http.ResponseWriter, r *http.Request) bool {
	if s.auth.Authenticated(r) {
		return true
	}
	writeDetail(w, http.StatusUnauthorized, "")
	return false
}

// queryLast is Starlette's request.query_params.get: the last value.
func queryLast(r *http.Request, key string) string {
	if v := r.URL.Query()[key]; len(v) > 0 {
		return v[len(v)-1]
	}
	return ""
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func isRegular(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

// pySuffix is pathlib's PurePath.suffix: "" for "name", ".hidden" and "a.".
func pySuffix(name string) string {
	i := strings.LastIndexByte(name, '.')
	if i <= 0 || i == len(name)-1 {
		return ""
	}
	return name[i:]
}

// ---- jobs -----------------------------------------------------------------

func (s *Server) handleEnqueue(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	f, err := parseForm(w, r)
	if err != nil {
		writeDetail(w, http.StatusBadRequest, "")
		return
	}
	inputFile, ok := f.get("input_file")
	if !ok {
		missingField(w, "input_file")
		return
	}
	safeInput, err := jobs.SafeFilename(inputFile)
	if err != nil {
		redirectWithParams(w, param{"error", err.Error()})
		return
	}
	if !exists(filepath.Join(s.dirs.Inputs, safeInput)) {
		seeOther(w, "/?error=Input+file+does+not+exist")
		return
	}
	mode, ok := f.get("mode")
	if !ok {
		mode = jobs.DefaultMode
	}
	if !jobs.ValidMode(mode) {
		seeOther(w, "/?error=Invalid+mode")
		return
	}

	var cats json.RawMessage
	if raw, ok := f.get("custom_categories"); ok {
		spec, err := categories.Parse(raw)
		if err != nil {
			redirectWithParams(w, param{"error", err.Error()})
			return
		}
		cats, err = json.Marshal(spec)
		if err != nil {
			writeDetail(w, http.StatusInternalServerError, "")
			return
		}
	}

	// The decision model only matters when custom categories are enabled.
	// The chosen model is always stored, so the run uses what the dashboard
	// showed even if config.json names a different default.
	decModel := ""
	if cats != nil {
		decModel = strings.TrimSpace(f.value("decision_model"))
		if decModel == "" {
			decModel = catalog.RecommendedDecisionModel
		}
	}
	if decModel != "" && decModel != catalog.RecommendedDecisionModel && !s.allowedModel(r, "decision", decModel) {
		seeOther(w, "/?error=Unknown+decision+model")
		return
	}

	model := strings.TrimSpace(f.value("llm_model"))
	if model == catalog.RecommendedModel {
		// The default needs no override flag: the run picks up config and
		// newer defaults automatically.
		model = ""
	} else if model != "" && !s.allowedModel(r, "classify", model) {
		seeOther(w, "/?error=Unknown+model")
		return
	}

	resModel := strings.TrimSpace(f.value("research_model"))
	if resModel == catalog.RecommendedResearchModel {
		resModel = ""
	} else if resModel != "" && !s.allowedModel(r, "research", resModel) {
		seeOther(w, "/?error=Unknown+research+model")
		return
	}

	// The checkbox posts "on" when checked and is absent when unchecked;
	// its hidden companion posts "off", so the last value wins. API clients
	// that omit both keep the server default (enabled).
	fallback, hasFallback := f.get("research_fallback")
	deep, hasDeep := f.get("deep_retry")

	_, err = s.manager.Create(jobs.NewJob{
		InputFile: safeInput, Mode: mode, Name: jobs.NormalizeJobName(f.value("name")),
		LLMModel: model, ResearchFallback: !hasFallback || fallback != "off",
		DeepRetry: !hasDeep || deep != "off", ResearchModel: resModel,
		CustomCategories: cats, DecisionModel: decModel,
	})
	if err != nil {
		redirectWithParams(w, param{"error", err.Error()})
		return
	}
	seeOther(w, "/?message=Job+queued")
}

// allowedModel warms the catalog and checks a non-default pick against it.
func (s *Server) allowedModel(r *http.Request, kind, id string) bool {
	s.catalog.Get(r.Context())
	return s.catalog.Allowed(kind, id)
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	err := s.manager.Cancel(r.PathValue("id"))
	switch {
	case errors.Is(err, jobs.ErrUnknownJob):
		seeOther(w, "/?error=Unknown+job")
	case err != nil:
		redirectWithParams(w, param{"error", err.Error()})
	default:
		seeOther(w, "/?message=Cancellation+requested")
	}
}

func (s *Server) handleDeleteJob(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	err := s.manager.Delete(r.PathValue("id"))
	switch {
	case errors.Is(err, jobs.ErrUnknownJob):
		seeOther(w, "/?error=Unknown+job")
	case err != nil:
		redirectWithParams(w, param{"error", err.Error()})
	default:
		seeOther(w, "/?message=Job+deleted")
	}
}

// ---- files ----------------------------------------------------------------

type uploadOutcome struct {
	filename string
	payload  []byte
	failure  string
}

// handleUpload accepts one or more input files. Each file is capped at
// MaxUploadBytes while it is read, so an oversized upload is refused
// without buffering it.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	reader, err := r.MultipartReader()
	if err != nil {
		missingField(w, "destination", "file")
		return
	}
	limitMB := s.maxUpload / (1024 * 1024)
	destination, haveDestination := "", false
	var outcomes []uploadOutcome
	var stored []string
	seenFile := false
	pending := 0 // bytes of files read before the destination field
	store := func(o uploadOutcome) uploadOutcome {
		// o.filename passed ValidateInputFilename in readUpload; re-derive
		// the path from the sanitizer here so the write is visibly contained.
		name, err := jobs.SafeFilename(o.filename)
		if err != nil {
			o.failure = err.Error()
			return o
		}
		path := filepath.Join(s.dirs.Inputs, name)
		if err := os.WriteFile(path, o.payload, 0o644); err != nil { //nolint:gosec // the library is operator-readable
			o.failure = err.Error()
			return o
		}
		// Pre-compute the content breakdown now so the file library renders
		// its counts immediately on reload.
		s.dirs.Breakdown(path)
		stored = append(stored, o.filename)
		o.payload = nil
		return o
	}
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			writeDetail(w, http.StatusBadRequest, "")
			return
		}
		_, params, _ := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
		field := part.FormName()
		rawName, isFile := params["filename"]
		switch {
		case field == "destination" && !isFile:
			value, _ := io.ReadAll(io.LimitReader(part, 4096))
			destination, haveDestination = string(value), true
		case field == "file" && isFile && haveDestination && destination != "inputs":
			seenFile = true // refused below; don't read it
		case field == "file" && isFile:
			seenFile = true
			o := s.readUpload(part, rawName, limitMB)
			switch {
			case o.failure != "":
			case haveDestination:
				o = store(o)
			case pending+len(o.payload) > s.maxUpload:
				// Files ahead of the destination field are held until it
				// arrives; cap what is held so a crafted form cannot pile
				// up unbounded uploads in memory (browsers send the
				// destination first, and then nothing is held at all).
				o = uploadOutcome{filename: rawName, failure: "the destination field must precede the files"}
			default:
				pending += len(o.payload)
			}
			outcomes = append(outcomes, o)
		}
		_, _ = io.Copy(io.Discard, part)
		_ = part.Close()
	}
	if !haveDestination || destination == "" || !seenFile {
		var missing []string
		if !haveDestination || destination == "" {
			missing = append(missing, "destination")
		}
		if !seenFile {
			missing = append(missing, "file")
		}
		missingField(w, missing...)
		return
	}
	if destination != "inputs" {
		redirectWithParams(w, param{"files_error", "Only input uploads are allowed"}, param{"files_scope", "inputs"})
		return
	}
	var failed []string
	for i, o := range outcomes {
		if o.failure == "" && o.payload != nil {
			// The destination field came after this file; store it now.
			o = store(o)
			outcomes[i] = o
		}
		if o.failure != "" {
			name := strings.TrimSpace(o.filename)
			if name == "" {
				name = "unnamed-file"
			}
			failed = append(failed, fmt.Sprintf("%s (%s)", name, o.failure))
		}
	}
	params := []param{{"files_scope", "inputs"}}
	switch len(stored) {
	case 0:
	case 1:
		params = append(params, param{"files_message", "Uploaded " + stored[0]})
	default:
		params = append(params, param{"files_message", fmt.Sprintf("Uploaded %d files", len(stored))})
	}
	if len(failed) > 0 {
		params = append(params, param{"files_error", "Skipped: " + strings.Join(failed[:min(3, len(failed))], "; ")})
	} else if len(stored) == 0 {
		params = append(params, param{"files_error", "No files uploaded"})
	}
	redirectWithParams(w, params...)
}

// readUpload validates one file part. On failure, filename holds the raw
// name the browser sent (for the error message).
func (s *Server) readUpload(part io.Reader, rawName string, limitMB int) uploadOutcome {
	fail := func(msg string) uploadOutcome { return uploadOutcome{filename: rawName, failure: msg} }
	filename, err := jobs.ValidateInputFilename(rawName)
	if err != nil {
		return fail(err.Error())
	}
	payload, err := io.ReadAll(io.LimitReader(part, int64(s.maxUpload)+1))
	if err != nil {
		return fail(err.Error())
	}
	if len(payload) == 0 {
		return fail("Uploaded file is empty")
	}
	if len(payload) > s.maxUpload {
		return fail(fmt.Sprintf("File exceeds %d MB limit", limitMB))
	}
	if err := jobs.VerifyInputPayload(filename, payload); err != nil {
		return fail(err.Error())
	}
	return uploadOutcome{filename: filename, payload: payload}
}

func (s *Server) handleRename(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	f, err := parseForm(w, r)
	if err != nil {
		writeDetail(w, http.StatusBadRequest, "")
		return
	}
	destination, ok1 := f.get("destination")
	filename, ok2 := f.get("filename")
	newName, ok3 := f.get("new_name")
	if !ok1 || !ok2 || !ok3 {
		var missing []string
		for _, kv := range []struct {
			name string
			ok   bool
		}{{"destination", ok1}, {"filename", ok2}, {"new_name", ok3}} {
			if !kv.ok {
				missing = append(missing, kv.name)
			}
		}
		missingField(w, missing...)
		return
	}
	scope := param{"files_scope", destination}
	if destination != "inputs" {
		redirectWithParams(w, param{"files_error", "Only input files can be renamed"}, scope)
		return
	}
	safeOld, err := jobs.SafeFilename(filename)
	if err != nil {
		redirectWithParams(w, param{"files_error", err.Error()}, scope)
		return
	}
	cleanedNew, err := jobs.SafeFilename(newName)
	if err != nil {
		redirectWithParams(w, param{"files_error", err.Error()}, scope)
		return
	}
	// Keep the original extension if the new name doesn't carry one: users
	// renaming "Q1 list (3).csv" to "JPMC allowlist" shouldn't have to
	// remember the suffix.
	if pySuffix(cleanedNew) == "" {
		cleanedNew += pySuffix(safeOld)
	}
	safeNew, err := jobs.ValidateInputFilename(cleanedNew)
	if err != nil {
		redirectWithParams(w, param{"files_error", err.Error()}, scope)
		return
	}
	oldPath := filepath.Join(s.dirs.Inputs, safeOld)
	newPath := filepath.Join(s.dirs.Inputs, safeNew)
	if !isRegular(oldPath) {
		redirectWithParams(w, param{"files_error", "File not found"}, scope)
		return
	}
	if safeNew == safeOld {
		redirectWithParams(w, param{"files_message", "Name unchanged"}, scope)
		return
	}
	if exists(newPath) {
		redirectWithParams(w, param{"files_error", fmt.Sprintf("A file named %s already exists", safeNew)}, scope)
		return
	}
	if err := os.Rename(oldPath, newPath); err != nil {
		redirectWithParams(w, param{"files_error", err.Error()}, scope)
		return
	}
	redirectWithParams(w, param{"files_message", "Renamed to " + safeNew}, scope)
}

func (s *Server) dirFor(destination string) (string, bool) {
	switch destination {
	case "inputs":
		return s.dirs.Inputs, true
	case "outputs":
		return s.dirs.Outputs, true
	}
	return "", false
}

// downloadTypes are the Content-Types Starlette's FileResponse sent.
var downloadTypes = map[string]string{
	".csv":  "text/csv; charset=utf-8",
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".json": "application/json",
	".log":  "text/plain; charset=utf-8",
	".txt":  "text/plain; charset=utf-8",
	".zip":  "application/zip",
}

// contentDisposition is Starlette's FileResponse header: a plain quoted
// filename when it needs no escaping, RFC 5987 otherwise.
func contentDisposition(filename string) string {
	quoted := pyQuote(filename, "/")
	if quoted != filename {
		return "attachment; filename*=utf-8''" + quoted
	}
	return `attachment; filename="` + filename + `"`
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	destination := r.PathValue("destination")
	dir, ok := s.dirFor(destination)
	if !ok {
		writeDetail(w, http.StatusNotFound, "")
		return
	}
	safeName, err := jobs.SafeFilename(r.PathValue("filename"))
	if err != nil || jobs.IsProtectedOutput(destination, safeName) {
		writeDetail(w, http.StatusNotFound, "")
		return
	}
	path := filepath.Join(dir, safeName)
	file, err := os.Open(path) //nolint:gosec // sanitized name inside a managed directory
	if err != nil {
		writeDetail(w, http.StatusNotFound, "")
		return
	}
	defer func() { _ = file.Close() }()
	st, err := file.Stat()
	if err != nil || !st.Mode().IsRegular() {
		writeDetail(w, http.StatusNotFound, "")
		return
	}
	ctype, ok := downloadTypes[strings.ToLower(pySuffix(safeName))]
	if !ok {
		ctype = "text/plain; charset=utf-8"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Disposition", contentDisposition(safeName))
	http.ServeContent(w, r, safeName, st.ModTime(), file)
}

func (s *Server) handleDeleteFile(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	f, err := parseForm(w, r)
	if err != nil {
		writeDetail(w, http.StatusBadRequest, "")
		return
	}
	destination, ok1 := f.get("destination")
	filename, ok2 := f.get("filename")
	if !ok1 || !ok2 {
		var missing []string
		if !ok1 {
			missing = append(missing, "destination")
		}
		if !ok2 {
			missing = append(missing, "filename")
		}
		missingField(w, missing...)
		return
	}
	scope := param{"files_scope", destination}
	dir, ok := s.dirFor(destination)
	if !ok {
		redirectWithParams(w, param{"files_error", "Invalid destination"}, scope)
		return
	}
	safeName, err := jobs.SafeFilename(filename)
	if err != nil {
		redirectWithParams(w, param{"files_error", err.Error()}, scope)
		return
	}
	path := filepath.Join(dir, safeName)
	if jobs.IsProtectedOutput(destination, safeName) || !isRegular(path) {
		redirectWithParams(w, param{"files_error", "File not found"}, scope)
		return
	}
	if err := os.Remove(path); err != nil {
		redirectWithParams(w, param{"files_error", err.Error()}, scope)
		return
	}
	redirectWithParams(w, param{"files_message", "Deleted " + safeName}, scope)
}

// selectedFiles resolves a batch selection to existing regular files,
// silently skipping unsafe names and the job-state file.
func selectedFiles(dir, destination string, names []string) []string {
	var out []string
	for _, name := range names {
		safeName, err := jobs.SafeFilename(name)
		if err != nil || jobs.IsProtectedOutput(destination, safeName) {
			continue
		}
		if path := filepath.Join(dir, safeName); isRegular(path) {
			out = append(out, path)
		}
	}
	return out
}

func (s *Server) handleBatchDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	f, err := parseForm(w, r)
	if err != nil {
		writeDetail(w, http.StatusBadRequest, "")
		return
	}
	destination, ok := f.get("destination")
	if !ok {
		missingField(w, "destination")
		return
	}
	scope := param{"files_scope", destination}
	dir, ok := s.dirFor(destination)
	if !ok {
		redirectWithParams(w, param{"files_error", "Invalid destination"}, scope)
		return
	}
	names := f.list("filenames")
	if len(names) == 0 {
		redirectWithParams(w, param{"files_error", "No files selected"}, scope)
		return
	}
	deleted := 0
	for _, path := range selectedFiles(dir, destination, names) {
		if os.Remove(path) == nil {
			deleted++
		}
	}
	redirectWithParams(w, param{"files_message", fmt.Sprintf("Deleted %d file(s)", deleted)}, scope)
}

// handleBatchDownload streams a zip of the selection straight to the
// client.
func (s *Server) handleBatchDownload(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	f, err := parseForm(w, r)
	if err != nil {
		writeDetail(w, http.StatusBadRequest, "")
		return
	}
	destination, ok := f.get("destination")
	if !ok {
		missingField(w, "destination")
		return
	}
	dir, ok := s.dirFor(destination)
	if !ok {
		writeDetail(w, http.StatusNotFound, "")
		return
	}
	names := f.list("filenames")
	if len(names) == 0 {
		writeDetail(w, http.StatusBadRequest, "No files selected")
		return
	}
	selected := selectedFiles(dir, destination, names)
	if len(selected) == 0 {
		writeDetail(w, http.StatusNotFound, "No valid files found")
		return
	}
	stamp := s.now().UTC().Format("20060102150405")
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="lens_%s_%s.zip"`, destination, stamp))
	w.WriteHeader(http.StatusOK)
	bundle := zip.NewWriter(w)
	for _, path := range selected {
		if err := addToZip(bundle, path); err != nil {
			logger.Warn("batch download: " + err.Error())
		}
	}
	if err := bundle.Close(); err != nil {
		logger.Warn("batch download: " + err.Error())
	}
}

func addToZip(bundle *zip.Writer, path string) error {
	file, err := os.Open(path) //nolint:gosec // path from selectedFiles
	if err != nil {
		// Deleted between selection and archiving; skip it.
		return nil //nolint:nilerr // a vanished file is skipped, as before
	}
	defer func() { _ = file.Close() }()
	st, err := file.Stat()
	if err != nil {
		return err
	}
	header, err := zip.FileInfoHeader(st)
	if err != nil {
		return err
	}
	header.Name = filepath.Base(path)
	header.Method = zip.Deflate
	dst, err := bundle.CreateHeader(header)
	if err != nil {
		return err
	}
	_, err = io.Copy(dst, file)
	return err
}

// ---- live status ----------------------------------------------------------

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

type liveJob struct {
	ID         string `json:"id"`
	Name       any    `json:"name"`
	Status     string `json:"status"`
	Mode       string `json:"mode"`
	InputFile  string `json:"input_file"`
	CreatedAt  string `json:"created_at"`
	StartedAt  any    `json:"started_at"`
	FinishedAt any    `json:"finished_at"`
	Error      any    `json:"error"`
	ReturnCode *int   `json:"return_code"`
}

type liveResponse struct {
	Job      *liveJob       `json:"job"`
	Progress map[string]any `json:"progress"`
	LogLines []string       `json:"log_lines"`
	Message  string         `json:"message"`
}

func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	id := queryLast(r, "job_id")
	if id == "" {
		id = s.manager.CurrentID()
	}
	if id == "" {
		writeJSON(w, http.StatusOK, liveResponse{Progress: map[string]any{}, LogLines: []string{}, Message: "No active job right now"})
		return
	}
	job, ok := s.manager.Get(id)
	if !ok {
		writeDetail(w, http.StatusNotFound, "Unknown job")
		return
	}
	progress := map[string]any{}
	logLines := []string{}
	if job.ProgressFile != "" {
		progress = jobs.ReadProgress(filepath.Join(s.dirs.Outputs, job.ProgressFile))
	}
	if job.LogFile != "" {
		logLines = jobs.TailLines(filepath.Join(s.dirs.Outputs, job.LogFile), 50)
	}
	writeJSON(w, http.StatusOK, liveResponse{
		Job: &liveJob{
			ID: job.ID, Name: nullable(job.Name), Status: job.Status, Mode: job.Mode, InputFile: job.InputFile,
			CreatedAt: job.CreatedAt, StartedAt: nullable(job.StartedAt), FinishedAt: nullable(job.FinishedAt),
			Error: nullable(job.Error), ReturnCode: job.ReturnCode,
		},
		Progress: progress, LogLines: logLines, Message: "ok",
	})
}

type queueItem struct {
	ID         string         `json:"id"`
	Name       any            `json:"name"`
	Status     string         `json:"status"`
	StartedAt  any            `json:"started_at"`
	FinishedAt any            `json:"finished_at"`
	Progress   map[string]any `json:"progress"`
}

func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	list := s.manager.List()
	items := make([]queueItem, 0, len(list))
	for _, job := range list {
		progress := map[string]any{}
		if job.ProgressFile != "" {
			progress = jobs.ReadProgress(filepath.Join(s.dirs.Outputs, job.ProgressFile))
		}
		items = append(items, queueItem{
			ID: job.ID, Name: nullable(job.Name), Status: job.Status,
			StartedAt: nullable(job.StartedAt), FinishedAt: nullable(job.FinishedAt), Progress: progress,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": items, "message": "ok"})
}

type healthResponse struct {
	Status   string `json:"status"`
	Time     string `json:"time"`
	AuthMode string `json:"auth_mode"`
}

// handleHealth is the public readiness probe. The provider was built (and
// central auth fully validated) at startup, so a 200 here means sign-in can
// work.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	writeJSON(w, http.StatusOK, healthResponse{
		Status: "ok", Time: s.now().UTC().Format("2006-01-02T15:04:05.000000+00:00"), AuthMode: s.auth.Mode(),
	})
}

// ---- sign-in --------------------------------------------------------------

// handleSignedOut is deliberately public and identity-free. Redirecting
// back to / would start central SSO immediately while the Auth cookie is
// still valid, making the local logout appear ineffective, so the page also
// points at the account page, the one place the Auth session itself ends.
func (s *Server) handleSignedOut(w http.ResponseWriter, r *http.Request) {
	s.render(w, "signed_out.html", struct {
		Nonce      string
		AccountURL string
	}{Nonce(r), s.auth.AccountURL()})
}

// ---- helpers shared with the spool ----------------------------------------

// marshalSpec renders validated categories for jobs.NewJob.
func marshalSpec(spec *categories.Spec) (json.RawMessage, error) {
	raw, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(bytes.TrimSpace(raw)), nil
}
