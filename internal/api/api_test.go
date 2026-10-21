// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package api_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/lens/internal/api"
	"github.com/ElcanoTek/lens/internal/catalog"
)

func TestRESTRejectsMissingAndBadCredentials(t *testing.T) {
	h := newHarness(t)
	h.token() // the database exists, so "wrong" is checked against it
	resp := h.do(http.MethodGet, "/api/v1/jobs", "", nil)
	if resp.Code != http.StatusUnauthorized || !strings.HasPrefix(resp.Header().Get("WWW-Authenticate"), "Bearer") {
		t.Fatalf("got %d %v", resp.Code, resp.Header())
	}
	if resp.json(t)["code"] != "unauthorized" {
		t.Fatal(resp.Body.String())
	}
	if got := h.do(http.MethodGet, "/api/v1/jobs", "lens_wrong", nil).Code; got != http.StatusUnauthorized {
		t.Fatalf("bad token: %d", got)
	}
	// A browser session cookie is not a credential here.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil)
	req.AddCookie(&http.Cookie{Name: "lens_ui", Value: "anything"})
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("cookie: %d", rec.Code)
	}
}

func TestRESTScopeIsEnforced(t *testing.T) {
	h := newHarness(t)
	reader := h.token("jobs:read")
	resp := h.postJSON("/api/v1/jobs", reader, map[string]any{"items": []string{"example.com"}})
	if resp.Code != http.StatusForbidden || !strings.Contains(resp.json(t)["error"].(string), "jobs:run") {
		t.Fatalf("%d %s", resp.Code, resp.Body)
	}
	if got := h.do(http.MethodGet, "/api/v1/jobs", reader, nil).Code; got != http.StatusOK {
		t.Fatalf("list: %d", got)
	}
	who := h.do(http.MethodGet, "/api/v1/whoami", reader, nil).json(t)
	if !reflect.DeepEqual(who["scopes"], []any{"jobs:read"}) || who["label"] != "fleet-test" || who["token_prefix"] != reader[:12] {
		t.Fatalf("whoami = %v", who)
	}
}

func TestRESTCreateFromItemsQueuesATaggedJob(t *testing.T) {
	h := newHarness(t)
	resp := h.postJSON("/api/v1/jobs", h.token(), map[string]any{
		"name":       "Q4 review",
		"items":      []string{"example.com", "Example.com", " example.org "},
		"deep_retry": false,
	})
	if resp.Code != http.StatusCreated {
		t.Fatalf("%d %s", resp.Code, resp.Body)
	}
	body := resp.json(t)
	if body["status"] != "queued" || body["submitted_by"] != "fleet-test" {
		t.Fatalf("body = %v", body)
	}
	if body["breakdown"].(map[string]any)["total"] != 2.0 { // trimmed and de-duplicated
		t.Fatalf("breakdown = %v", body["breakdown"])
	}
	if body["dashboard_url"] != "https://"+lensHost+"/" {
		t.Fatalf("dashboard_url = %v", body["dashboard_url"])
	}
	id := body["id"].(string)
	job, _ := h.manager.Get(id)
	if job.DeepRetry || !job.ResearchFallback {
		t.Fatalf("job = %+v", job)
	}
	if !strings.HasPrefix(job.InputFile, "Q4 review ") || !strings.HasSuffix(job.InputFile, ".csv") {
		t.Fatalf("input file %q", job.InputFile)
	}
	stored, _ := os.ReadFile(filepath.Join(h.manager.Dirs.Inputs, job.InputFile))
	if string(stored) != "Domain\r\nexample.com\r\nexample.org\r\n" {
		t.Fatalf("stored CSV %q", stored)
	}
	h.manager.Load() // survives a restart
	if again, _ := h.manager.Get(id); again.SubmittedBy != "fleet-test" {
		t.Fatalf("after reload %+v", again)
	}
}

func multipartBody(t *testing.T, filename string, content []byte, fields map[string]string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if filename != "" {
		part, err := w.CreateFormFile("file", filename)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = part.Write(content)
	}
	for k, v := range fields {
		_ = w.WriteField(k, v)
	}
	_ = w.Close()
	return &buf, w.FormDataContentType()
}

