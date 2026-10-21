// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ElcanoTek/lens/internal/apitoken"
	"github.com/ElcanoTek/lens/internal/catalog"
	"github.com/ElcanoTek/lens/internal/categories"
	"github.com/ElcanoTek/lens/internal/config"
	"github.com/ElcanoTek/lens/internal/jobs"
)

// Operations shared by REST and MCP. Each returns a Body or an error; an
// *Error is the caller's fault (and is shown to them), anything else is an
// internal failure.

func (a *API) fileTooLarge() *Error {
	return apiErr(http.StatusRequestEntityTooLarge, "file_too_large", "File exceeds %d MB", a.maxUpload/(1024*1024))
}

func (a *API) getJobOr404(jobID any) (*jobs.Job, error) {
	id := strings.TrimSpace(strOrEmpty(jobID))
	if !jobIDRe.MatchString(id) {
		return nil, apiErr(http.StatusBadRequest, "invalid_job_id",
			"%s is not a Lens job id (YYYYMMDDHHMMSS-xxxxxx).", pyRepr(id))
	}
	job, ok := a.manager.Get(id)
	if !ok {
		return nil, apiErr(http.StatusNotFound, "job_not_found", "No job %s on this Lens. Use list_jobs to find it.", id)
	}
	return &job, nil
}

// ListJobs is list_jobs: recent jobs, newest first, optionally only those in
// status. status nil means any; limit follows boundedInt.
func (a *API) ListJobs(r *http.Request, status, limit any) (Body, error) {
	want := ""
	if status != nil {
		s, ok := status.(string)
		if !ok || !slices.Contains(JobStatuses, s) {
			return nil, apiErr(http.StatusBadRequest, "invalid_status", "status must be one of %s", pyList(JobStatuses))
		}
		want = s
	}
	count, err := boundedInt(limit, "limit", DefaultJobList, 1, MaxJobList)
	if err != nil {
		return nil, err
	}
	all := a.manager.List()
	matching := all[:0:0]
	for i := range all {
		if want == "" || all[i].Status == want {
			matching = append(matching, all[i])
		}
	}
	views := []Body{}
	for i := range matching[:min(count, len(matching))] {
		views = append(views, a.jobView(&matching[i], r, false))
	}
	return Body{"jobs": views, "total_matching": len(matching)}, nil
}

// GetJob is get_job: the detail view of one job.
func (a *API) GetJob(r *http.Request, jobID any) (Body, error) {
	job, err := a.getJobOr404(jobID)
	if err != nil {
		return nil, err
	}
	return a.jobView(job, r, true), nil
}

var unsafeNameChars = regexp.MustCompile(`[^A-Za-z0-9._ ()-]`)

// inputName names the library file an inline items list is stored as.
func (a *API) inputName(name, suffix string) string {
	stamp := a.now().UTC().Format("20060102-150405")
	if name == "" {
		name = "api-list"
	}
	stem := unsafeNameChars.ReplaceAllString(strings.TrimSpace(name), "_")
	if len(stem) > 60 { // all ASCII after the substitution
		stem = stem[:60]
	}
	stem = strings.Trim(stem, "._ -")
	if stem == "" {
		stem = "api-list"
	}
	return stem + " " + stamp + suffix
}

// itemsCSV turns an inline identifier list into a one-column CSV: trimmed,
// blank entries dropped, case-insensitively de-duplicated.
func itemsCSV(items any) ([]byte, error) {
	list, ok := items.([]any)
	if !ok || len(list) == 0 {
		return nil, invalidArg("items must be a non-empty list of identifiers")
	}
	if len(list) > MaxInlineItems {
		return nil, apiErr(http.StatusBadRequest, "too_many_items",
			"items holds %d identifiers; the inline limit is %d. "+
				"Send a CSV/XLSX file instead (content_base64 with filename).", len(list), MaxInlineItems)
	}
	cleaned := []string{}
	seen := map[string]bool{}
	for _, raw := range list {
		s, ok := raw.(string)
		if !ok {
			return nil, invalidArg("every item must be a string")
		}
		value := strings.TrimSpace(s)
		if value == "" {
			continue
		}
		if utf8.RuneCountInString(value) > MaxItemChars || strings.ContainsAny(value, "\r\n") {
			head := []rune(value)
			head = head[:min(40, len(head))]
			return nil, invalidArg("item %s… is not a single identifier", pyRepr(string(head)))
		}
		key := strings.ToLower(value)
		if !seen[key] {
			seen[key] = true
			cleaned = append(cleaned, value)
		}
	}
	if len(cleaned) == 0 {
		return nil, invalidArg("items contained no identifiers")
	}
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	w.UseCRLF = true // Python's csv.writer line terminator
	_ = w.Write([]string{"Domain"})
	for _, v := range cleaned {
		_ = w.Write([]string{v})
	}
	w.Flush()
	return buf.Bytes(), w.Error()
}

