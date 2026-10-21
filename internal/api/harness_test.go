// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package api_test

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ElcanoTek/lens/internal/api"
	"github.com/ElcanoTek/lens/internal/apitoken"
	"github.com/ElcanoTek/lens/internal/catalog"
	"github.com/ElcanoTek/lens/internal/config"
	"github.com/ElcanoTek/lens/internal/jobs"
)

const lensHost = "lens.example.test"

func TestMain(m *testing.M) {
	jobs.ComputeBreakdown = fakeBreakdown
	os.Exit(m.Run())
}

// fakeBreakdown stands in for internal/detect: Domain/url/app_id columns
// count as websites, numeric ids as iOS, com.* as Android; a Bundle ID
// column makes a CTV list.
func fakeBreakdown(path string) (map[string]int, error) {
	out := map[string]int{"websites": 0, "ios": 0, "android": 0, "ctv": 0, "total": 0}
	if !strings.HasSuffix(strings.ToLower(path), ".csv") {
		return out, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	rows, err := r.ReadAll()
	if err != nil || len(rows) == 0 {
		return out, nil
	}
	col, ctv := -1, false
	for i, h := range rows[0] {
		switch strings.ToLower(strings.TrimSpace(h)) {
		case "domain", "url", "app_id":
			col = i
		case "bundle id":
			col, ctv = i, true
		}
	}
	if col < 0 {
		return out, nil
	}
	for _, row := range rows[1:] {
		if col >= len(row) || strings.TrimSpace(row[col]) == "" {
			continue
		}
		v := strings.TrimSpace(row[col])
		switch {
		case ctv:
			out["ctv"]++
		case strings.Trim(v, "0123456789") == "":
			out["ios"]++
		case strings.HasPrefix(v, "com."):
			out["android"]++
		default:
			out["websites"]++
		}
		out["total"]++
	}
	return out, nil
}

type harness struct {
	t       *testing.T
	home    string
	manager *jobs.Manager
	tokens  *apitoken.Store
	cfg     *config.Config
	deps    api.Deps
	api     *api.API
	handler http.Handler
}

// newHarness is a Lens with a temp home, an idle job queue (no worker, as
// the Python suite mocked worker_loop), a temp token database and an empty
// model catalog served by a local stub.
func newHarness(t *testing.T, tweak ...func(*api.Deps)) *harness {
	t.Helper()
	t.Setenv("LENS_PUBLIC_URL", "https://"+lensHost)
	home := t.TempDir()
	m := jobs.NewManager(home)
	if err := m.Dirs.Ensure(); err != nil {
		t.Fatal(err)
	}
	models := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data": []}`)
	}))
	t.Cleanup(models.Close)
	source := catalog.NewSource("")
	source.URL = models.URL
	tokens := apitoken.NewStore(filepath.Join(home, "api_tokens.db"))
	t.Cleanup(func() { _ = tokens.Close() })
	h := &harness{t: t, home: home, manager: m, tokens: tokens, cfg: config.Default()}
	h.deps = api.Deps{Manager: m, Tokens: tokens, Catalog: source, Config: h.cfg}
	for _, f := range tweak {
		f(&h.deps)
	}
	h.api = api.New(h.deps)
	h.handler = h.api.Routes()
	return h
}

func (h *harness) token(scopes ...string) string {
	h.t.Helper()
	if len(scopes) == 0 {
		scopes = apitoken.Scopes
	}
	_, raw, err := h.tokens.Create(context.Background(), "fleet-test", scopes)
	if err != nil {
		h.t.Fatal(err)
	}
	return raw
}

type response struct {
	*httptest.ResponseRecorder
}

func (r response) json(t *testing.T) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(r.Body.Bytes(), &body); err != nil {
		t.Fatalf("not JSON (%d): %s", r.Code, r.Body.String())
	}
	return body
}

func (h *harness) do(method, path, token string, body io.Reader, headers ...string) response {
	h.t.Helper()
	req := httptest.NewRequest(method, path, body)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return response{rec}
}

func (h *harness) postJSON(path, token string, payload any) response {
	h.t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		h.t.Fatal(err)
	}
	return h.do(http.MethodPost, path, token, bytes.NewReader(raw), "Content-Type", "application/json")
}

func (h *harness) jobCount() int { return len(h.manager.List()) }

// queued writes list.csv to the library and queues a job for it directly,
// as a dashboard upload would.
func (h *harness) queued() *jobs.Job {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.manager.Dirs.Inputs, "list.csv"), []byte("Domain\nexample.com\n"), 0o600); err != nil {
		h.t.Fatal(err)
	}
	job, err := h.manager.Create(jobs.NewJob{InputFile: "list.csv", Mode: "auto", Name: "t", ResearchFallback: true, DeepRetry: true, SubmittedBy: "fleet-test"})
	if err != nil {
		h.t.Fatal(err)
	}
	return job
}

var rows = [][]string{
	{"example.com", "WEBSITE", "Premium", "News"},
	{"example.org", "WEBSITE", "Standard", "News"},
	{"example.net", "WEBSITE", "Premium", "Technology"},
}

// finish pretends the pipeline ran: it writes an output CSV, progress and a
// log for id, and marks the job completed in _jobs.json (then reloads, the
// way a restart would).
func (h *harness) finish(id string, data [][]string) {
	h.t.Helper()
	out := h.manager.Dirs.Outputs
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	_ = w.Write([]string{"Domain", "Type", "Quality", "IAB Tier 1"})
	_ = w.WriteAll(data)
	processed := map[string]any{}
	for _, r := range data {
		processed[r[0]] = map[string]any{}
	}
	progress, _ := json.Marshal(map[string]any{
		"total_domains": len(data), "processed_domains": processed, "successful_count": len(data),
		"error_count": 0, "cost": map[string]any{"usd": "0.0123", "calls": 4},
	})
	for name, content := range map[string][]byte{
		id + "_output.csv": buf.Bytes(), id + "_progress.json": progress, id + ".log": []byte("started\nfinished\n"),
	} {
		if err := os.WriteFile(filepath.Join(out, name), content, 0o600); err != nil {
			h.t.Fatal(err)
		}
	}
	h.patchJob(id, map[string]any{
		"status": "completed", "output_file": id + "_output.csv", "progress_file": id + "_progress.json", "log_file": id + ".log",
	})
}

func (h *harness) patchJob(id string, fields map[string]any) {
	h.t.Helper()
	path := h.manager.Dirs.StatePath()
	raw, err := os.ReadFile(path)
	if err != nil {
		h.t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		h.t.Fatal(err)
	}
	for _, j := range doc["jobs"].([]any) {
		job := j.(map[string]any)
		if job["id"] == id {
			for k, v := range fields {
				job[k] = v
			}
		}
	}
	raw, _ = json.Marshal(doc)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		h.t.Fatal(err)
	}
	h.manager.Load()
}

func domains(t *testing.T, text string) []string {
	t.Helper()
	records, err := csv.NewReader(strings.NewReader(text)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range records[1:] {
		out = append(out, r[0])
	}
	return out
}