func TestRESTMultipartUpload(t *testing.T) {
	h := newHarness(t)
	body, ct := multipartBody(t, "sites.csv", []byte("Domain\nexample.com\nexample.org\n"),
		map[string]string{"name": "upload", "options": `{"research_fallback": false}`})
	resp := h.do(http.MethodPost, "/api/v1/jobs", h.token(), body, "Content-Type", ct)
	if resp.Code != http.StatusCreated {
		t.Fatalf("%d %s", resp.Code, resp.Body)
	}
	job, _ := h.manager.Get(resp.json(t)["id"].(string))
	if job.InputFile != "sites.csv" || job.ResearchFallback || job.Name != "upload" {
		t.Fatalf("job = %+v", job)
	}
}

func TestRESTCreateFailsFastWithDescriptiveErrors(t *testing.T) {
	many := make([]string, api.MaxInlineItems+1)
	for i := range many {
		many[i] = "x"
	}
	cases := []struct {
		name    string
		payload map[string]any
		code    string
	}{
		{"empty", map[string]any{}, "invalid_argument"},
		{"two sources", map[string]any{"items": []string{"a.com"}, "input_file": "x.csv"}, "invalid_argument"},
		{"missing library file", map[string]any{"input_file": "missing.csv"}, "input_not_found"},
		{"bad extension", map[string]any{"content_base64": "Zm9v", "filename": "evil.exe"}, "invalid_file"},
		{"bad base64", map[string]any{"content_base64": "not base64!!", "filename": "x.csv"}, "invalid_file"},
		{"unlisted model", map[string]any{"items": []string{"a.com"}, "llm_model": "vendor/unlisted"}, "invalid_model"},
		{"garbage model", map[string]any{"items": []string{"a.com"}, "llm_model": "bad model; rm"}, "invalid_model"},
		{"no categories", map[string]any{"items": []string{"a.com"}, "custom_categories": map[string]any{"categories": []any{}}}, "invalid_custom_categories"},
		{"empty categories", map[string]any{"items": []string{"a.com"}, "custom_categories": map[string]any{}}, "invalid_custom_categories"},
		{"bad bool", map[string]any{"items": []string{"a.com"}, "deep_retry": "maybe"}, "invalid_argument"},
		{"ctv items", map[string]any{"items": []string{"a.com"}, "ctv": true}, "invalid_argument"},
		{"too many", map[string]any{"items": many}, "too_many_items"},
		{"non-string item", map[string]any{"items": []any{"a.com", 3}}, "invalid_argument"},
		{"multi-line item", map[string]any{"items": []string{"a.com\nb.com"}}, "invalid_argument"},
		{"blank items", map[string]any{"items": []string{" ", ""}}, "invalid_argument"},
		{"items not a list", map[string]any{"items": "a.com"}, "invalid_argument"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			resp := h.postJSON("/api/v1/jobs", h.token(), tc.payload)
			if resp.Code != 400 && resp.Code != 404 && resp.Code != 413 {
				t.Fatalf("status %d: %s", resp.Code, resp.Body)
			}
			body := resp.json(t)
			if body["code"] != tc.code || body["ok"] != false {
				t.Fatalf("body = %v", body)
			}
			if h.jobCount() != 0 {
				t.Fatal("a job was queued")
			}
		})
	}
}

func TestRESTRefusesAFileWithNothingToClassify(t *testing.T) {
	h := newHarness(t)
	resp := h.postJSON("/api/v1/jobs", h.token(), map[string]any{
		"content_base64": base64.StdEncoding.EncodeToString([]byte("Notes\n\n")), "filename": "empty.csv",
	})
	body := resp.json(t)
	if resp.Code != 400 || body["code"] != "no_identifiers" || h.jobCount() != 0 {
		t.Fatalf("%d %v", resp.Code, body)
	}
	if body["details"].(map[string]any)["input_file"] != "empty.csv" {
		t.Fatalf("details = %v", body["details"])
	}
}

func TestRESTJobLookupErrors(t *testing.T) {
	h := newHarness(t)
	raw := h.token()
	if got := h.do(http.MethodGet, "/api/v1/jobs/nope", raw, nil).json(t); got["code"] != "invalid_job_id" ||
		got["error"] != "'nope' is not a Lens job id (YYYYMMDDHHMMSS-xxxxxx)." {
		t.Fatalf("got %v", got)
	}
	missing := h.do(http.MethodGet, "/api/v1/jobs/20260101000000-abcdef", raw, nil)
	if missing.Code != 404 || missing.json(t)["code"] != "job_not_found" {
		t.Fatalf("%d %s", missing.Code, missing.Body)
	}
	if unknown := h.do(http.MethodGet, "/api/v1/nothing", raw, nil); unknown.Code != 404 || unknown.json(t)["ok"] != false {
		t.Fatalf("%d %s", unknown.Code, unknown.Body)
	}
}