func (a *API) decodeUpload(contentB64, filename any) (string, []byte, error) {
	name, ok := filename.(string)
	if !ok || strings.TrimSpace(name) == "" {
		return "", nil, invalidArg("filename (ending .csv or .xlsx) is required with content_base64")
	}
	safe, err := jobs.ValidateInputFilename(strings.TrimSpace(name))
	if err != nil {
		return "", nil, apiErr(http.StatusBadRequest, "invalid_file", "%s", err.Error())
	}
	text, ok := contentB64.(string)
	if !ok || text == "" {
		return "", nil, invalidArg("content_base64 must be a non-empty base64 string")
	}
	if int64(len(text)) > a.maxUpload*4/3+4 {
		return "", nil, a.fileTooLarge()
	}
	// Strict, like Python's b64decode(validate=True): Go's decoder would
	// otherwise skip embedded newlines.
	payload, err := base64.StdEncoding.DecodeString(text)
	if err != nil || strings.ContainsAny(text, "\r\n") {
		return "", nil, apiErr(http.StatusBadRequest, "invalid_file", "content_base64 is not valid base64")
	}
	return safe, payload, nil
}

func (a *API) storeInput(filename string, payload []byte) (string, error) {
	if len(payload) == 0 {
		return "", apiErr(http.StatusBadRequest, "invalid_file", "The input file is empty")
	}
	if int64(len(payload)) > a.maxUpload {
		return "", a.fileTooLarge()
	}
	if err := jobs.VerifyInputPayload(filename, payload); err != nil {
		return "", apiErr(http.StatusBadRequest, "invalid_file", "%s", err.Error())
	}
	if len(filename) > MaxFilenameBytes {
		return "", apiErr(http.StatusBadRequest, "invalid_file", "Filename is longer than %d bytes; shorten it", MaxFilenameBytes)
	}
	if err := os.MkdirAll(a.dirs.Inputs, 0o755); err != nil { //nolint:gosec // managed-files is operator-readable
		return "", err
	}
	stored, err := jobs.StoreUniqueInput(a.dirs.Inputs, filename, payload)
	if err != nil {
		return "", err
	}
	if err := a.requireIdentifiers(stored); err != nil {
		return "", err
	}
	return stored, nil
}

func (a *API) requireIdentifiers(stored string) error {
	if a.dirs.Breakdown(filepath.Join(a.dirs.Inputs, stored))["total"] == 0 {
		// Keep the file (it is now in the library and harmless) but refuse
		// to queue a run that would find nothing to classify.
		e := apiErr(http.StatusBadRequest, "no_identifiers",
			"Lens found no websites, apps or CTV channels in %s. Put domains, app "+
				"store IDs or package names in a column named Domain (or url/app_id), or send a "+
				"CTV list with a Bundle ID column.", stored)
		e.Details = map[string]any{"input_file": stored}
		return e
	}
	return nil
}

func cleanModel(args map[string]any, name string) (string, error) {
	v := args[name]
	if !truthy(v) {
		return "", nil
	}
	model, err := jobs.CleanModelID(pyStr(v))
	if err != nil {
		return "", apiErr(http.StatusBadRequest, "invalid_model", "%s", err.Error())
	}
	return model, nil
}

type models struct{ llm, research, decision string }

