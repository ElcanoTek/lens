// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package pipeline

import (
	"bytes"
	"context"
	"encoding/csv"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/ElcanoTek/lens/internal/categories"
	"github.com/ElcanoTek/lens/internal/config"
	"github.com/ElcanoTek/lens/internal/logx"
	"github.com/ElcanoTek/lens/internal/progress"
)

// fakeScraper serves canned results per domain.
type fakeScraper struct {
	mu       sync.Mutex
	mode     string
	results  map[string]ScrapeResult
	scrape   func(domain string) ScrapeResult
	pools    []int
	closed   bool
	scraped  []string
	acquired int
	released int
}

func (f *fakeScraper) AcquireSession(context.Context) (Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acquired++
	return struct{}{}, nil
}

func (f *fakeScraper) ReleaseSession(Session) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released++
}

func (f *fakeScraper) Scrape(_ context.Context, _ Session, domain string) ScrapeResult {
	f.mu.Lock()
	f.scraped = append(f.scraped, domain)
	fn, results := f.scrape, f.results
	f.mu.Unlock()
	if fn != nil {
		return fn(domain)
	}
	if r, ok := results[domain]; ok {
		return r
	}
	return ScrapeResult{Error: "no canned result"}
}

func (f *fakeScraper) Mode() string {
	if f.mode == "" {
		return "direct"
	}
	return f.mode
}

func (f *fakeScraper) CreateSessionPool(_ context.Context, n int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pools = append(f.pools, n)
	return nil
}

func (f *fakeScraper) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

// fakeLLM records calls and answers through overridable functions.
type fakeLLM struct {
	mu             sync.Mutex
	connectionOK   bool
	classifySite   func(SiteInput) (Classification, error)
	researchSite   func(string, ResearchOptions) (Research, error)
	classifyApp    func(AppInput) (Classification, error)
	researchCTV    func(CTVResearchInput) (Research, error)
	classifyCTV    func(CTVClassifyInput) (Classification, error)
	siteCalls      []SiteInput
	researchCalls  []string
	researchOpts   []ResearchOptions
	appCalls       []AppInput
	ctvResearch    []CTVResearchInput
	ctvClassify    []CTVClassifyInput
	connectionRuns int
}

func premium() Classification {
	return Classification{
		Success: true, Quality: "Premium", Justification: "Major news agency",
		Tier1: "News and Politics", Description: "Global news agency",
		Language: "English", PoliticalLeaning: "Center", AudienceSize: "XL", Source: "openrouter",
	}
}

func newFakeLLM() *fakeLLM { return &fakeLLM{connectionOK: true} }

func (f *fakeLLM) TestConnection(context.Context) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connectionRuns++
	return f.connectionOK
}

func (f *fakeLLM) ClassifySite(_ context.Context, in SiteInput) (Classification, error) {
	f.mu.Lock()
	f.siteCalls = append(f.siteCalls, in)
	fn := f.classifySite
	f.mu.Unlock()
	if fn != nil {
		return fn(in)
	}
	return premium(), nil
}

func (f *fakeLLM) ResearchWebsite(_ context.Context, domain string, opts ResearchOptions) (Research, error) {
	f.mu.Lock()
	f.researchCalls = append(f.researchCalls, domain)
	f.researchOpts = append(f.researchOpts, opts)
	fn := f.researchSite
	f.mu.Unlock()
	if fn != nil {
		return fn(domain, opts)
	}
	return Research{Success: true, ResearchContent: "Reuters is a global news agency."}, nil
}

func (f *fakeLLM) ClassifyApp(_ context.Context, in AppInput) (Classification, error) {
	f.mu.Lock()
	f.appCalls = append(f.appCalls, in)
	fn := f.classifyApp
	f.mu.Unlock()
	if fn != nil {
		return fn(in)
	}
	return premium(), nil
}

