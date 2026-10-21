// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ElcanoTek/lens/internal/config"
	"github.com/ElcanoTek/lens/internal/logx"
	"github.com/ElcanoTek/lens/internal/openrouter"
	"github.com/ElcanoTek/lens/internal/scrape"
)

// rewriteTransport sends every request, whatever its URL, to one test
// server, keeping the Host header so the server knows which site was asked
// for. It lets the real direct scraper fetch "https://example.com".
type rewriteTransport struct{ target *url.URL }

func (rt rewriteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	out := r.Clone(r.Context())
	out.URL.Scheme = rt.target.Scheme
	out.URL.Host = rt.target.Host
	resp, err := http.DefaultTransport.RoundTrip(out)
	if resp != nil {
		resp.Request = r // the scraper checks where it ended up
	}
	return resp, err
}

// fakeWeb serves a readable page per host, a 403 for blocked hosts and a
// JS-only stub for others.
func fakeWeb(t *testing.T, blocked, jsOnly []string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch {
		case slices.Contains(blocked, host):
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, "<html><body>Forbidden</body></html>")
		case slices.Contains(jsOnly, host):
			_, _ = io.WriteString(w, `<html><head><title>App</title></head><body><div id="root"></div><script>render()</script></body></html>`)
		default:
			body := strings.Repeat(fmt.Sprintf("<p>%s publishes documentation, software releases and community news for developers.</p>", host), 8)
			_, _ = fmt.Fprintf(w, `<html><head><title>%s home</title><meta name="description" content="About %s"></head><body>%s</body></html>`, host, host, body)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fakeOpenRouter answers chat completions like OpenRouter: tool calls for
// classification, plain content for research and the connection test, and
// usage.cost on every reply.
type fakeOpenRouter struct {
	mu       sync.Mutex
	requests []map[string]any
	srv      *httptest.Server
}

func newFakeOpenRouter(t *testing.T) *fakeOpenRouter {
	t.Helper()
	f := &fakeOpenRouter{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("bad request body: %v", err)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		f.mu.Lock()
		f.requests = append(f.requests, body)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		usage := map[string]any{"total_tokens": 10, "cost": 0.001}
		if tools, ok := body["tools"].([]any); ok && len(tools) > 0 {
			args, _ := json.Marshal(map[string]any{
				"quality": "Standard", "justification": "Well-maintained project site", "vertical": "Technology & Computing",
				"description": "An open-source project site", "confidence": "High", "language": "English",
				"political_leaning": "Non-Political", "audience_size": "M",
			})
			name := tools[0].(map[string]any)["function"].(map[string]any)["name"]
			_ = json.NewEncoder(w).Encode(map[string]any{
				"model":   "openai/gpt-6-luna",
				"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"tool_calls": []any{map[string]any{"function": map[string]any{"name": name, "arguments": string(args)}}}}}},
				"usage":   usage,
			})
			return
		}
		content := "OK"
		if body["model"] == "perplexity/sonar" {
			content = "This domain belongs to a long-established open-source foundation that publishes documentation, release notes and community news read by developers worldwide."
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   body["model"],
			"choices": []any{map[string]any{"message": map[string]any{"content": content}}},
			"usage":   usage,
		})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// TestEndToEndRunWithRealClients runs `lens run` on the CI sample list with
// the real OpenRouter client and the real direct scraper, both pointed at
// httptest servers, and checks the output CSV and progress file.
func TestEndToEndRunWithRealClients(t *testing.T) {
	sample, err := os.ReadFile(filepath.Join("..", "..", "test-input", "url_master.csv"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	t.Chdir(dir) // no config.json or .env from the developer's tree
	input := filepath.Join(dir, "url_master.csv")
	writeFile(t, input, string(sample))
	lines := strings.Split(strings.TrimSpace(strings.ReplaceAll(string(sample), "\r\n", "\n")), "\n")[1:]
	domains := make([]string, 0, len(lines))
	for _, line := range lines {
		domains = append(domains, strings.SplitN(line, ",", 2)[0])
	}

	web := fakeWeb(t, []string{"example.net"}, []string{"example.org"})
	webURL, _ := url.Parse(web.URL)
	api := newFakeOpenRouter(t)
	t.Setenv("OPENROUTER_API_KEY", "test-key")
	t.Setenv("SCRAPER_VERBOSE", "")

	deps := Deps{
		NewLLM: func(_ context.Context, opts LLMOptions) (LLM, error) {
			c, err := openrouter.New(openrouter.Options{
				APIKey: opts.APIKey, Model: opts.Model, FallbackModel: opts.FallbackModel,
				Temperature: opts.Temperature, MaxTokens: opts.MaxTokens, MaxRetries: opts.MaxRetries,
				BaseURL: api.srv.URL + "/api/v1",
			})
			if err != nil {
				return nil, err
			}
			return NewLLMAdapter(c), nil
		},
		NewScraper: func(ctx context.Context, opts ScraperOptions) (ScraperBackend, error) {
			o := scrape.OptionsFromConfig(config.Default(), opts.Mode, opts.RejectRedirects)
			o.HTTPClient = &http.Client{Transport: rewriteTransport{webURL}}
			o.MaxRetries = 1
			return StartScraper(ctx, o)
		},
		DeepAvailable: func() bool { return false }, // no podman here: the ladder goes direct -> research
	}
	output := filepath.Join(dir, "out", "results.csv")
	progressPath := filepath.Join(dir, "out", "progress.json")
	if err := os.MkdirAll(filepath.Dir(output), 0o750); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr lockedBuffer
	code := runMain(context.Background(), []string{
		"--scrape-mode", "auto", "--quiet",
		"--input-csv", input, "--output-csv", output,
		"--progress-file", progressPath, "--log-file", filepath.Join(dir, "run.log"),
		"--jsonl", filepath.Join(dir, "run.jsonl"),
	}, deps, &stdout, &stderr)
	logx.SetOutput(io.Discard, slog.LevelError)
	if code != 0 {
		logText, _ := os.ReadFile(filepath.Join(dir, "run.log"))
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s\nlog:\n%s", code, stdout.String(), stderr.String(), logText)
	}

	raw, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(raw, []byte(strings.Join(config.CSVFieldnames, ",")+"\r\n")) || bytes.Count(raw, []byte("\r\n")) != len(domains)+1 {
		t.Fatalf("output CSV header/line endings:\n%s", raw)
	}
	_, rows := readCSV(t, output)
	if len(rows) != len(domains) {
		t.Fatalf("%d rows for %d domains", len(rows), len(domains))
	}
	byDomain := map[string]map[string]string{}
	for _, r := range rows {
		byDomain[r["Domain"]] = r
	}
	for _, d := range domains {
		r := byDomain[d]
		if r == nil {
			t.Fatalf("no row for %s", d)
		}
		wantMode, wantBot, wantCost := "direct", "None Detected", "0.001000"
		switch d {
		case "example.net": // 403 -> research rescue
			wantMode, wantBot, wantCost = "research", "Moderate", "0.002000"
		case "example.org": // JS-only page -> research rescue
			wantMode, wantBot, wantCost = "research", "Unknown", "0.002000"
		}
		if r["Quality"] != "Standard" || r["Type"] != "WEBSITE" || r["Scrape_Mode"] != wantMode ||
			r["Bot_Protection"] != wantBot || r["Cost_USD"] != wantCost || r["IAB Tier 1"] != "Technology & Computing" ||
			r["Classifier_Mode"] != "openrouter" || r["Language"] != "English" {
			t.Errorf("%s: %v", d, r)
		}
		if wantMode == "direct" && (r["Content_Length"] == "0" || !strings.Contains(r["Scraped_At"], "T")) {
			t.Errorf("%s: content length / timestamp: %v", d, r)
		}
	}

	var prog struct {
		Total      int `json:"total_domains"`
		Successful int `json:"successful_count"`
		Errors     int `json:"error_count"`
		Processed  map[string]struct {
			Status string         `json:"status"`
			Result map[string]any `json:"result"`
		} `json:"processed_domains"`
		Cost struct {
			USD   float64 `json:"usd"`
			Calls int     `json:"calls"`
		} `json:"cost"`
	}
	rawProgress, err := os.ReadFile(progressPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(rawProgress, &prog); err != nil {
		t.Fatal(err)
	}
	if prog.Total != len(domains) || prog.Successful != len(domains) || prog.Errors != 0 || len(prog.Processed) != len(domains) {
		t.Fatalf("progress = %+v", prog)
	}
	res := prog.Processed["example.net"].Result
	if res["Scrape_Mode"] != "research" || res["Quality"] != "Standard" || res["Cost_USD"] != "0.002000" {
		t.Fatalf("progress result = %v", res)
	}
	// One connection test, one classification per domain, research +
	// classification for each of the two rescued domains.
	wantCalls := 1 + len(domains) + 2
	if prog.Cost.Calls != wantCalls || api.count() != wantCalls {
		logText, _ := os.ReadFile(filepath.Join(dir, "run.log"))
		t.Fatalf("calls: progress=%d api=%d want %d\nlog:\n%.3000s", prog.Cost.Calls, api.count(), wantCalls, logText)
	}
	if !strings.Contains(stdout.String(), "--- Lens run complete ---") || !strings.Contains(stdout.String(), fmt.Sprintf("✅ Success: %d", len(domains))) {
		t.Fatalf("stdout:\n%s", stdout.String())
	}
	logText, _ := os.ReadFile(filepath.Join(dir, "run.log"))
	for _, want := range []string{
		" - orchestration - INFO - Auto mode: classifying 2 unscrapeable website(s) from external research (perplexity/sonar)",
		"OpenRouter spend for this run: $",
		"Site analysis completed successfully!",
	} {
		if !strings.Contains(string(logText), want) {
			t.Errorf("log lacks %q", want)
		}
	}
	jsonl, _ := os.ReadFile(filepath.Join(dir, "run.jsonl"))
	if bytes.Count(jsonl, []byte("\n")) != len(domains)+2 {
		t.Errorf("jsonl attempts = %d", bytes.Count(jsonl, []byte("\n")))
	}

	// A second run resumes: everything is done, nothing is fetched or billed.
	before := api.count()
	stdout.Reset()
	code = runMain(context.Background(), []string{"--quiet", "--input-csv", input, "--output-csv", output,
		"--progress-file", progressPath, "--log-file", filepath.Join(dir, "run.log")}, deps, &stdout, &stderr)
	logx.SetOutput(io.Discard, slog.LevelError)
	if code != 0 || api.count() != before {
		t.Fatalf("resume: exit %d, %d new API calls", code, api.count()-before)
	}
	if again, _ := os.ReadFile(output); !bytes.Equal(again, raw) {
		t.Fatal("resume changed the output")
	}
}

func (f *fakeOpenRouter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// lockedBuffer is a bytes.Buffer safe for the log handler and the reporter
// writing to it at once (os.Stdout is in production).
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func (l *lockedBuffer) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.b.Reset()
}