// validatedModels applies the dashboard form's model rules: the recommended
// model passes, anything else must be in the live catalog.
func (a *API) validatedModels(ctx context.Context, args map[string]any, hasCategories bool) (models, error) {
	var m models
	var err error
	if m.llm, err = cleanModel(args, "llm_model"); err != nil {
		return m, err
	}
	if m.research, err = cleanModel(args, "research_model"); err != nil {
		return m, err
	}
	if m.decision, err = cleanModel(args, "decision_model"); err != nil {
		return m, err
	}
	checks := []struct{ value, kind, name string }{
		{m.llm, "classify", "llm_model"},
		{m.research, "research", "research_model"},
	}
	if hasCategories {
		// Omitted stays empty so the run inherits this instance's configured
		// decision_model; only an explicit choice is validated and passed.
		checks = append(checks, struct{ value, kind, name string }{m.decision, "decision", "decision_model"})
	} else {
		m.decision = ""
	}
	for _, c := range checks {
		if c.value == "" || c.value == catalog.Recommended(c.kind) {
			continue
		}
		if !a.modelAllowed(ctx, c.kind, c.value) {
			return m, apiErr(http.StatusBadRequest, "invalid_model",
				"%s %s is not offered by this Lens. Call get_options for the list.", c.name, pyRepr(c.value))
		}
	}
	// An explicit choice is stored and passed to the subprocess even when it
	// names the recommended model: this instance's config.json may default
	// to something else, and the caller asked for this one.
	return m, nil
}

func (a *API) modelCatalog(ctx context.Context) catalog.Catalog {
	if a.catalog == nil {
		return catalog.Catalog{}
	}
	return a.catalog.Get(ctx)
}

func (a *API) modelAllowed(ctx context.Context, kind, id string) bool {
	for _, o := range a.modelCatalog(ctx).Options(kind) {
		if o.ID == id {
			return true
		}
	}
	return false
}

// Upload is a file part from a REST multipart request.
type Upload struct {
	Filename string
	Payload  []byte
}