func TestRESTResultsFilesAndCancel(t *testing.T) {
	h := newHarness(t)
	raw := h.token()
	job := h.queued()
	early := h.do(http.MethodGet, "/api/v1/jobs/"+job.ID+"/results", raw, nil)
	if early.Code != 409 || early.json(t)["code"] != "not_started" {
		t.Fatalf("%d %s", early.Code, early.Body)
	}
	detail := h.do(http.MethodGet, "/api/v1/jobs/"+job.ID, raw, nil).json(t)
	if detail["queue_position"] != 1.0 || !strings.Contains(detail["next_step"].(string), "get_job") {
		t.Fatalf("detail = %v", detail)
	}

	h.finish(job.ID, rows)
	results := h.do(http.MethodGet, "/api/v1/jobs/"+job.ID+"/results?quality=premium&columns=Domain,Quality&limit=1", raw, nil).json(t)
	summary := results["summary"].(map[string]any)
	if !reflect.DeepEqual(summary["quality"], map[string]any{"Premium": 2.0, "Standard": 1.0}) ||
		!reflect.DeepEqual(summary["top_iab_tier_1"], map[string]any{"News": 2.0, "Technology": 1.0}) {
		t.Fatalf("summary = %v", summary)
	}
	if results["matching_rows"] != 2.0 || !reflect.DeepEqual(results["rows"], []any{map[string]any{"Domain": "example.com", "Quality": "Premium"}}) {
		t.Fatalf("results = %v", results)
	}
	if !strings.Contains(results["note"].(string), "More rows remain") || results["complete"] != true {
		t.Fatalf("results = %v", results)
	}

	output := h.do(http.MethodGet, "/api/v1/jobs/"+job.ID+"/files/output", raw, nil)
	if output.Code != 200 || !reflect.DeepEqual(domains(t, output.Body.String()), []string{"example.com", "example.org", "example.net"}) {
		t.Fatalf("%d %s", output.Code, output.Body)
	}
	if ct := output.Header().Get("Content-Type"); ct != "text/csv; charset=utf-8" {
		t.Fatalf("content type %q", ct)
	}
	if got := h.do(http.MethodGet, "/api/v1/jobs/"+job.ID+"/files/output", "", nil).Code; got != 401 {
		t.Fatalf("unauthenticated download: %d", got)
	}
	if got := h.do(http.MethodGet, "/api/v1/jobs/"+job.ID+"/files/categories", raw, nil).Code; got != 404 {
		t.Fatalf("categories: %d", got)
	}
	if got := h.do(http.MethodGet, "/api/v1/jobs/"+job.ID+"/files/bogus", raw, nil); got.Code != 404 || got.json(t)["code"] != "file_missing" {
		t.Fatalf("bogus kind: %d", got.Code)
	}
	cancel := h.do(http.MethodPost, "/api/v1/jobs/"+job.ID+"/cancel", raw, nil)
	if cancel.Code != 409 || cancel.json(t)["code"] != "not_cancellable" {
		t.Fatalf("%d %s", cancel.Code, cancel.Body)
	}
}

func TestCancelAQueuedJob(t *testing.T) {
	h := newHarness(t)
	job := h.queued()
	resp := h.do(http.MethodPost, "/api/v1/jobs/"+job.ID+"/cancel", h.token(), nil)
	if resp.Code != 200 || resp.json(t)["status"] != "cancelled" {
		t.Fatalf("%d %s", resp.Code, resp.Body)
	}
	if got := h.do(http.MethodPost, "/api/v1/jobs/"+job.ID+"/cancel", h.token("jobs:read"), nil).Code; got != 403 {
		t.Fatalf("reader cancel: %d", got)
	}
}

