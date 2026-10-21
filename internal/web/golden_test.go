// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package web

import (
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/lens/internal/catalog"
	"github.com/ElcanoTek/lens/internal/jobs"
)

// TestRenderMatchesPython renders the dashboard for a rich state (uploads,
// live/queued/failed/cancelled jobs, archived artifacts) and diffs it with
// what the Python release renders for the same state. It needs the Python
// reference environment, so it only runs when LENS_PYREF names its python
// (run from the repository root's internal/web directory):
//
//	LENS_PYREF=/path/to/venv/bin/python LENS_GOLDEN_OUT=/tmp/out go test -run RenderMatchesPython ./internal/web
func TestRenderMatchesPython(t *testing.T) {
	python := os.Getenv("LENS_PYREF")
	if python == "" {
		t.Skip("LENS_PYREF not set")
	}
	state := os.Getenv("LENS_GOLDEN_OUT")
	if state == "" {
		state = t.TempDir()
	}
	_ = os.RemoveAll(state)
	home := filepath.Join(state, "home")
	repo, _ := filepath.Abs("../..")

	script := `
for a in "$@"; do case "$prev" in --progress-file) prog="$a";; --input-csv) in="$a";; esac; prev="$a"; done
case "$in" in
  *quick*) exit 0;;
  *fail*) exit 3;;
esac
printf '%s' '{"total_domains": 120, "processed_domains": {"a.com": {"status": "success"}, "b.com": {"status": "retry_pending"}, "c.com": {"status": "error"}}, "successful_count": 1, "error_count": 1, "cost": {"usd": 0.01234, "calls": 7, "unpriced_calls": 1}}' > "$prog"
sleep 60
`
	cat := catalog.Catalog{
		Classify: []catalog.Option{{ID: catalog.RecommendedModel, Label: "OpenAI: GPT Luna Latest · $0.2/M in · $1.2/M out"}, {ID: "cheap/model", Label: "Cheap Model · $0.1/M in · $0.4/M out"}},
		Research: []catalog.Option{{ID: "perplexity/sonar-pro", Label: "Perplexity: Sonar Pro · $3/M in · $15/M out · search from $5/1K requests"}},
		Decision: []catalog.Option{{ID: catalog.RecommendedDecisionModel, Label: "TypeSafe: Jev · $0.1/M in"}},
	}
	if err := os.MkdirAll(filepath.Join(home, "managed-files", "inputs"), 0o750); err != nil {
		t.Fatal(err)
	}
	inputs := filepath.Join(home, "managed-files", "inputs")
	files := map[string]string{
		"mixed list.csv":      "identifier\nexample.com\nanother-site.org\nhttps://third.io\n284882215\ncom.example.app\n",
		"quick (2).csv":       "Domain\nexample.com\n",
		"fail.csv":            "Domain\nexample.com\n",
		"long.csv":            "example.com\nfoo.com\nbar.com\n",
		"ctv.csv":             "App Name,Bundle ID\nPlex,com.plexapp.ctv\nRoku,12345\n",
		"empty-ish.csv":       "notes\nhello world\n",
		"AT_T ünïcode x.csv":  "Domain\nexample.com\n",
	}
	ages := []time.Duration{0, 30 * time.Hour, 72 * time.Hour, 10 * time.Minute, 400 * 24 * time.Hour, 50 * time.Hour, 5 * time.Minute}
	i := 0
	for name, body := range files {
		p := filepath.Join(inputs, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		mt := time.Now().Add(-ages[i%len(ages)])
		_ = os.Chtimes(p, mt, mt)
		i++
	}
	// Python computes the breakdown caches (detection is another package's
	// port); both sides then read the same sidecars.
	run := func(mode string) {
		cmd := exec.Command(python, filepath.Join(filepath.Dir(state), "pyrender.py"), state, mode) //nolint:gosec // test-only reference run
		if p := os.Getenv("LENS_PYRENDER"); p != "" {
			cmd = exec.Command(python, p, state, mode) //nolint:gosec // test-only reference run
		}
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "OPENROUTER_API_KEY=test")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("python %s: %v\n%s", mode, err, out)
		}
		t.Logf("python %s: %s", mode, out)
	}
	run("breakdowns")

	outputs := filepath.Join(home, "managed-files", "outputs")
	_ = os.MkdirAll(outputs, 0o750)
	archived := map[string]string{
		"20260101120000-abcdef_output.csv":                 "Domain\n",
		"20260101120000-abcdef_progress.json":              `{"summary": {"total_domains": 3, "processed": 3, "successful": 2, "errors": 1}, "cost": {"usd": 1234.5, "calls": 3}}`,
		"20260101120000-abcdef.log":                        "x\n",
		"20260101120000-abcdef_output.csv.categories.json": "{}",
		"20251231235959-zzzzzz_progress.json":              `{"total_domains": 0}`,
		"legacy-run.log":                                   "x\n",
		"notes.txt":                                        "ignored",
	}
	for name, body := range archived {
		if err := os.WriteFile(filepath.Join(outputs, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	h := newHarness(t, true, func(o *Options) {
		o.Home = home
		m := jobs.NewManager(home)
		m.Executable = fakeExecutable(t, state, script)
		m.BaseArgs = nil
		m.MaxParallel = 1
		// Jobs were created and started 20 minutes ago, so the ETA is real.
		m.Now = func() time.Time { return time.Now().Add(-20 * time.Minute) }
		o.Manager = m
	})
	h.catalog.cat = cat
	m := h.srv.Manager()
	mk := func(n jobs.NewJob) *jobs.Job {
		if n.Mode == "" {
			n.Mode = "auto"
		}
		j, err := m.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(1100 * time.Millisecond) // distinct ids/created_at seconds
		return j
	}
	quick := mk(jobs.NewJob{InputFile: "quick (2).csv", Name: "Quick & <dirty> \"run\"", ResearchFallback: true, DeepRetry: true})
	waitStatus(t, h, quick.ID, jobs.StatusCompleted)
	failed := mk(jobs.NewJob{InputFile: "fail.csv", ResearchFallback: false, DeepRetry: false, LLMModel: "cheap/model"})
	waitStatus(t, h, failed.ID, jobs.StatusFailed)
	cats := json.RawMessage(`{"categories":[{"name":"Educational","type":"boolean","question":"Teach?"},{"name":"Tone","type":"choice","question":"Tone?","options":["Calm","Loud"]}]}`)
	long := mk(jobs.NewJob{InputFile: "long.csv", ResearchFallback: true, DeepRetry: true, ResearchModel: "perplexity/sonar-pro", CustomCategories: cats, DecisionModel: "~typesafe/jev-latest", SubmittedBy: "fleet"})
	waitStatus(t, h, long.ID, jobs.StatusRunning)
	t.Cleanup(func() { _ = m.Cancel(long.ID) })
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(outputs, long.ID+"_progress.json")); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	queued := mk(jobs.NewJob{InputFile: "mixed list.csv", Mode: "ctv", Name: "Queued one", ResearchFallback: true, DeepRetry: true})
	cancelled := mk(jobs.NewJob{InputFile: "mixed list.csv", ResearchFallback: true, DeepRetry: true})
	if err := m.Cancel(cancelled.ID); err != nil {
		t.Fatal(err)
	}
	_ = queued

	pages := map[string]string{
		"index":      "/?message=Job+queued&files_scope=inputs&files_error=Skipped%3A+a+%28b%29&error=",
		"signed_out": "/signed-out",
	}
	for name, path := range pages {
		_, body := h.get(path)
		if err := os.WriteFile(filepath.Join(state, "go_"+name+".html"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// Dump the job table for Python (None where Go has "").
	type dumpJob map[string]any
	var list []dumpJob
	for _, j := range m.List() {
		raw, _ := json.Marshal(j)
		var d dumpJob
		_ = json.Unmarshal(raw, &d)
		for k, v := range d {
			if v == "" {
				d[k] = nil
			}
		}
		if !j.HasCategories() {
			d["custom_categories"] = nil
		}
		list = append(list, d)
	}
	catJSON, _ := json.Marshal(cat)
	dump, _ := json.Marshal(map[string]any{"jobs": list, "current": m.CurrentID(), "pages": pages})
	_ = os.WriteFile(filepath.Join(state, "jobs.json"), dump, 0o600)
	_ = os.WriteFile(filepath.Join(state, "catalog.json"), catJSON, 0o600)
	pyOut := filepath.Join(state, "py-outputs")
	_ = os.MkdirAll(pyOut, 0o750)
	entries, _ := os.ReadDir(outputs)
	for _, e := range entries {
		if e.Type().IsRegular() && e.Name() != jobs.JobsStateFilename {
			data, _ := os.ReadFile(filepath.Join(outputs, e.Name())) //nolint:gosec // test copy
			_ = os.WriteFile(filepath.Join(pyOut, e.Name()), data, 0o600)
		}
	}
	run("render")
	if out := os.Getenv("LENS_GOLDEN_OUT"); out != "" {
		t.Logf("go STATIC_VERSION %s", h.srv.StaticVersion())
	}

	normalize := func(s string) string {
		s = regexp.MustCompile(`nonce="[A-Za-z0-9_-]+"`).ReplaceAllString(s, `nonce="N"`)
		// html/template percent-encodes URL paths ("my%20file.csv"); Jinja
		// left the raw name for the browser to encode. Same request.
		s = regexp.MustCompile(`href="(/files/download/[^"]*)"`).ReplaceAllStringFunc(s, func(m string) string {
			raw := strings.TrimSuffix(strings.TrimPrefix(m, `href="`), `"`)
			if dec, err := url.PathUnescape(raw); err == nil {
				raw = dec
			}
			return `href="` + raw + `"`
		})
		// The two renders are seconds apart; a live ETA may tick.
		s = regexp.MustCompile(`~(\d+h )?\d+m remaining`).ReplaceAllString(s, "~ETA remaining")
		s = regexp.MustCompile(`\?v=[0-9a-f]{12}`).ReplaceAllString(s, `?v=V`)
		return s
	}
	for name := range pages {
		goHTML, _ := os.ReadFile(filepath.Join(state, "go_"+name+".html"))  //nolint:gosec // test output
		pyHTML, _ := os.ReadFile(filepath.Join(state, "py_"+name+".html"))  //nolint:gosec // test output
		g, p := normalize(string(goHTML)), normalize(string(pyHTML))
		if g == p {
			t.Logf("%s: identical (%d bytes)", name, len(g))
			continue
		}
		gl, pl := strings.Split(g, "\n"), strings.Split(p, "\n")
		n := 0
		for i := 0; i < max(len(gl), len(pl)) && n < 40; i++ {
			var a, b string
			if i < len(gl) {
				a = gl[i]
			}
			if i < len(pl) {
				b = pl[i]
			}
			if a != b {
				t.Errorf("%s line %d:\n  go: %s\n  py: %s", name, i+1, a, b)
				n++
			}
		}
		if len(gl) != len(pl) {
			t.Errorf("%s: %d lines (go) vs %d (py)", name, len(gl), len(pl))
		}
	}
}

func waitStatus(t *testing.T, h *harness, id, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if j, ok := h.srv.Manager().Get(id); ok && j.Status == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	j, _ := h.srv.Manager().Get(id)
	t.Fatalf("job %s is %q, want %q", id, j.Status, want)
}