// CreateJob is create_job: validate every option, store the input in the
// library, and queue the job. upload carries a REST multipart file; MCP and
// JSON callers send content_base64 instead.
func (a *API) CreateJob(r *http.Request, token *apitoken.Token, args map[string]any, upload *Upload) (Body, error) {
	if err := RequireScope(token, apitoken.ScopeRun); err != nil {
		return nil, err
	}
	var unknown []string
	for key := range args {
		if !slices.Contains(CreateJobArgs, key) {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		// A misspelt option (deep_rety) must not silently queue a costly run
		// with the default it meant to change.
		sort.Strings(unknown)
		return nil, invalidArg("Unknown option(s): %s. Valid: %s.", strings.Join(unknown, ", "), strings.Join(CreateJobArgs, ", "))
	}
	sources := 0
	for _, key := range []string{"items", "content_base64", "input_file"} {
		if truthy(args[key]) {
			sources++
		}
	}
	if upload != nil {
		sources++
	}
	if sources != 1 {
		return nil, invalidArg("Give exactly one input: items (a list of identifiers), content_base64 + filename " +
			"(a CSV/XLSX file), or input_file (a file already in the Lens library).")
	}
	// Validate every option before anything is written to the library.
	name := ""
	if v, ok := args["name"]; ok && v != nil {
		name = jobs.NormalizeJobName(pyStr(v))
	}
	ctv, err := optionalBool(args["ctv"], "ctv", false)
	if err != nil {
		return nil, err
	}
	researchFallback, err := optionalBool(args["research_fallback"], "research_fallback", true)
	if err != nil {
		return nil, err
	}
	deepRetry, err := optionalBool(args["deep_retry"], "deep_retry", true)
	if err != nil {
		return nil, err
	}
	// A job can only turn these passes OFF (`lens run` gets `--... off`);
	// "on" defers to config.json. Refuse an explicit true this Lens would
	// ignore instead of reporting a setting the run never used.
	for _, pass := range []struct {
		flag    string
		wanted  bool
		enabled bool
	}{
		{"research_fallback", args["research_fallback"] != nil && researchFallback, a.cfg.ResearchFallbackEnabled},
		{"deep_retry", args["deep_retry"] != nil && deepRetry, a.cfg.DeepRetryEnabled},
	} {
		if pass.wanted && !pass.enabled {
			return nil, invalidArg("%s is turned off on this Lens (config.json); omit it or pass false.", pass.flag)
		}
	}

	var spec json.RawMessage
	if raw, ok := args["custom_categories"]; ok && raw != nil {
		// An empty object is a malformed request, not "no categories":
		// running without the columns the caller asked for would waste the
		// whole run.
		var parsed *categories.Spec
		if text, isText := raw.(string); isText {
			parsed, err = categories.Parse(text)
		} else {
			parsed, err = categories.Validate(raw)
		}
		if err != nil {
			return nil, apiErr(http.StatusBadRequest, "invalid_custom_categories", "%s", err.Error())
		}
		if spec, err = json.Marshal(parsed); err != nil {
			return nil, err
		}
	}
	ctx := ctxOf(r)
	m, err := a.validatedModels(ctx, args, spec != nil)
	if err != nil {
		return nil, err
	}

	var stored string
	switch {
	case truthy(args["items"]):
		if ctv {
			return nil, invalidArg("CTV jobs need a file with app names and bundle IDs; send content_base64 instead of items.")
		}
		filename := a.inputName(name, ".csv")
		payload, err := itemsCSV(args["items"])
		if err != nil {
			return nil, err
		}
		if stored, err = a.storeInput(filename, payload); err != nil {
			return nil, err
		}
	case truthy(args["content_base64"]):
		filename, payload, err := a.decodeUpload(args["content_base64"], args["filename"])
		if err != nil {
			return nil, err
		}
		if stored, err = a.storeInput(filename, payload); err != nil {
			return nil, err
		}
	case upload != nil:
		filename, err := jobs.ValidateInputFilename(upload.Filename)
		if err != nil {
			return nil, apiErr(http.StatusBadRequest, "invalid_file", "%s", err.Error())
		}
		if stored, err = a.storeInput(filename, upload.Payload); err != nil {
			return nil, err
		}
	default:
		stored, err = jobs.ValidateInputFilename(strings.TrimSpace(pyStr(args["input_file"])))
		if err != nil {
			return nil, apiErr(http.StatusBadRequest, "invalid_file", "%s", err.Error())
		}
		if !isFile(filepath.Join(a.dirs.Inputs, stored)) {
			return nil, apiErr(http.StatusNotFound, "input_not_found",
				"No input file named %s. Use list_input_files to see the library.", pyRepr(stored))
		}
		if err := a.requireIdentifiers(stored); err != nil {
			return nil, err
		}
	}

	mode := jobs.DefaultMode
	if ctv {
		mode = "ctv"
	}
	job, err := a.manager.Create(jobs.NewJob{
		InputFile: stored, Mode: mode, Name: name, LLMModel: m.llm, ResearchFallback: researchFallback,
		DeepRetry: deepRetry, ResearchModel: m.research, CustomCategories: spec, DecisionModel: m.decision,
		SubmittedBy: token.Label,
	})
	if err != nil {
		return nil, err
	}
	a.log.Info(fmt.Sprintf("Queued job %s from API token %d (%s)", job.ID, token.ID, stored))
	view := a.jobView(job, r, true)
	view["breakdown"] = a.dirs.Breakdown(filepath.Join(a.dirs.Inputs, stored))
	return view, nil
}

// CancelJob is cancel_job: cancel a queued job, or stop a running one.
func (a *API) CancelJob(r *http.Request, token *apitoken.Token, jobID any) (Body, error) {
	if err := RequireScope(token, apitoken.ScopeRun); err != nil {
		return nil, err
	}
	job, err := a.getJobOr404(jobID)
	if err != nil {
		return nil, err
	}
	if err := a.manager.Cancel(job.ID); err != nil {
		var notAllowed *jobs.NotAllowedError
		if errors.As(err, &notAllowed) {
			return nil, apiErr(http.StatusConflict, "not_cancellable",
				"Job %s is %s; only queued or running jobs can be cancelled.", job.ID, job.Status)
		}
		if errors.Is(err, jobs.ErrUnknownJob) { // deleted in the meantime
			return nil, apiErr(http.StatusNotFound, "job_not_found", "No job %s on this Lens. Use list_jobs to find it.", job.ID)
		}
		return nil, err
	}
	if current, ok := a.manager.Get(job.ID); ok {
		job = &current
	}
	return a.jobView(job, r, true), nil
}

// DownloadLink is get_download_link: a short-lived signed URL for one file.
func (a *API) DownloadLink(r *http.Request, token *apitoken.Token, jobID, kind any) (Body, error) {
	if err := RequireScope(token, apitoken.ScopeRead); err != nil {
		return nil, err
	}
	k := strOrEmpty(kind)
	if k == "" {
		k = "output"
	}
	if !slices.Contains(FileKinds, k) {
		return nil, invalidArg("file must be one of %s", pyList(FileKinds))
	}
	job, err := a.getJobOr404(jobID)
	if err != nil {
		return nil, err
	}
	path := a.filePath(job, k)
	st, statErr := os.Stat(path)
	if path == "" || statErr != nil || !st.Mode().IsRegular() {
		yet := " yet"
		if jobs.IsTerminal(job.Status) {
			yet = ""
		}
		available := "none"
		if files := a.availableFiles(job); len(files) > 0 {
			available = pyList(files)
		}
		return nil, apiErr(http.StatusNotFound, "file_missing", "Job %s (%s) has no %s file%s. Available: %s.",
			job.ID, job.Status, k, yet, available)
	}
	ticket, expires, err := a.tokens.SignDownload(ctxOf(r), token, job.ID, k, 0)
	if err != nil {
		return nil, err
	}
	return Body{
		"job_id":     job.ID,
		"file":       k,
		"filename":   filepath.Base(path),
		"size_bytes": st.Size(),
		"url":        PublicBaseURL(r) + "/api/v1/downloads/" + ticket,
		"expires_at": time.Unix(expires, 0).UTC().Format("2006-01-02T15:04:05+00:00"),
		"next_step": "Fetch this URL into the workspace (in Fleet: download_url). It needs no " +
			"Authorization header, works for one file only and expires at expires_at.",
	}, nil
}

// ListInputFiles is list_input_files: the library, newest first.
func (a *API) ListInputFiles() (Body, error) {
	if err := os.MkdirAll(a.dirs.Inputs, 0o755); err != nil { //nolint:gosec // managed-files is operator-readable
		return nil, err
	}
	files := []Body{}
	for _, f := range a.dirs.ListInputFiles(a.now()) {
		files = append(files, Body{"name": f.Name, "size": f.Size, "uploaded": f.Uploaded, "breakdown": f.Breakdown})
	}
	return Body{"files": files}, nil
}

// GetOptions is get_options: this instance's defaults, the selectable
// models, limits, the custom-categories format and the output columns.
func (a *API) GetOptions(ctx context.Context) (Body, error) {
	cat := a.modelCatalog(ctx)
	ids := func(kind string) []Body {
		out := []Body{}
		for _, o := range cat.Options(kind) {
			out = append(out, Body{"id": o.ID, "label": o.Label})
		}
		return out
	}
	return Body{
		// What a job gets when it leaves each option out, on THIS Lens
		// (config.json may override the shipped defaults).
		"defaults": Body{
			"llm_model":                a.cfg.LLMModel,
			"research_model":           a.cfg.ResearchModel,
			"ctv_classification_model": a.cfg.CTVClassificationModel,
			"ctv_research_model":       a.cfg.CTVResearchModel,
			"decision_model":           a.cfg.DecisionModel,
			"research_fallback":        a.cfg.ResearchFallbackEnabled,
			"deep_retry":               a.cfg.DeepRetryEnabled,
		},
		"models": Body{
			"classify": ids("classify"),
			"research": ids("research"),
			"decision": ids("decision"),
		},
		"limits": Body{
			"inline_items":         MaxInlineItems,
			"upload_bytes":         a.maxUpload,
			"custom_categories":    categories.MaxCategories,
			"result_rows_per_call": MaxResultRows,
		},
		"custom_categories_format": Body{
			"categories": []Body{
				{"name": "Brand safe", "type": "boolean", "question": "Is this content brand safe? …"},
				{
					"name": "Primary purpose", "type": "choice",
					"question": "What is the main purpose of the site or app?",
					"options":  []string{"News", "Entertainment", "Shopping", "Other", "Unknown"},
				},
			},
		},
		"output_columns": Body{"standard": config.CSVFieldnames, "ctv": config.CTVCSVFieldnames},
	}, nil
}