func TestSignedDownloadLinkIsBearerFreeScopedAndExpires(t *testing.T) {
	h := newHarness(t)
	raw := h.token()
	job := h.queued()
	missing := h.do(http.MethodPost, "/api/v1/jobs/"+job.ID+"/files/output/link", raw, nil)
	if missing.Code != 404 || missing.json(t)["code"] != "file_missing" ||
		!strings.Contains(missing.json(t)["error"].(string), "no output file yet. Available: ['input'].") {
		t.Fatalf("%d %s", missing.Code, missing.Body)
	}
	h.finish(job.ID, rows)
	link := h.do(http.MethodPost, "/api/v1/jobs/"+job.ID+"/files/output/link", raw, nil).json(t)
	url := link["url"].(string)
	if !strings.HasPrefix(url, "https://"+lensHost+"/api/v1/downloads/") || link["filename"] != job.ID+"_output.csv" {
		t.Fatalf("link = %v", link)
	}
	if _, err := time.Parse("2006-01-02T15:04:05-07:00", link["expires_at"].(string)); err != nil {
		t.Fatalf("expires_at %v", link["expires_at"])
	}
	path := strings.SplitN(url, lensHost, 2)[1]
	fetched := h.do(http.MethodGet, path, "", nil)
	if fetched.Code != 200 || !reflect.DeepEqual(domains(t, fetched.Body.String()), []string{"example.com", "example.org", "example.net"}) {
		t.Fatalf("%d %s", fetched.Code, fetched.Body)
	}
	if cd := fetched.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Fatalf("content-disposition %q", cd)
	}

	ticket := path[strings.LastIndex(path, "/")+1:]
	payload, signature, _ := strings.Cut(ticket, ".")
	decoded, _ := base64.RawURLEncoding.DecodeString(payload)
	var claims map[string]any
	_ = json.Unmarshal(decoded, &claims)
	claims["k"] = "log"
	tampered, _ := json.Marshal(claims)
	forged := base64.RawURLEncoding.EncodeToString(tampered) + "." + signature
	if got := h.do(http.MethodGet, "/api/v1/downloads/"+forged, "", nil); got.Code != 403 || got.json(t)["code"] != "invalid_link" {
		t.Fatalf("forged: %d", got.Code)
	}

	h.tokens.SetClock(func() time.Time { return time.Now().Add(time.Hour) })
	expired := h.do(http.MethodGet, path, "", nil)
	if expired.Code != 403 || !strings.Contains(expired.json(t)["error"].(string), "expired") {
		t.Fatalf("%d %s", expired.Code, expired.Body)
	}
	h.tokens.SetClock(time.Now)

	token, err := h.tokens.Verify(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.tokens.Revoke(context.Background(), token.ID); err != nil {
		t.Fatal(err)
	}
	if got := h.do(http.MethodGet, path, "", nil).Code; got != 403 {
		t.Fatalf("revoked: %d", got)
	}
}

func TestRESTQueryErrorsUseTheAPIErrorShape(t *testing.T) {
	h := newHarness(t)
	raw := h.token()
	// Auth comes first, then the API's own validation.
	if got := h.do(http.MethodGet, "/api/v1/jobs?limit=abc", "", nil).Code; got != 401 {
		t.Fatalf("unauthenticated: %d", got)
	}
	resp := h.do(http.MethodGet, "/api/v1/jobs?limit=abc", raw, nil)
	if resp.Code != 400 || resp.json(t)["code"] != "invalid_argument" {
		t.Fatalf("%d %s", resp.Code, resp.Body)
	}
	if got := h.do(http.MethodGet, "/api/v1/jobs?status=", raw, nil); got.Code != 400 || got.json(t)["code"] != "invalid_status" {
		t.Fatalf("empty status: %d %s", got.Code, got.Body)
	}
	job := h.queued()
	h.finish(job.ID, rows)
	bad := h.do(http.MethodGet, "/api/v1/jobs/"+job.ID+"/results?offset=-1", raw, nil)
	if bad.Code != 400 || bad.json(t)["code"] != "invalid_argument" {
		t.Fatalf("%d %s", bad.Code, bad.Body)
	}
	cols := h.do(http.MethodGet, "/api/v1/jobs/"+job.ID+"/results?columns=Domain,Nope", raw, nil).json(t)
	if cols["error"] != "Unknown column(s) ['Nope']; available: ['Domain', 'Type', 'Quality', 'IAB Tier 1']" {
		t.Fatalf("columns error %v", cols["error"])
	}
}

func TestListJobsFiltersAndLimits(t *testing.T) {
	h := newHarness(t)
	raw := h.token()
	first := h.queued()
	time.Sleep(2 * time.Millisecond) // distinct created_at
	second := h.queued()
	h.finish(first.ID, rows)
	queued := h.do(http.MethodGet, "/api/v1/jobs?status=queued", raw, nil).json(t)
	if queued["total_matching"] != 1.0 || queued["jobs"].([]any)[0].(map[string]any)["id"] != second.ID {
		t.Fatalf("queued = %v", queued)
	}
	limited := h.do(http.MethodGet, "/api/v1/jobs?limit=1", raw, nil).json(t)
	if limited["total_matching"] != 2.0 || len(limited["jobs"].([]any)) != 1 {
		t.Fatalf("limited = %v", limited)
	}
	completed := h.do(http.MethodGet, "/api/v1/jobs/"+first.ID, raw, nil).json(t)
	if completed["progress"].(map[string]any)["percent"] != 100.0 || completed["cost_usd"] != 0.0123 {
		t.Fatalf("completed = %v", completed)
	}
	if !reflect.DeepEqual(completed["files"], []any{"output", "log", "input"}) || completed["eta_seconds"] != nil {
		t.Fatalf("completed = %v", completed)
	}
}

