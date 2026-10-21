// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package web

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/ElcanoTek/lens/internal/catalog"
	"github.com/ElcanoTek/lens/internal/jobs"
)

// Ported from tests/test_web_service.py (the dashboard routes).

const spec = `{"categories": [{"name": "Educational", "type": "boolean", "question": "Does this teach useful skills?"}]}`

func TestCustomCategoriesFormValidationAndKeyStaysServerSide(t *testing.T) {
	t.Setenv("TYPESAFE_API_KEY", "private-typesafe-key")
	h := newHarness(t, false)
	h.writeInput("list.csv", "Domain\nexample.com\n")
	_, page := h.get("/")
	if !strings.Contains(page, `id="custom-categories-enabled"`) || strings.Contains(page, "private-typesafe-key") {
		t.Fatal("editor missing or key leaked")
	}
	resp := h.postForm("/jobs", url.Values{"input_file": {"list.csv"}, "custom_categories": {spec}})
	if !strings.Contains(location(resp), "message=") {
		t.Fatalf("location %q", location(resp))
	}
	list := h.jobs()
	if len(list) != 1 {
		t.Fatalf("jobs = %d", len(list))
	}
	job := list[0]
	var got map[string]any
	_ = json.Unmarshal(job.CustomCategories, &got)
	var want map[string]any
	_ = json.Unmarshal([]byte(spec), &want)
	if !jsonEqual(got, want) {
		t.Fatalf("categories = %s", job.CustomCategories)
	}
	if !strings.Contains(settingsSummary(&job), "Educational") {
		t.Fatalf("summary %q", settingsSummary(&job))
	}
	saved, _ := os.ReadFile(filepath.Join(h.dirs.Outputs, jobs.JobsStateFilename))
	if strings.Contains(string(saved), "private-typesafe-key") {
		t.Fatal("key persisted")
	}
	// The recommended decision model is stored explicitly so the run
	// matches the UI.
	if job.DecisionModel != "~typesafe/jev-latest" {
		t.Fatalf("decision model %q", job.DecisionModel)
	}
	h.srv.Manager().Load()
	if j, _ := h.srv.Manager().Get(job.ID); !bytes.Equal(j.CustomCategories, job.CustomCategories) {
		t.Fatalf("reloaded categories %s", j.CustomCategories)
	}
	for _, invalid := range []string{"not JSON", `{"categories": []}`} {
		resp := h.postForm("/jobs", url.Values{"input_file": {"list.csv"}, "custom_categories": {invalid}})
		if !strings.Contains(location(resp), "error=") {
			t.Fatalf("%q accepted: %s", invalid, location(resp))
		}
	}
	resp = h.postForm("/jobs", url.Values{"input_file": {"list.csv"}, "custom_categories": {spec}, "decision_model": {"openai/not-a-listed-decision-model"}})
	if !strings.Contains(location(resp), "Unknown+decision+model") || len(h.jobs()) != 1 {
		t.Fatalf("unknown decision model: %s", location(resp))
	}
	_, page = h.get("/")
	if !strings.Contains(page, `name="decision_model"`) {
		t.Fatal("decision model select missing")
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

func TestDecisionModelIsValidatedPersistedAndPassed(t *testing.T) {
	h := newHarness(t, false)
	h.catalog.cat = catalog.Catalog{Decision: []catalog.Option{{ID: "openai/gpt-6-luna-decisions", Label: "OpenAI decisions"}}}
	h.writeInput("list.csv", "Domain\nexample.com\n")
	if _, page := h.get("/"); !strings.Contains(page, `value="openai/gpt-6-luna-decisions"`) {
		t.Fatal("decision option not rendered")
	}
	// Without categories the model is irrelevant and is not stored.
	h.postForm("/jobs", url.Values{"input_file": {"list.csv"}, "decision_model": {"openai/gpt-6-luna-decisions"}})
	resp := h.postForm("/jobs", url.Values{"input_file": {"list.csv"}, "custom_categories": {spec}, "decision_model": {"openai/gpt-6-luna-decisions"}})
	if !strings.Contains(location(resp), "message=") {
		t.Fatal(location(resp))
	}
	var plain, custom jobs.Job
	for _, j := range h.jobs() {
		if j.HasCategories() {
			custom = j
		} else {
			plain = j
		}
	}
	if plain.DecisionModel != "" || custom.DecisionModel != "openai/gpt-6-luna-decisions" {
		t.Fatalf("plain %q custom %q", plain.DecisionModel, custom.DecisionModel)
	}
	if !strings.Contains(settingsSummary(&custom), "decision: openai/gpt-6-luna-decisions") {
		t.Fatal(settingsSummary(&custom))
	}
	argv := h.srv.Manager().Command(&custom)
	if i := slices.Index(argv, "--decision-model"); i < 0 || argv[i+1] != "openai/gpt-6-luna-decisions" {
		t.Fatalf("argv %v", argv)
	}
	if slices.Contains(h.srv.Manager().Command(&plain), "--decision-model") {
		t.Fatal("plain job passes a decision model")
	}
	h.srv.Manager().Load()
	if j, _ := h.srv.Manager().Get(custom.ID); j.DecisionModel != "openai/gpt-6-luna-decisions" {
		t.Fatal("not persisted")
	}
}

func writeState(t *testing.T, home string, state any) {
	t.Helper()
	dirs := jobs.DirsFor(home)
	if err := dirs.Ensure(); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(state)
	if err := os.WriteFile(dirs.StatePath(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPersistedJobsLoadAndValidate(t *testing.T) {
	home := t.TempDir()
	cats := json.RawMessage(`{"categories": [{"name": "Educational", "type": "boolean", "question": "Teach?"}]}`)
	writeState(t, home, map[string]any{"jobs": []any{
		// Saved before decision models existed: the subprocess falls back
		// to config.DECISION_MODEL.
		map[string]any{"id": "older-run", "input_file": "list.csv", "mode": "auto", "status": "queued", "custom_categories": cats},
		// Corrupt definitions cannot be silently queued.
		map[string]any{"id": "bad-labels", "input_file": "list.csv", "mode": "auto", "status": "queued", "custom_categories": map[string]any{"categories": []any{}}},
		// Traversal in input_file drops the job; in aux names nulls them.
		map[string]any{"id": "evil-1", "input_file": "../../etc/passwd", "mode": "direct", "status": "queued", "created_at": "2026-01-01T00:00:00+00:00"},
		map[string]any{
			"id": "good-1", "input_file": "list.csv", "mode": "direct", "status": "completed", "created_at": "2026-01-01T00:00:01+00:00",
			"output_file": "good-1_output.csv", "progress_file": "../../../etc/shadow", "log_file": "good-1.log",
		},
	}})
	h := newHarness(t, false, func(o *Options) {
		o.Home = home
		m := jobs.NewManager(home)
		o.Manager = m
	})
	m := h.srv.Manager()
	if j, _ := m.Get("older-run"); j.Status != jobs.StatusQueued || j.DecisionModel != "" {
		t.Fatalf("older-run = %+v", j)
	}
	if j, _ := m.Get("bad-labels"); j.Status != jobs.StatusFailed {
		t.Fatalf("bad-labels = %+v", j)
	}
	if m.QueueLength() != 1 || m.QueuePosition("older-run") != 1 {
		t.Fatal("queue")
	}
	if _, ok := m.Get("evil-1"); ok {
		t.Fatal("traversal job loaded")
	}
	if j, _ := m.Get("good-1"); j.OutputFile != "good-1_output.csv" || j.ProgressFile != "" || j.LogFile != "good-1.log" {
		t.Fatalf("good-1 = %+v", j)
	}
}

func TestCategoriesSidecarHandoffAndDeletion(t *testing.T) {
	h := newHarness(t, true)
	h.writeInput("list.csv", "Domain\nexample.com\n")
	h.postForm("/jobs", url.Values{"input_file": {"list.csv"}, "custom_categories": {spec}})
	job := h.jobs()[0]
	waitStatus(t, h, job.ID, jobs.StatusCompleted)
	sidecar := filepath.Join(h.dirs.Outputs, job.ID+"_output.csv.categories.json")
	raw, err := os.ReadFile(sidecar) //nolint:gosec // test file
	if err != nil {
		t.Fatal(err)
	}
	// Written exactly as Python's json.dumps(spec, indent=2).
	want := "{\n  \"categories\": [\n    {\n      \"name\": \"Educational\",\n      \"type\": \"boolean\",\n      \"question\": \"Does this teach useful skills?\"\n    }\n  ]\n}"
	if string(raw) != want {
		t.Fatalf("sidecar = %q", raw)
	}
	if resp := h.postForm("/jobs/"+job.ID+"/delete", nil); location(resp) != "/?message=Job+deleted" {
		t.Fatal(location(resp))
	}
	if _, err := os.Stat(sidecar); err == nil {
		t.Fatal("sidecar survived deletion")
	}
}

func TestCancelQueuedJob(t *testing.T) {
	h := newHarness(t, false)
	h.writeInput("sample.csv", "pageURL\nexample.com\n")
	h.postForm("/jobs", url.Values{"input_file": {"sample.csv"}, "mode": {"direct"}})
	job := h.jobs()[0]
	if resp := h.postForm("/jobs/"+job.ID+"/cancel", nil); location(resp) != "/?message=Cancellation+requested" {
		t.Fatal(location(resp))
	}
	if j, _ := h.srv.Manager().Get(job.ID); j.Status != jobs.StatusCancelled {
		t.Fatal(j.Status)
	}
	if resp := h.postForm("/jobs/"+job.ID+"/cancel", nil); !strings.HasPrefix(location(resp), "/?error=Job+") {
		t.Fatal(location(resp))
	}
	if resp := h.postForm("/jobs/nope/cancel", nil); location(resp) != "/?error=Unknown+job" {
		t.Fatal(location(resp))
	}
	if resp := h.postForm("/jobs/nope/delete", nil); location(resp) != "/?error=Unknown+job" {
		t.Fatal(location(resp))
	}
}

func TestLiveEndpointReturnsProgressAndLog(t *testing.T) {
	h := newHarness(t, true, func(o *Options) {
		m := jobs.NewManager(o.Home)
		m.Executable = fakeExecutable(t, o.Home, `for a in "$@"; do case "$prev" in --progress-file) p="$a";; --log-file) l="$a";; esac; prev="$a"; done
printf '{"summary": {"processed": 3, "total_domains": 10}}' > "$p"; printf 'line1\nline2\n' > "$l"; trap 'exit 0' INT; sleep 30 & wait`)
		m.BaseArgs = nil
		o.Manager = m
	})
	h.writeInput("sample.csv", "pageURL\nexample.com\n")
	h.postForm("/jobs", url.Values{"input_file": {"sample.csv"}, "mode": {"direct"}})
	job := h.jobs()[0]
	waitStatus(t, h, job.ID, jobs.StatusRunning)
	t.Cleanup(func() { _ = h.srv.Manager().Cancel(job.ID) })
	var payload struct {
		Job      map[string]any `json:"job"`
		Progress map[string]any `json:"progress"`
		LogLines []string       `json:"log_lines"`
		Message  string         `json:"message"`
	}
	for range 200 {
		_, body := h.get("/api/jobs/live")
		_ = json.Unmarshal([]byte(body), &payload)
		if len(payload.LogLines) == 2 && payload.Progress["processed"] != nil {
			break
		}
		waitShort()
	}
	if payload.Job["id"] != job.ID || payload.Progress["processed"] != 3.0 || payload.LogLines[len(payload.LogLines)-1] != "line2" {
		t.Fatalf("payload = %+v", payload)
	}
	if payload.Job["name"] != nil || payload.Job["finished_at"] != nil {
		t.Fatalf("nulls: %+v", payload.Job)
	}
	_, body := h.get("/api/jobs/queue")
	if !strings.Contains(body, `"id":"`+job.ID+`"`) || !strings.Contains(body, `"message":"ok"`) {
		t.Fatal(body)
	}
	resp, body := h.get("/api/jobs/live?job_id=missing")
	if resp.StatusCode != 404 || body != `{"detail":"Unknown job"}` {
		t.Fatal(resp.StatusCode, body)
	}
}

func TestLiveWithoutActiveJob(t *testing.T) {
	h := newHarness(t, false)
	_, body := h.get("/api/jobs/live")
	if body != `{"job":null,"progress":{},"log_lines":[],"message":"No active job right now"}` {
		t.Fatal(body)
	}
}

func TestBatchDeleteAndDownload(t *testing.T) {
	h := newHarness(t, false)
	h.writeInput("a.csv", "a")
	h.writeInput("b.csv", "b")
	resp := h.do(http.MethodPost, "/files/batch-download", strings.NewReader(url.Values{"destination": {"inputs"}, "filenames": {"a.csv", "b.csv", "../x", "missing.csv"}}.Encode()), "application/x-www-form-urlencoded")
	data, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/zip") ||
		!regexp.MustCompile(`^attachment; filename="lens_inputs_\d{14}\.zip"$`).MatchString(resp.Header.Get("Content-Disposition")) {
		t.Fatalf("%d %v", resp.StatusCode, resp.Header)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil || len(zr.File) != 2 || zr.File[0].Name != "a.csv" || zr.File[1].Name != "b.csv" {
		t.Fatalf("zip: %v %v", err, zr)
	}
	for _, tc := range []struct {
		form url.Values
		code int
		body string
	}{
		{url.Values{"destination": {"elsewhere"}, "filenames": {"a.csv"}}, 404, `{"detail":"Not Found"}`},
		{url.Values{"destination": {"inputs"}}, 400, `{"detail":"No files selected"}`},
		{url.Values{"destination": {"inputs"}, "filenames": {"nope.csv"}}, 404, `{"detail":"No valid files found"}`},
	} {
		resp := h.do(http.MethodPost, "/files/batch-download", strings.NewReader(tc.form.Encode()), "application/x-www-form-urlencoded")
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != tc.code || string(body) != tc.body {
			t.Errorf("%v: %d %s", tc.form, resp.StatusCode, body)
		}
	}
	del := h.postForm("/files/batch-delete", url.Values{"destination": {"inputs"}, "filenames": {"a.csv", "b.csv"}})
	if del.StatusCode != 303 || location(del) != "/?files_message=Deleted+2+file%28s%29&files_scope=inputs" {
		t.Fatal(del.StatusCode, location(del))
	}
	if exists(filepath.Join(h.dirs.Inputs, "a.csv")) || exists(filepath.Join(h.dirs.Inputs, "b.csv")) {
		t.Fatal("not deleted")
	}
	if resp := h.postForm("/files/batch-delete", url.Values{"destination": {"inputs"}}); location(resp) != "/?files_error=No+files+selected&files_scope=inputs" {
		t.Fatal(location(resp))
	}
}

type upload struct {
	name    string
	content []byte
}

func (h *harness) upload(destination string, files ...upload) *http.Response {
	h.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if destination != "" {
		_ = mw.WriteField("destination", destination)
	}
	for _, f := range files {
		// Raw Content-Disposition so names with "/" or quotes reach the
		// server exactly as sent.
		hdr := make(map[string][]string)
		hdr["Content-Disposition"] = []string{`form-data; name="file"; filename="` + strings.ReplaceAll(f.name, `"`, `\"`) + `"`}
		hdr["Content-Type"] = []string{"application/octet-stream"}
		pw, _ := mw.CreatePart(hdr)
		_, _ = pw.Write(f.content)
	}
	_ = mw.Close()
	resp := h.do(http.MethodPost, "/files/upload", &buf, mw.FormDataContentType())
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp
}

func TestUploads(t *testing.T) {
	h := newHarness(t, false)
	csv := []byte("pageURL\nexample.com\n")
	for _, tc := range []struct {
		name     string
		files    []upload
		stored   []string
		location string
	}{
		{"spaces", []upload{{"my file.csv", csv}}, []string{"my file.csv"}, "/?files_scope=inputs&files_message=Uploaded+my+file.csv"},
		{"xlsx", []upload{{"my workbook.xlsx", []byte("PK\x03\x04rest")}}, []string{"my workbook.xlsx"}, ""},
		{"sanitized", []upload{{"AT&T, John's list.csv", csv}}, []string{"AT_T_ John_s list.csv"}, ""},
		{"corrupt xlsx", []upload{{"broken.xlsx", []byte("not actually a zip archive")}}, nil,
			"/?files_scope=inputs&files_error=Skipped%3A+broken.xlsx+%28File+does+not+look+like+a+valid+.xlsx+workbook+%28wrong+file+contents%29%29"},
		{"traversal", []upload{{"../escape.csv", csv}}, nil, "/?files_scope=inputs&files_error=Skipped%3A+..%2Fescape.csv+%28Filename+must+not+contain+path+separators%29"},
		{"empty name", []upload{{"", csv}}, nil, "/?files_scope=inputs&files_error=Skipped%3A+unnamed-file+%28Filename+cannot+be+empty%29"},
		{"empty file", []upload{{"e.csv", nil}}, nil, "/?files_scope=inputs&files_error=Skipped%3A+e.csv+%28Uploaded+file+is+empty%29"},
		{"several", []upload{{"one.csv", csv}, {"two.csv", csv}, {"bad.txt", csv}}, []string{"one.csv", "two.csv"},
			"/?files_scope=inputs&files_message=Uploaded+2+files&files_error=Skipped%3A+bad.txt+%28Input+files+must+be+.csv+or+.xlsx%29"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := h.upload("inputs", tc.files...)
			if resp.StatusCode != 303 {
				t.Fatal(resp.StatusCode)
			}
			if tc.location != "" && location(resp) != tc.location {
				t.Fatalf("location %s", location(resp))
			}
			if tc.stored == nil && !strings.Contains(location(resp), "files_error") {
				t.Fatal(location(resp))
			}
			for _, name := range tc.stored {
				if !isRegular(filepath.Join(h.dirs.Inputs, name)) {
					t.Fatalf("%s not stored", name)
				}
			}
		})
	}
	if exists(filepath.Join(h.dirs.Inputs, "broken.xlsx")) || exists(filepath.Join(filepath.Dir(h.dirs.Inputs), "escape.csv")) {
		t.Fatal("rejected upload written")
	}
	if resp := h.upload("outputs", upload{"x.csv", csv}); location(resp) != "/?files_error=Only+input+uploads+are+allowed&files_scope=inputs" {
		t.Fatal(location(resp))
	}
	if resp := h.upload("inputs"); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatal(resp.StatusCode)
	}
}

func TestUploadRejectsOversizeFileWhileReading(t *testing.T) {
	h := newHarness(t, false)
	h.srv.maxUpload = 1024 * 1024
	resp := h.upload("inputs", upload{"big.csv", bytes.Repeat([]byte("x"), 1024*1024+1)}, upload{"ok.csv", []byte("Domain\na.com\n")})
	if exists(filepath.Join(h.dirs.Inputs, "big.csv")) || !exists(filepath.Join(h.dirs.Inputs, "ok.csv")) {
		t.Fatal("cap not enforced per file")
	}
	if loc := location(resp); !strings.Contains(loc, "files_error") || !strings.Contains(loc, "exceeds+1+MB+limit") {
		t.Fatal(loc)
	}
}

func TestJobsStateFileIsNeverExposed(t *testing.T) {
	h := newHarness(t, false)
	state := h.writeOutput(jobs.JobsStateFilename, "{}")
	if resp, _ := h.get("/files/download/outputs/" + jobs.JobsStateFilename); resp.StatusCode != 404 {
		t.Fatal(resp.StatusCode)
	}
	resp := h.postForm("/files/delete", url.Values{"destination": {"outputs"}, "filename": {jobs.JobsStateFilename}})
	if resp.StatusCode != 303 || !exists(state) {
		t.Fatal("deleted via /files/delete")
	}
	resp = h.postForm("/files/batch-delete", url.Values{"destination": {"outputs"}, "filenames": {jobs.JobsStateFilename}})
	if resp.StatusCode != 303 || !exists(state) {
		t.Fatal("deleted via batch-delete")
	}
}

func TestDownload(t *testing.T) {
	h := newHarness(t, false)
	h.writeOutput("run_output.csv", "Domain\nexample.com\n")
	h.writeInput("my list.csv", "Domain\n")
	// A backslash is a path separator to SafeFilename: 404, never a 500.
	for _, p := range []string{"/files/download/outputs/..%5Cpasswd", "/files/download/outputs/..%2Fpasswd", "/files/download/elsewhere/x.csv", "/files/download/outputs/missing.csv"} {
		if resp, body := h.get(p); resp.StatusCode != 404 || body != `{"detail":"Not Found"}` {
			t.Errorf("%s: %d %s", p, resp.StatusCode, body)
		}
	}
	resp, body := h.get("/files/download/outputs/run_output.csv")
	if resp.StatusCode != 200 || body != "Domain\nexample.com\n" || resp.Header.Get("Content-Type") != "text/csv; charset=utf-8" ||
		resp.Header.Get("Content-Disposition") != `attachment; filename="run_output.csv"` {
		t.Fatal(resp.StatusCode, resp.Header)
	}
	resp, _ = h.get("/files/download/inputs/my%20list.csv")
	if resp.Header.Get("Content-Disposition") != "attachment; filename*=utf-8''my%20list.csv" {
		t.Fatal(resp.Header.Get("Content-Disposition"))
	}
}

func TestDeleteFile(t *testing.T) {
	h := newHarness(t, false)
	h.writeOutput("run.log", "x")
	for _, tc := range []struct {
		form url.Values
		loc  string
	}{
		{url.Values{"destination": {"outputs"}, "filename": {"run.log"}}, "/?files_message=Deleted+run.log&files_scope=outputs"},
		{url.Values{"destination": {"outputs"}, "filename": {"run.log"}}, "/?files_error=File+not+found&files_scope=outputs"},
		{url.Values{"destination": {"nowhere"}, "filename": {"run.log"}}, "/?files_error=Invalid+destination&files_scope=nowhere"},
		{url.Values{"destination": {"inputs"}, "filename": {"a/b"}}, "/?files_error=Filename+must+not+contain+path+separators&files_scope=inputs"},
	} {
		if resp := h.postForm("/files/delete", tc.form); location(resp) != tc.loc {
			t.Errorf("%v: %s", tc.form, location(resp))
		}
	}
}

func TestEnqueueErrorRedirectIsURLEncoded(t *testing.T) {
	h := newHarness(t, false)
	resp := h.postForm("/jobs", url.Values{"input_file": {`bad\name.csv`}, "mode": {"direct"}})
	loc := location(resp)
	// The message must be query-encoded: raw spaces in a Location header
	// are invalid and leak unencoded user input.
	if resp.StatusCode != 303 || strings.Contains(loc, " ") || loc != "/?error=Filename+must+not+contain+path+separators" {
		t.Fatal(resp.StatusCode, loc)
	}
	h.writeInput("list.csv", "Domain\n")
	if resp := h.postForm("/jobs", url.Values{"input_file": {"missing.csv"}}); location(resp) != "/?error=Input+file+does+not+exist" {
		t.Fatal(location(resp))
	}
	if resp := h.postForm("/jobs", url.Values{"input_file": {"list.csv"}, "mode": {"nope"}}); location(resp) != "/?error=Invalid+mode" {
		t.Fatal(location(resp))
	}
	if resp := h.postForm("/jobs", url.Values{}); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatal(resp.StatusCode)
	}
}

func TestEnqueueModelAndResearchOptions(t *testing.T) {
	h := newHarness(t, false)
	h.catalog.cat = catalog.Catalog{
		Classify: []catalog.Option{{ID: "cheap/model", Label: "Cheap Model"}},
		Research: []catalog.Option{{ID: "perplexity/sonar-pro", Label: "Sonar Pro"}},
	}
	h.writeInput("list.csv", "Domain\nexample.com\n")
	post := func(v url.Values) string {
		v.Set("input_file", "list.csv")
		return location(h.postForm("/jobs", v))
	}
	// Checkbox pattern: hidden "off" + checked "on" → enabled (last wins).
	if loc := post(url.Values{"llm_model": {"cheap/model"}, "research_fallback": {"off", "on"}, "deep_retry": {"off", "on"}}); !strings.Contains(loc, "message=") {
		t.Fatal(loc)
	}
	// Unchecked: only the hidden "off" arrives.
	post(url.Values{"research_fallback": {"off"}, "deep_retry": {"off"}})
	// The recommended models need no override flag.
	post(url.Values{"llm_model": {catalog.RecommendedModel}, "research_model": {catalog.RecommendedResearchModel}})
	post(url.Values{"research_model": {"perplexity/sonar-pro"}, "name": {"  my   run  "}})
	for v, want := range map[string]string{"llm_model": "error=Unknown+model", "research_model": "error=Unknown+research+model"} {
		if loc := post(url.Values{v: {"evil/not-in-list"}}); !strings.Contains(loc, want) {
			t.Fatal(loc)
		}
	}
	list := h.oldestFirst()
	if len(list) != 4 {
		t.Fatalf("%d jobs", len(list))
	}
	if list[0].LLMModel != "cheap/model" || !list[0].ResearchFallback || !list[0].DeepRetry {
		t.Fatalf("job0 %+v", list[0])
	}
	if list[1].LLMModel != "" || list[1].ResearchFallback || list[1].DeepRetry {
		t.Fatalf("job1 %+v", list[1])
	}
	if list[2].LLMModel != "" || list[2].ResearchModel != "" {
		t.Fatalf("job2 %+v", list[2])
	}
	if list[3].ResearchModel != "perplexity/sonar-pro" || list[3].Name != "my run" {
		t.Fatalf("job3 %+v", list[3])
	}
	if got := settingsSummary(&list[1]); got != "headless Chrome retry off · research fallback off" {
		t.Fatal(got)
	}
}

func TestIndexRendersAdvancedModelPicker(t *testing.T) {
	h := newHarness(t, false)
	h.catalog.cat = catalog.Catalog{
		Classify: []catalog.Option{{ID: catalog.RecommendedModel, Label: "Gemini Flash Latest"}, {ID: "cheap/model", Label: "Cheap Model · $0.1/M in · $0.4/M out"}},
		Research: []catalog.Option{{ID: catalog.RecommendedResearchModel, Label: "Sonar Pro · $3/M in · $15/M out · search from $5/1K requests"}, {ID: "perplexity/sonar-pro", Label: "Sonar · $1/M in · $1/M out"}},
	}
	resp, body := h.get("/")
	if resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	for _, want := range []string{
		`id="llm-model-select"`, `data-previous-defaults="~google/gemini-flash-latest"`, "Gemini Flash Latest — recommended",
		"cheap/model", `name="research_fallback"`, "Sonar Pro · $3/M in · $15/M out · search from $5/1K requests — recommended",
		`data-previous-defaults="perplexity/sonar-pro"`, "TypeSafe: Jev Latest — recommended",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Count(body, `value="`+catalog.RecommendedModel+`"`) != 1 {
		t.Error("recommended model listed twice")
	}
}

func TestIndexRequiresSignIn(t *testing.T) {
	h := newHarness(t, false)
	resp, err := http.Get(h.ts.URL + "/?x=1") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	_ = resp
	anon := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequest(http.MethodGet, h.ts.URL+"/?x=1", nil) //nolint:noctx // test
	r, err := anon.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Body.Close()
	want := "https://auth.elcanotek.com/?return_to=" + pyQuote(h.ts.URL+"/?x=1", "")
	if r.StatusCode != 303 || location(r) != want {
		t.Fatalf("%d %s", r.StatusCode, location(r))
	}
	for _, path := range []string{"/api/jobs/live", "/api/jobs/queue", "/files/download/inputs/a.csv"} {
		req, _ := http.NewRequest(http.MethodGet, h.ts.URL+path, nil) //nolint:noctx // test
		r, _ := anon.Do(req)
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if r.StatusCode != 401 || string(body) != `{"detail":"Unauthorized"}` {
			t.Errorf("%s: %d %s", path, r.StatusCode, body)
		}
	}
	req, _ = http.NewRequest(http.MethodPost, h.ts.URL+"/jobs", strings.NewReader("input_file=a.csv")) //nolint:noctx // test
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r, _ = anon.Do(req)
	_ = r.Body.Close()
	if r.StatusCode != 401 {
		t.Fatal(r.StatusCode)
	}
}

func TestRenamesAndFileLibraryRendering(t *testing.T) {
	h := newHarness(t, false)
	h.writeInput("old name.csv", "Domain\nfoo.com\n")
	if resp := h.postForm("/files/rename", url.Values{"destination": {"inputs"}, "filename": {"old name.csv"}, "new_name": {"JPMC allowlist.csv"}}); location(resp) != "/?files_message=Renamed+to+JPMC+allowlist.csv&files_scope=inputs" {
		t.Fatal(location(resp))
	}
	if exists(filepath.Join(h.dirs.Inputs, "old name.csv")) || !exists(filepath.Join(h.dirs.Inputs, "JPMC allowlist.csv")) {
		t.Fatal("rename")
	}
	// The extension is kept when the new name has none.
	h.writeInput("list.xlsx", "PK\x03\x04fake")
	h.postForm("/files/rename", url.Values{"destination": {"inputs"}, "filename": {"list.xlsx"}, "new_name": {"Q1 priorities"}})
	if !exists(filepath.Join(h.dirs.Inputs, "Q1 priorities.xlsx")) {
		t.Fatal("extension not kept")
	}
	h.writeInput("a.csv", "x")
	h.writeInput("b.csv", "y")
	for _, tc := range []struct {
		form url.Values
		loc  string
	}{
		{url.Values{"destination": {"inputs"}, "filename": {"a.csv"}, "new_name": {"b.csv"}}, "/?files_error=A+file+named+b.csv+already+exists&files_scope=inputs"},
		{url.Values{"destination": {"inputs"}, "filename": {"a.csv"}, "new_name": {"../escape.csv"}}, "/?files_error=Filename+must+not+contain+path+separators&files_scope=inputs"},
		{url.Values{"destination": {"outputs"}, "filename": {"run_output.csv"}, "new_name": {"renamed.csv"}}, "/?files_error=Only+input+files+can+be+renamed&files_scope=outputs"},
		{url.Values{"destination": {"inputs"}, "filename": {"a.csv"}, "new_name": {"a"}}, "/?files_message=Name+unchanged&files_scope=inputs"},
		{url.Values{"destination": {"inputs"}, "filename": {"zz.csv"}, "new_name": {"y"}}, "/?files_error=File+not+found&files_scope=inputs"},
		{url.Values{"destination": {"inputs"}, "filename": {"a.csv"}, "new_name": {"a.txt"}}, "/?files_error=Input+files+must+be+.csv+or+.xlsx&files_scope=inputs"},
	} {
		if resp := h.postForm("/files/rename", tc.form); location(resp) != tc.loc {
			t.Errorf("%v: %s", tc.form, location(resp))
		}
	}
	if b, _ := os.ReadFile(filepath.Join(h.dirs.Inputs, "b.csv")); string(b) != "y" { //nolint:gosec // test file
		t.Fatal("target overwritten")
	}
}

func TestBreakdownRendering(t *testing.T) {
	prev := jobs.ComputeBreakdown
	t.Cleanup(func() { jobs.ComputeBreakdown = prev })
	jobs.ComputeBreakdown = func(string) (map[string]int, error) {
		return map[string]int{"websites": 3, "ios": 1, "android": 1, "total": 5}, nil
	}
	h := newHarness(t, false)
	h.writeInput("mixed.csv", "identifier\nexample.com\n")
	h.writeInput("other.csv", "identifier\nexample.com\n")
	_, body := h.get("/")
	for _, want := range []string{
		`<span class="comp-seg seg-site" data-pct="60.0"></span>`,
		`<strong>5</strong> rows`,
		`<span class="tchip"><span class="tdot seg-site"></span>Websites <b>3</b></span>`,
		`<span class="tchip"><span class="tdot seg-ios"></span>iOS <b>1</b></span>`,
		`aria-label="3 Websites · 1 iOS · 1 Android"`,
		`data-breakdown='{"total": 5, "types": [{"count": 3, "key": "websites", "label": "Websites", "pct": 60.0, "seg": "seg-site"}, {"count": 1, "key": "ios", "label": "iOS", "pct": 20.0, "seg": "seg-ios"}, {"count": 1, "key": "android", "label": "Android", "pct": 20.0, "seg": "seg-android"}]}'`,
		`Uploaded files <span class="ws-count">(2)</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %s", want)
		}
	}
	if strings.Contains(body, `id="file-search"`) {
		t.Error("search box shown for two files")
	}
}

func TestInputFilesListedNewestFirst(t *testing.T) {
	h := newHarness(t, false)
	old := h.writeInput("old.csv", "x")
	h.writeInput("new.csv", "y")
	past := mustTime(t, old).Add(-3600e9)
	_ = os.Chtimes(old, past, past)
	_, body := h.get("/")
	if strings.Index(body, `value="new.csv"`) > strings.Index(body, `value="old.csv"`) {
		t.Fatal("not newest first")
	}
	if !strings.Contains(body, `Uploaded files <span class="ws-count">(2)</span>`) {
		t.Fatal("count")
	}
}