func (f *fakeLLM) ResearchCTVApp(_ context.Context, in CTVResearchInput) (Research, error) {
	f.mu.Lock()
	f.ctvResearch = append(f.ctvResearch, in)
	fn := f.researchCTV
	f.mu.Unlock()
	if fn != nil {
		return fn(in)
	}
	return Research{Success: true, ResearchContent: "Netflix is a major streaming service."}, nil
}

func (f *fakeLLM) ClassifyCTVApp(_ context.Context, in CTVClassifyInput) (Classification, error) {
	f.mu.Lock()
	f.ctvClassify = append(f.ctvClassify, in)
	fn := f.classifyCTV
	f.mu.Unlock()
	if fn != nil {
		return fn(in)
	}
	c := premium()
	c.ContentType = "On-Demand Streaming"
	c.TargetAudience = "General audience"
	return c, nil
}

type fakeStore struct {
	meta  AppMetadata
	calls []string
	mu    sync.Mutex
}

func (f *fakeStore) FetchAppMetadata(_ context.Context, id string) (AppMetadata, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, id)
	return f.meta, nil
}

type classifyCall struct{ identifier, content, source, title string }

type fakeCategories struct {
	mu        sync.Mutex
	questions []string
	result    categories.Result
	calls     []classifyCall
}

func (f *fakeCategories) ResearchQuestions() []string { return f.questions }

func (f *fakeCategories) Classify(_ context.Context, identifier, content, source, title string) (categories.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, classifyCall{identifier, content, source, title})
	out := categories.Result{}
	for k, v := range f.result {
		out[k] = v
	}
	return out, nil
}

// sink is an in-memory results CSV.
type sink struct {
	buf     bytes.Buffer
	columns []string
	writer  *ResultsWriter
}

func newSink(t *testing.T, columns []string) *sink {
	t.Helper()
	s := &sink{columns: columns}
	s.writer = NewResultsWriter(&s.buf, columns)
	return s
}

func (s *sink) rows(t *testing.T) []map[string]string {
	t.Helper()
	r := csv.NewReader(bytes.NewReader(s.buf.Bytes()))
	recs, err := r.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	out := make([]map[string]string, 0, len(recs))
	for _, rec := range recs {
		row := map[string]string{}
		for i, c := range s.columns {
			row[c] = rec[i]
		}
		out = append(out, row)
	}
	return out
}

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.InputCSVPath = filepath.Join(dir, "input.csv")
	cfg.OutputCSVPath = filepath.Join(dir, "output.csv")
	cfg.ProgressFilePath = filepath.Join(dir, "progress.json")
	cfg.LogFilePath = filepath.Join(dir, "run.log")
	cfg.OpenRouterAPIKey = "test-key"
	return cfg
}

func newTracker(t *testing.T) *progress.Tracker {
	t.Helper()
	return progress.Open(filepath.Join(t.TempDir(), "progress.json"))
}

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readCSV(t *testing.T, path string) ([]string, []map[string]string) {
	t.Helper()
	header, rows, err := readCSVDicts(path)
	if err != nil {
		t.Fatal(err)
	}
	return header, rows
}

// makeOrchestrator mirrors tests/conftest.py's make_orchestrator.
func makeOrchestrator(t *testing.T, input, mode string, mutate ...func(*Options)) *Orchestrator {
	t.Helper()
	cfg := testConfig(t)
	writeFile(t, cfg.InputCSVPath, input)
	opts := Options{Config: cfg, Quiet: true, ScrapeMode: mode, RejectRedirects: true, Out: &bytes.Buffer{}}
	for _, m := range mutate {
		m(&opts)
	}
	o, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func mark(t *testing.T, tr *progress.Tracker, id, status, msg string) {
	t.Helper()
	if err := tr.Mark(id, status, nil, msg, nil, ""); err != nil {
		t.Fatal(err)
	}
}

func TestMain(m *testing.M) {
	logx.SetOutput(io.Discard, slog.LevelError)
	os.Exit(m.Run())
}