func TestLibraryFileWithoutIdentifiersIsRefusedBeforeQueueing(t *testing.T) {
	h := newHarness(t)
	if err := os.WriteFile(filepath.Join(h.manager.Dirs.Inputs, "notes.csv"), []byte("Notes\nhello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resp := h.postJSON("/api/v1/jobs", h.token(), map[string]any{"input_file": "notes.csv"})
	if resp.Code != 400 || resp.json(t)["code"] != "no_identifiers" || h.jobCount() != 0 {
		t.Fatalf("%d %s", resp.Code, resp.Body)
	}
}

func TestLibraryFileJob(t *testing.T) {
	h := newHarness(t)
	if err := os.WriteFile(filepath.Join(h.manager.Dirs.Inputs, "lib.csv"), []byte("Domain\na.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resp := h.postJSON("/api/v1/jobs", h.token(), map[string]any{"input_file": " lib.csv "})
	if resp.Code != 201 || resp.json(t)["input_file"] != "lib.csv" {
		t.Fatalf("%d %s", resp.Code, resp.Body)
	}
}

func TestOversizedMultipartUploadIsRefused(t *testing.T) {
	h := newHarness(t, func(d *api.Deps) { d.MaxUploadBytes = 64 })
	body, ct := multipartBody(t, "big.csv", []byte("Domain\n"+strings.Repeat("example.com\n", 20)), nil)
	resp := h.do(http.MethodPost, "/api/v1/jobs", h.token(), body, "Content-Type", ct)
	if resp.Code != 413 || resp.json(t)["code"] != "file_too_large" {
		t.Fatalf("%d %s", resp.Code, resp.Body)
	}
	if _, err := os.Stat(filepath.Join(h.manager.Dirs.Inputs, "big.csv")); err == nil {
		t.Fatal("oversized upload was stored")
	}
}

func chunked(h *harness, path, token, contentType string, body []byte) response {
	h.t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, io.MultiReader(bytes.NewReader(body)))
	req.ContentLength = -1
	req.TransferEncoding = []string{"chunked"}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return response{rec}
}

func TestChunkedBodiesAreCappedWhileStreaming(t *testing.T) {
	h := newHarness(t, func(d *api.Deps) { d.MaxUploadBytes = 64 })
	big, _ := json.Marshal(map[string]any{"items": []string{"a.com"}, "pad": strings.Repeat("x", 2_000_000)})
	resp := chunked(h, "/api/v1/jobs", h.token(), "application/json", big)
	if resp.Code != 413 || resp.json(t)["code"] != "too_large" {
		t.Fatalf("%d %s", resp.Code, resp.Body)
	}
	// Declared too large: refused before reading.
	declared := h.do(http.MethodPost, "/api/v1/jobs", h.token(), bytes.NewReader(big), "Content-Type", "application/json")
	if declared.Code != 413 || declared.json(t)["code"] != "file_too_large" {
		t.Fatalf("%d %s", declared.Code, declared.Body)
	}
}

func TestChunkedMultipartIsCappedWhileParsing(t *testing.T) {
	h := newHarness(t, func(d *api.Deps) { d.MaxUploadBytes = 64 })
	boundary := "lensboundary"
	body := []byte("--" + boundary + "\r\nContent-Disposition: form-data; name=\"file\"; filename=\"big.csv\"\r\n" +
		"Content-Type: text/csv\r\n\r\nDomain\n" + strings.Repeat("example.com\n", 200_000) + "\r\n--" + boundary + "--\r\n")
	resp := chunked(h, "/api/v1/jobs", h.token(), "multipart/form-data; boundary="+boundary, body)
	if resp.Code != 413 || resp.json(t)["code"] != "too_large" || h.jobCount() != 0 {
		t.Fatalf("%d %s", resp.Code, resp.Body)
	}
}

func TestSettingsReportWhatTheRunUsesOnThisInstance(t *testing.T) {
	h := newHarness(t)
	h.cfg.LLMModel = "vendor/configured-classifier"
	h.cfg.DeepRetryEnabled = false
	raw := h.token()
	created := h.postJSON("/api/v1/jobs", raw, map[string]any{"items": []string{"a.com"}}).json(t)
	settings := created["settings"].(map[string]any)
	if settings["llm_model"] != "vendor/configured-classifier" || settings["deep_retry"] != false ||
		settings["research_model"] != h.cfg.ResearchModel || settings["content"] != "websites/apps" || settings["decision_model"] != nil {
		t.Fatalf("settings = %v", settings)
	}
	// An explicit request for the recommended model is kept and passed on.
	explicit := h.postJSON("/api/v1/jobs", raw, map[string]any{"items": []string{"b.com"}, "llm_model": catalog.RecommendedModel}).json(t)
	if job, _ := h.manager.Get(explicit["id"].(string)); job.LLMModel != catalog.RecommendedModel {
		t.Fatalf("job = %+v", job)
	}
	if explicit["settings"].(map[string]any)["llm_model"] != catalog.RecommendedModel {
		t.Fatalf("settings = %v", explicit["settings"])
	}
	// An explicit true this instance would ignore is refused, not misreported.
	refused := h.postJSON("/api/v1/jobs", raw, map[string]any{"items": []string{"c.com"}, "deep_retry": true})
	if refused.Code != 400 || !strings.Contains(refused.json(t)["error"].(string), "deep_retry") {
		t.Fatalf("%d %s", refused.Code, refused.Body)
	}
	options := h.do(http.MethodGet, "/api/v1/options", raw, nil).json(t)
	defaults := options["defaults"].(map[string]any)
	if defaults["llm_model"] != "vendor/configured-classifier" || defaults["deep_retry"] != false {
		t.Fatalf("defaults = %v", defaults)
	}
}

func TestCTVJobSettingsUseTheCTVModels(t *testing.T) {
	h := newHarness(t)
	h.cfg.CTVClassificationModel = "vendor/ctv-classifier"
	content := base64.StdEncoding.EncodeToString([]byte("App Name,Bundle ID\nNews,com.news\n"))
	created := h.postJSON("/api/v1/jobs", h.token(), map[string]any{"content_base64": content, "filename": "ctv.csv"}).json(t)
	settings := created["settings"].(map[string]any)
	if settings["content"] != "ctv" || settings["llm_model"] != "vendor/ctv-classifier" || created["breakdown"].(map[string]any)["ctv"] != 1.0 {
		t.Fatalf("created = %v", created)
	}
}

func TestListedModelsAreAccepted(t *testing.T) {
	models := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data": [{"id": "vendor/listed", "name": "Listed", "supported_parameters": ["tools"],
			"pricing": {"prompt": "0.000001", "completion": "0.000002"}}]}`)
	}))
	defer models.Close()
	h := newHarness(t, func(d *api.Deps) {
		d.Catalog = catalog.NewSource("")
		d.Catalog.URL = models.URL
	})
	raw := h.token()
	resp := h.postJSON("/api/v1/jobs", raw, map[string]any{"items": []string{"a.com"}, "llm_model": "vendor/listed"})
	if resp.Code != 201 {
		t.Fatalf("%d %s", resp.Code, resp.Body)
	}
	// Listed as a classifier, not as a research model.
	if got := h.postJSON("/api/v1/jobs", raw, map[string]any{"items": []string{"a.com"}, "research_model": "vendor/listed"}).json(t); got["code"] != "invalid_model" {
		t.Fatalf("got %v", got)
	}
	classify := h.do(http.MethodGet, "/api/v1/options", raw, nil).json(t)["models"].(map[string]any)["classify"].([]any)
	if len(classify) != 1 || classify[0].(map[string]any)["id"] != "vendor/listed" {
		t.Fatalf("classify = %v", classify)
	}
}

func TestPagingWalksTheRows(t *testing.T) {
	h := newHarness(t)
	raw := h.token()
	job := h.queued()
	h.finish(job.ID, rows)
	var got []string
	for i := range 3 {
		page := h.do(http.MethodGet, "/api/v1/jobs/"+job.ID+"/results?offset="+string(rune('0'+i))+"&limit=1", raw, nil).json(t)
		got = append(got, page["rows"].([]any)[0].(map[string]any)["Domain"].(string))
		if !reflect.DeepEqual(page["summary"].(map[string]any)["quality"], map[string]any{"Premium": 2.0, "Standard": 1.0}) {
			t.Fatalf("page = %v", page)
		}
	}
	if !reflect.DeepEqual(got, []string{"example.com", "example.org", "example.net"}) {
		t.Fatalf("got %v", got)
	}
	premium := h.do(http.MethodGet, "/api/v1/jobs/"+job.ID+"/results?quality=Premium&limit=0", raw, nil).json(t)
	if premium["matching_rows"] != 2.0 || len(premium["rows"].([]any)) != 0 {
		t.Fatalf("premium = %v", premium)
	}
}

func TestResultsWhileRunningAndMissingOutput(t *testing.T) {
	h := newHarness(t)
	raw := h.token()
	exe := filepath.Join(h.home, "fake-lens")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nexec sleep 30\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	h.manager.Executable = exe
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.manager.Run(ctx); close(done) }()
	defer func() { stop(); <-done }()
	job := h.queued()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if j, _ := h.manager.Get(job.ID); j.Status == "running" && j.LogFile != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("job never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := h.do(http.MethodGet, "/api/v1/jobs/"+job.ID+"/results", raw, nil).json(t); got["code"] != "no_results_yet" {
		t.Fatalf("got %v", got)
	}
	out := h.manager.Dirs.Outputs
	_ = os.WriteFile(filepath.Join(out, job.ID+"_output.csv"), []byte("Domain,Quality\nexample.com,Premium\n"), 0o600)
	_ = os.WriteFile(filepath.Join(out, job.ID+".log"), []byte("started\nworking\n"), 0o600)
	_ = os.WriteFile(filepath.Join(out, job.ID+"_progress.json"), []byte(`{"total_domains": 4, "processed_domains": {"example.com": {}}}`), 0o600)
	running := h.do(http.MethodGet, "/api/v1/jobs/"+job.ID+"/results", raw, nil).json(t)
	if running["complete"] != false || !strings.Contains(running["note"].(string), "rows written so far") {
		t.Fatalf("running = %v", running)
	}
	detail := h.do(http.MethodGet, "/api/v1/jobs/"+job.ID, raw, nil).json(t)
	if !reflect.DeepEqual(detail["log_tail"], []any{"started", "working"}) || detail["progress"].(map[string]any)["percent"] != 25.0 {
		t.Fatalf("detail = %v", detail)
	}
	if _, ok := detail["eta_seconds"]; !ok {
		t.Fatalf("detail = %v", detail)
	}
	cancelled := h.do(http.MethodPost, "/api/v1/jobs/"+job.ID+"/cancel", raw, nil).json(t)
	if cancelled["status"] != "cancelling" {
		t.Fatalf("cancel = %v", cancelled)
	}
	for {
		if j, _ := h.manager.Get(job.ID); j.Status == "cancelled" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("job never stopped")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := os.Remove(filepath.Join(out, job.ID+"_output.csv")); err != nil {
		t.Fatal(err)
	}
	missing := h.do(http.MethodGet, "/api/v1/jobs/"+job.ID+"/results", raw, nil)
	if missing.Code != 404 || missing.json(t)["code"] != "output_missing" {
		t.Fatalf("%d %s", missing.Code, missing.Body)
	}
}

func TestOmittedDecisionModelInheritsThisInstancesDefault(t *testing.T) {
	h := newHarness(t)
	h.cfg.DecisionModel = "vendor/configured-decisions"
	spec := map[string]any{"categories": []any{map[string]any{"name": "Kids", "type": "boolean", "question": "For kids?"}}}
	created := h.postJSON("/api/v1/jobs", h.token(), map[string]any{"items": []string{"a.com"}, "custom_categories": spec}).json(t)
	job, _ := h.manager.Get(created["id"].(string))
	if job.DecisionModel != "" || !job.HasCategories() {
		t.Fatalf("job = %+v", job)
	}
	settings := created["settings"].(map[string]any)
	if settings["decision_model"] != "vendor/configured-decisions" || !reflect.DeepEqual(settings["custom_categories"], []any{"Kids"}) {
		t.Fatalf("settings = %v", settings)
	}
	// Categories as a JSON string are accepted too.
	text, _ := json.Marshal(spec)
	if resp := h.postJSON("/api/v1/jobs", h.token(), map[string]any{"items": []string{"b.com"}, "custom_categories": string(text)}); resp.Code != 201 {
		t.Fatalf("%d %s", resp.Code, resp.Body)
	}
}

func TestUnknownCreateOptionsAreRefused(t *testing.T) {
	h := newHarness(t)
	resp := h.postJSON("/api/v1/jobs", h.token(), map[string]any{"items": []string{"a.com"}, "deep_rety": false})
	if resp.Code != 400 || !strings.Contains(resp.json(t)["error"].(string), "deep_rety") || h.jobCount() != 0 {
		t.Fatalf("%d %s", resp.Code, resp.Body)
	}
	body, ct := multipartBody(t, "a.csv", []byte("Domain\na.com\n"), map[string]string{"options": `{"reseach_fallback": false}`})
	form := h.do(http.MethodPost, "/api/v1/jobs", h.token(), body, "Content-Type", ct)
	if form.Code != 400 || !strings.Contains(form.json(t)["error"].(string), "reseach_fallback") {
		t.Fatalf("%d %s", form.Code, form.Body)
	}
	body, ct = multipartBody(t, "a.csv", []byte("Domain\na.com\n"), map[string]string{"options": `[1]`})
	if got := h.do(http.MethodPost, "/api/v1/jobs", h.token(), body, "Content-Type", ct).json(t); got["error"] != "options must be a JSON object" {
		t.Fatalf("got %v", got)
	}
}

func TestOverlongUploadNamesAreInvalidFiles(t *testing.T) {
	h := newHarness(t)
	resp := h.postJSON("/api/v1/jobs", h.token(), map[string]any{
		"content_base64": base64.StdEncoding.EncodeToString([]byte("Domain\na.com\n")),
		"filename":       strings.Repeat("x", 300) + ".csv",
	})
	if resp.Code != 400 || resp.json(t)["code"] != "invalid_file" {
		t.Fatalf("%d %s", resp.Code, resp.Body)
	}
}

func TestMalformedBodiesGetTheAPIErrorShape(t *testing.T) {
	h := newHarness(t)
	for _, tc := range []struct{ body, contentType string }{
		{"--x\r\nnot a real part", "multipart/form-data; boundary=x"},
		{"{not json", "application/json"},
		{"[1, 2]", "application/json"},
	} {
		resp := h.do(http.MethodPost, "/api/v1/jobs", h.token(), strings.NewReader(tc.body), "Content-Type", tc.contentType)
		body := resp.json(t)
		if (resp.Code != 400 && resp.Code != 413) || body["ok"] != false || body["code"] == nil {
			t.Fatalf("%q: %d %v", tc.body, resp.Code, body)
		}
	}
}

func TestPublicBaseURL(t *testing.T) {
	t.Setenv("LENS_PUBLIC_URL", "")
	for host, want := range map[string]string{
		"[::1]:8808":       "http://[::1]:8808",
		"127.0.0.1:8808":   "http://127.0.0.1:8808",
		"localhost":        "http://localhost",
		"lens.example.com": "https://lens.example.com",
	} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = host
		if got := api.PublicBaseURL(req); got != want {
			t.Errorf("%s: got %s, want %s", host, got, want)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-Host", "lens.example.com, proxy")
	req.Header.Set("X-Forwarded-Proto", "http")
	if got := api.PublicBaseURL(req); got != "http://lens.example.com" {
		t.Errorf("forwarded: %s", got)
	}
	t.Setenv("LENS_PUBLIC_URL", " https://lens.example.org/ ")
	if got := api.PublicBaseURL(nil); got != "https://lens.example.org" {
		t.Errorf("configured: %s", got)
	}
}

func TestOptionsReportTheConfiguredDecisionModel(t *testing.T) {
	h := newHarness(t)
	h.cfg.DecisionModel = "vendor/configured-decisions"
	options := h.do(http.MethodGet, "/api/v1/options", h.token(), nil).json(t)
	if options["defaults"].(map[string]any)["decision_model"] != "vendor/configured-decisions" {
		t.Fatalf("options = %v", options)
	}
	limits := options["limits"].(map[string]any)
	if limits["inline_items"] != float64(api.MaxInlineItems) || limits["custom_categories"] != 12.0 {
		t.Fatalf("limits = %v", limits)
	}
}

func TestListInputs(t *testing.T) {
	h := newHarness(t)
	if err := os.WriteFile(filepath.Join(h.manager.Dirs.Inputs, "chat list.csv"), []byte("Domain\na.com\nb.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	files := h.do(http.MethodGet, "/api/v1/inputs", h.token(), nil).json(t)["files"].([]any)
	if len(files) != 1 {
		t.Fatalf("files = %v", files)
	}
	f := files[0].(map[string]any)
	if f["name"] != "chat list.csv" || f["size"] != "19 B" || f["breakdown"].(map[string]any)["websites"] != 2.0 {
		t.Fatalf("file = %v", f)
	}
}
