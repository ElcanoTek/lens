// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ElcanoTek/lens/internal/categories"
	"github.com/ElcanoTek/lens/internal/config"
	"github.com/ElcanoTek/lens/internal/cost"
	"github.com/ElcanoTek/lens/internal/item"
	"github.com/ElcanoTek/lens/internal/logx"
	"github.com/ElcanoTek/lens/internal/progress"
)

const ctvCSV = "SSP,Publisher,\"App, Account or Network Name\",Bundle ID\nExampleSSP,Northwind Broadcasting,Example News Network,512001\n"

func loadItems(t *testing.T, o *Orchestrator) []item.WorkItem {
	t.Helper()
	items, err := o.loadInputData()
	if err != nil {
		t.Fatal(err)
	}
	return items
}

func ids(items []item.WorkItem) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Identifier
	}
	return out
}

// recorder replaces processItems and records what a pass would process.
type recorder struct {
	mu    sync.Mutex
	items []item.WorkItem
}

func (r *recorder) process(_ context.Context, items []item.WorkItem) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items = append(r.items, items...)
}

// scraperFactory records the options of every scraper built.
type scraperFactory struct {
	mu    sync.Mutex
	built []ScraperOptions
	make  func(ScraperOptions) *fakeScraper
	last  *fakeScraper
}

func (f *scraperFactory) new(_ context.Context, opts ScraperOptions) (ScraperBackend, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.built = append(f.built, opts)
	s := &fakeScraper{mode: opts.Mode}
	if f.make != nil {
		s = f.make(opts)
	}
	f.last = s
	return s, nil
}

// ---- mode resolution (tests/test_auto_mode.py) ----

func TestAutoModeResolvesToFastFirstPass(t *testing.T) {
	o := makeOrchestrator(t, "pageURL\nexample.com\n", "auto")
	if !o.autoMode || o.scrapeMode != "direct" {
		t.Fatalf("auto=%v mode=%s", o.autoMode, o.scrapeMode)
	}
	o = makeOrchestrator(t, "pageURL\nexample.com\n", "deep")
	if o.autoMode || o.scrapeMode != "deep" {
		t.Fatalf("auto=%v mode=%s", o.autoMode, o.scrapeMode)
	}
	if _, err := New(Options{Config: testConfig(t), ScrapeMode: "bogus"}); err == nil || !strings.Contains(err.Error(), "Unsupported scrape mode") {
		t.Fatalf("err = %v", err)
	}
	// "" falls back to config.json's scrape_mode.
	o = makeOrchestrator(t, "pageURL\nexample.com\n", "")
	if !o.autoMode {
		t.Fatal("config default is auto")
	}
}

func TestAutoModeRoutesCTVFilesToCTVWorkflow(t *testing.T) {
	o := makeOrchestrator(t, ctvCSV, "auto")
	if items := loadItems(t, o); len(items) != 0 || !o.isCTVInput {
		t.Fatalf("items=%v ctv=%v", items, o.isCTVInput)
	}
	o = makeOrchestrator(t, ctvCSV, "direct")
	loadItems(t, o)
	if o.isCTVInput {
		t.Fatal("non-auto mode must only warn about a CTV list")
	}
}

func TestAutoModeDetectsMixedContent(t *testing.T) {
	o := makeOrchestrator(t, "pageURL\nexample.com\n333903271\ncom.spotify.music\n", "auto")
	items := loadItems(t, o)
	if o.isCTVInput || !o.hasWebsites || !o.hasIOSApps || !o.hasAndroidApps || len(items) != 3 {
		t.Fatalf("items=%v flags=%v %v %v %v", items, o.isCTVInput, o.hasWebsites, o.hasIOSApps, o.hasAndroidApps)
	}
	if o.progress.Summary().Total != 3 {
		t.Fatal("total not set")
	}
}

// ---- deep retry pass ----

func TestDeepRetryNoopWithoutFailures(t *testing.T) {
	o := makeOrchestrator(t, "pageURL\nexample.com\n", "auto", func(opts *Options) {
		opts.Deps.DeepAvailable = func() bool { panic("availability probe should not run") }
	})
	items := loadItems(t, o)
	var stack closers
	o.retryFailedWebsitesDeep(context.Background(), &stack, items)
}

func TestDeepRetrySkippedWhenDeepCrawlUnavailable(t *testing.T) {
	rec := &recorder{}
	o := makeOrchestrator(t, "pageURL\nexample.com\n", "auto", func(opts *Options) {
		opts.Deps.DeepAvailable = func() bool { return false }
	})
	o.processItems = rec.process
	items := loadItems(t, o)
	mark(t, o.progress, "example.com", progress.StatusError, "HTTP 403")
	var stack closers
	o.retryFailedWebsitesDeep(context.Background(), &stack, items)
	if len(rec.items) != 0 {
		t.Fatalf("processed %v", rec.items)
	}
}

func TestDeepRetryTargetsOnlyFailedWebsites(t *testing.T) {
	rec := &recorder{}
	factory := &scraperFactory{}
	o := makeOrchestrator(t, "pageURL\nexample.com\ngithub.com\n333903271\n", "auto", func(opts *Options) {
		opts.Deps.DeepAvailable = func() bool { return true }
		opts.Deps.NewScraper = factory.new
	})
	o.processItems = rec.process
	items := loadItems(t, o)
	mark(t, o.progress, "example.com", progress.StatusError, "Scraper returned empty content")
	mark(t, o.progress, "github.com", progress.StatusSuccess, "")
	mark(t, o.progress, "333903271", progress.StatusError, "rate limited")

	var stack closers
	o.retryFailedWebsitesDeep(context.Background(), &stack, items)
	// Only the failed WEBSITE retries; the failed iOS app does not.
	if got := ids(rec.items); !slices.Equal(got, []string{"example.com"}) {
		t.Fatalf("processed %v", got)
	}
	if o.scrapeMode != "deep" || !slices.Equal(factory.last.pools, []int{1}) {
		t.Fatalf("mode=%s pools=%v", o.scrapeMode, factory.last.pools)
	}
	stack.closeAll()
	if !factory.last.closed {
		t.Fatal("retry scraper not closed with the exit stack")
	}
}

func TestDeepRetryIncludesRetryPendingItems(t *testing.T) {
	rec := &recorder{}
	factory := &scraperFactory{}
	o := makeOrchestrator(t, "pageURL\nexample.com\ngithub.com\n", "auto", func(opts *Options) {
		opts.Deps.DeepAvailable = func() bool { return true }
		opts.Deps.NewScraper = factory.new
	})
	o.processItems = rec.process
	items := loadItems(t, o)
	mark(t, o.progress, "example.com", progress.StatusRetryPending, "JS-only page")
	mark(t, o.progress, "github.com", progress.StatusSuccess, "")

	var stack closers
	defer stack.closeAll()
	o.retryFailedWebsitesDeep(context.Background(), &stack, items)
	if got := ids(rec.items); !slices.Equal(got, []string{"example.com"}) {
		t.Fatalf("processed %v", got)
	}
	// The research fallback runs after the deep pass by default, so deep
	// failures stay retryable; with the fallback disabled they are terminal.
	if !o.deferWebsiteFailures {
		t.Fatal("deep failures should stay retryable")
	}
	o.cfg.ResearchFallbackEnabled = false
	rec.items = nil
	o.retryFailedWebsitesDeep(context.Background(), &stack, items)
	if got := ids(rec.items); !slices.Equal(got, []string{"example.com"}) || o.deferWebsiteFailures {
		t.Fatalf("processed %v defer=%v", got, o.deferWebsiteFailures)
	}
}

func TestDeepRetrySkipsAntibotBlockedSites(t *testing.T) {
	rec := &recorder{}
	factory := &scraperFactory{}
	o := makeOrchestrator(t, "pageURL\nblocked.com\nrenderfail.com\n", "auto", func(opts *Options) {
		opts.Deps.DeepAvailable = func() bool { return true }
		opts.Deps.NewScraper = factory.new
	})
	o.processItems = rec.process
	items := loadItems(t, o)
	// The direct pass proved blocked.com is blocking this IP; renderfail.com
	// failed for a reason a rendering browser might fix.
	mark(t, o.progress, "blocked.com", progress.StatusError, "Received HTTP status 403")
	mark(t, o.progress, "renderfail.com", progress.StatusError, "Scraper returned empty content")
	var stack closers
	defer stack.closeAll()
	o.retryFailedWebsitesDeep(context.Background(), &stack, items)
	if got := ids(rec.items); !slices.Equal(got, []string{"renderfail.com"}) {
		t.Fatalf("processed %v", got)
	}
	// The deep pass follows cross-host redirects by default.
	if !slices.Equal(factory.built, []ScraperOptions{{Mode: "deep", RejectRedirects: false}}) {
		t.Fatalf("built %v", factory.built)
	}

	// Only blocked sites left: nothing for the deep crawler.
	rec.items, factory.built = nil, nil
	mark(t, o.progress, "renderfail.com", progress.StatusSuccess, "")
	o.retryFailedWebsitesDeep(context.Background(), &stack, items)
	if len(rec.items) != 0 || len(factory.built) != 0 {
		t.Fatal("deep pass ran for anti-bot blocks only")
	}

	// skip_deep_retry_on_block off: blocked sites are retried too.
	o.cfg.SkipDeepRetryOnBlk = false
	o.retryFailedWebsitesDeep(context.Background(), &stack, items)
	if got := ids(rec.items); !slices.Equal(got, []string{"blocked.com"}) {
		t.Fatalf("processed %v", got)
	}
}

func TestDeepRetryRespectsStrictRedirectPolicy(t *testing.T) {
	factory := &scraperFactory{}
	o := makeOrchestrator(t, "pageURL\nrenderfail.com\n", "auto", func(opts *Options) {
		opts.Deps.DeepAvailable = func() bool { return true }
		opts.Deps.NewScraper = factory.new
	})
	o.processItems = (&recorder{}).process
	o.cfg.RetryFollowRedirect = false
	items := loadItems(t, o)
	mark(t, o.progress, "renderfail.com", progress.StatusError, "Scraper returned empty content")
	var stack closers
	defer stack.closeAll()
	o.retryFailedWebsitesDeep(context.Background(), &stack, items)
	if !slices.Equal(factory.built, []ScraperOptions{{Mode: "deep", RejectRedirects: true}}) {
		t.Fatalf("built %v", factory.built)
	}
}

func TestDeepRetryPassSkippedWhenCrawlerFailsToStart(t *testing.T) {
	rec := &recorder{}
	o := makeOrchestrator(t, "pageURL\nrenderfail.com\n", "auto", func(opts *Options) {
		opts.Deps.DeepAvailable = func() bool { return true }
		opts.Deps.NewScraper = func(context.Context, ScraperOptions) (ScraperBackend, error) {
			return nil, os.ErrPermission
		}
	})
	o.processItems = rec.process
	items := loadItems(t, o)
	mark(t, o.progress, "renderfail.com", progress.StatusError, "Scraper returned empty content")
	var stack closers
	o.retryFailedWebsitesDeep(context.Background(), &stack, items)
	if len(rec.items) != 0 || o.scrapeMode != "direct" {
		t.Fatal("pass should have been skipped")
	}
}

func TestDeepRetrySwitchDisablesTheDeepPass(t *testing.T) {
	o := makeOrchestrator(t, "pageURL\nexample.com\n", "auto")
	o.cfg.DeepRetryEnabled = false
	if o.deepCrawlAvailable() {
		t.Fatal("deep_retry_enabled=false must disable the deep pass")
	}
	o.cfg.DeepRetryEnabled = true
	o.cfg.DeepScrapePodmanBinary = "definitely-not-a-real-podman-binary"
	if o.deepCrawlAvailable() {
		t.Fatal("a missing podman binary must disable the deep pass")
	}
}

// ---- dedupe ----

func TestDedupeOutputCSVPrefersSuccessRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "output.csv")
	writeFile(t, path, "Domain,Quality,Scrape_Mode\r\na.com,Failed,direct\r\nb.com,Premium,direct\r\na.com,Standard,deep\r\nc.com,Failed,direct\r\nc.com,Failed,deep\r\nb.com,Failed,research\r\n")
	dedupeOutputCSV(path)
	raw, _ := os.ReadFile(path)
	// The deep-crawl rescue replaces the Failed row; repeated failures
	// collapse to the most recent one; a later failure never replaces a
	// success; first-appearance order is kept.
	want := "Domain,Quality,Scrape_Mode\r\na.com,Standard,deep\r\nb.com,Premium,direct\r\nc.com,Failed,deep\r\n"
	if string(raw) != want {
		t.Fatalf("got %q", raw)
	}
	// Nothing to dedupe: the file is left byte-for-byte alone.
	writeFile(t, path, "Domain,Quality\na.com,Premium\n")
	dedupeOutputCSV(path)
	if raw, _ := os.ReadFile(path); string(raw) != "Domain,Quality\na.com,Premium\n" {
		t.Fatalf("rewritten: %q", raw)
	}
}

// ---- research pass (tests/test_research_fallback.py) ----

func TestOrchestratorResearchPassProcessesFailedWebsites(t *testing.T) {
	o := makeOrchestrator(t, "pageURL\nblocked.com\nfine.com\n", "auto")
	items := loadItems(t, o)
	mark(t, o.progress, "blocked.com", progress.StatusError, "Received HTTP status 401")
	mark(t, o.progress, "fine.com", progress.StatusSuccess, "")
	var mu sync.Mutex
	var researched []string
	o.researchDomain = func(_ context.Context, _ *DomainProcessor, domain string) {
		mu.Lock()
		defer mu.Unlock()
		researched = append(researched, domain)
	}
	o.researchFailedWebsites(context.Background(), items)
	if !slices.Equal(researched, []string{"blocked.com"}) {
		t.Fatalf("researched %v", researched)
	}

	researched = nil
	o.cfg.ResearchFallbackEnabled = false
	o.researchFailedWebsites(context.Background(), items)
	if len(researched) != 0 {
		t.Fatal("research ran while disabled")
	}
}

func TestResearchPassConcurrencyIsIndependentOfCrawlPool(t *testing.T) {
	var domains []string
	for n := range 10 {
		domains = append(domains, "site"+string(rune('a'+n))+".com")
	}
	o := makeOrchestrator(t, "pageURL\n"+strings.Join(domains, "\n")+"\n", "auto")
	items := loadItems(t, o)
	for _, d := range domains {
		mark(t, o.progress, d, progress.StatusError, "Received HTTP status 403")
	}
	o.cfg.ConcurrentSessions = 2
	o.cfg.ResearchMaxConcurrent = 6
	var mu sync.Mutex
	inFlight, peak := 0, 0
	o.researchDomain = func(context.Context, *DomainProcessor, string) {
		mu.Lock()
		inFlight++
		peak = max(peak, inFlight)
		mu.Unlock()
		time.Sleep(30 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
	}
	o.researchFailedWebsites(context.Background(), items)
	if peak != 6 {
		t.Fatalf("peak = %d", peak)
	}
}

// ---- output schema migration ----

func TestSetupOutputFileMigratesOldHeader(t *testing.T) {
	o := makeOrchestrator(t, "pageURL\nexample.com\n", "auto")
	var old []string
	for _, f := range config.CSVFieldnames {
		if f != "Bot_Protection" {
			old = append(old, f)
		}
	}
	row := make([]string, len(old))
	for i := range row {
		row[i] = "x"
	}
	writeFile(t, o.cfg.OutputCSVPath, strings.Join(old, ",")+"\r\n"+strings.Join(row, ",")+"\r\n")
	w, err := openResultsFile(o.cfg.OutputCSVPath, o.outputColumns(false))
	if err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	header, rows := readCSV(t, o.cfg.OutputCSVPath)
	if !slices.Equal(header, config.CSVFieldnames) || rows[0]["Bot_Protection"] != "" || rows[0]["Domain"] != "x" {
		t.Fatalf("header=%v rows=%v", header, rows)
	}
}

func TestResumeRenamesLegacyTypesafeColumns(t *testing.T) {
	spec := testSpec(t)
	for _, ctv := range []bool{false, true} {
		o := makeOrchestrator(t, "pageURL\nexample.com\n", "auto", func(opts *Options) {
			opts.CustomCategories = spec
			opts.Deps.NewCategories = func(*categories.Spec, string) (CategoryClassifier, error) { return &fakeCategories{}, nil }
		})
		base := config.CSVFieldnames
		if ctv {
			base = config.CTVCSVFieldnames
		}
		legacy := slices.Clone(base)
		for _, c := range categories.Columns(spec) {
			if !strings.HasPrefix(c, "Decision_") {
				legacy = append(legacy, c)
			}
		}
		legacy = append(legacy, "TypeSafe_Status", "TypeSafe_Error", "TypeSafe_Model", "TypeSafe_Answers")
		values := map[string]string{"Custom: Brand safe": "No", "TypeSafe_Status": "success", "TypeSafe_Model": "jev-1.13.0"}
		row := make([]string, len(legacy))
		for i, c := range legacy {
			row[i] = values[c]
		}
		writeFile(t, o.cfg.OutputCSVPath, strings.Join(legacy, ",")+"\r\n"+strings.Join(row, ",")+"\r\n")
		w, err := openResultsFile(o.cfg.OutputCSVPath, o.outputColumns(ctv))
		if err != nil {
			t.Fatal(err)
		}
		_ = w.Close()
		header, rows := readCSV(t, o.cfg.OutputCSVPath)
		if !slices.Equal(header, o.outputColumns(ctv)) || rows[0]["Decision_Status"] != "success" ||
			rows[0]["Decision_Model"] != "jev-1.13.0" || rows[0]["Decision_Provider"] != "" || rows[0]["Custom: Brand safe"] != "No" {
			t.Fatalf("ctv=%v header=%v rows=%v", ctv, header, rows)
		}
	}
}

// ---- custom-category binding (tests/test_custom_categories.py) ----

func categoryOpts(spec *categories.Spec) func(*Options) {
	return func(opts *Options) {
		opts.CustomCategories = spec
		opts.Deps.NewCategories = func(*categories.Spec, string) (CategoryClassifier, error) { return &fakeCategories{}, nil }
	}
}

func TestCSVResumeDefinitionsAndDisabledRunAreIsolated(t *testing.T) {
	spec := testSpec(t)
	for _, ctv := range []bool{false, true} {
		cfg := testConfig(t)
		newRun := func(s *categories.Spec) *Orchestrator {
			o, err := New(Options{Config: cfg, Out: &bytes.Buffer{}, CustomCategories: s,
				Deps: Deps{NewCategories: func(*categories.Spec, string) (CategoryClassifier, error) { return &fakeCategories{}, nil }}})
			if err != nil {
				t.Fatal(err)
			}
			return o
		}
		o := newRun(spec)
		if err := o.prepareCustomCategories(); err != nil {
			t.Fatal(err)
		}
		w, err := openResultsFile(cfg.OutputCSVPath, o.outputColumns(ctv))
		if err != nil {
			t.Fatal(err)
		}
		rec := NewRecord()
		rec.Set("Quality", "Premium")
		rec.Set("Custom: Brand safe", "No")
		if err := w.WriteRow(rec); err != nil {
			t.Fatal(err)
		}
		_ = w.Close()
		original, _ := os.ReadFile(cfg.OutputCSVPath)
		sidecar, _ := os.ReadFile(cfg.OutputCSVPath + ".categories.json")
		if string(sidecar) != spec.PythonJSON(2) {
			t.Fatalf("sidecar = %s", sidecar)
		}
		var saved map[string]any
		raw, _ := os.ReadFile(cfg.ProgressFilePath)
		if err := json.Unmarshal(raw, &saved); err != nil || saved["custom_categories"] == nil {
			t.Fatalf("progress file lacks custom_categories: %s", raw)
		}

		resumed := newRun(spec)
		if err := resumed.prepareCustomCategories(); err != nil {
			t.Fatal(err)
		}
		w, err = openResultsFile(cfg.OutputCSVPath, resumed.outputColumns(ctv))
		if err != nil {
			t.Fatal(err)
		}
		_ = w.Close()
		if now, _ := os.ReadFile(cfg.OutputCSVPath); !bytes.Equal(now, original) {
			t.Fatal("resume rewrote the output")
		}

		changed, err := categories.Parse(strings.Replace(spec.PythonJSON(0), "Would a mainstream brand be comfortable appearing beside this content?", "Different rubric?", 1))
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range []*categories.Spec{changed, nil} {
			err := newRun(s).prepareCustomCategories()
			if err == nil || !(strings.Contains(err.Error(), "changed") || strings.Contains(err.Error(), "different")) {
				t.Fatalf("spec %v: err = %v", s, err)
			}
		}
		if now, _ := os.ReadFile(cfg.OutputCSVPath); !bytes.Equal(now, original) {
			t.Fatal("a refused run touched the output")
		}
	}
}

func TestEnablingCategoriesOnExistingPlainOutputRequiresNewPaths(t *testing.T) {
	spec := testSpec(t)
	o := makeOrchestrator(t, "Domain\nexample.com\n", "direct", categoryOpts(spec))
	original := "Domain,Quality\nexample.com,Premium\n"
	writeFile(t, o.cfg.OutputCSVPath, original)
	err := o.prepareCustomCategories()
	if err == nil || !strings.Contains(err.Error(), "different") {
		t.Fatalf("err = %v", err)
	}
	if raw, _ := os.ReadFile(o.cfg.OutputCSVPath); string(raw) != original {
		t.Fatal("output touched")
	}
}

func TestProgressWithEntriesButNoDefinitionsRefusesCategories(t *testing.T) {
	o := makeOrchestrator(t, "Domain\nexample.com\n", "direct", categoryOpts(testSpec(t)))
	mark(t, o.progress, "example.com", progress.StatusSuccess, "")
	if err := o.prepareCustomCategories(); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("err = %v", err)
	}
}

// ---- full runs ----

// runDeps wires fakes for a whole run.
type runDeps struct {
	llm      *fakeLLM
	scrapers *scraperFactory
	ios      *fakeStore
	android  *fakeStore
}

func newRunDeps() *runDeps {
	return &runDeps{
		llm:      newFakeLLM(),
		scrapers: &scraperFactory{},
		ios:      &fakeStore{meta: AppMetadata{Success: true, AppName: "Demo", ContentForLLM: "Store source text"}},
		android:  &fakeStore{meta: AppMetadata{Success: true, AppName: "Droid", ContentForLLM: "Play source text"}},
	}
}

func (d *runDeps) deps() Deps {
	return Deps{
		NewLLM:        func(context.Context, LLMOptions) (LLM, error) { return d.llm, nil },
		NewScraper:    d.scrapers.new,
		NewIOS:        func(context.Context) (StoreClient, error) { return d.ios, nil },
		NewAndroid:    func(context.Context) (StoreClient, error) { return d.android, nil },
		DeepAvailable: func() bool { return false },
	}
}

func goodPage(mode string) ScrapeResult {
	content := strings.Repeat("Useful source text. ", 100)
	return ScrapeResult{Success: true, Content: content, ContentLength: len(content), StatusCode: 200, Mode: mode}
}

func TestFullOrchestratorRunWiresOptionalClientAndResumes(t *testing.T) {
	spec := testSpec(t)
	for _, ctv := range []bool{false, true} {
		cfg := testConfig(t)
		cfg.CTVRequestDelay = 0
		input := "Domain\nexample.com\n123456789\n"
		if ctv {
			input = "app_name,bundle_id\nDemo TV,com.demo.tv\n"
		}
		writeFile(t, cfg.InputCSVPath, input)
		d := newRunDeps()
		d.scrapers.make = func(o ScraperOptions) *fakeScraper {
			return &fakeScraper{mode: o.Mode, scrape: func(string) ScrapeResult { return goodPage(o.Mode) }}
		}
		d.llm.researchCTV = func(CTVResearchInput) (Research, error) {
			return Research{Success: true, ResearchContent: "CTV source text"}, nil
		}
		custom := &fakeCategories{result: decisionSuccess()}
		deps := d.deps()
		deps.NewCategories = func(*categories.Spec, string) (CategoryClassifier, error) { return custom, nil }
		run := func() {
			o, err := New(Options{Config: cfg, Quiet: true, ScrapeMode: "direct", CTVMode: ctv, CustomCategories: spec, Deps: deps, Out: &bytes.Buffer{}})
			if err != nil {
				t.Fatal(err)
			}
			if err := o.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		run()
		_, rows := readCSV(t, cfg.OutputCSVPath)
		want := 2
		if ctv {
			want = 1
		}
		if len(rows) != want || len(custom.calls) != want {
			t.Fatalf("ctv=%v rows=%d classify=%d", ctv, len(rows), len(custom.calls))
		}
		for _, row := range rows {
			if row["Custom: Purpose"] != "Education" || row["Decision_Status"] != "success" || row["Quality"] != "Premium" {
				t.Fatalf("row = %v", row)
			}
		}
		before, _ := os.ReadFile(cfg.OutputCSVPath)
		// A new process using identical definitions skips completed items.
		run()
		if after, _ := os.ReadFile(cfg.OutputCSVPath); !bytes.Equal(before, after) {
			t.Fatal("resumed run changed the output")
		}
		if len(custom.calls) != want {
			t.Fatal("resumed run re-classified")
		}
	}
}

func TestAutoRunLadderDedupesAndFinalizes(t *testing.T) {
	cfg := testConfig(t)
	writeFile(t, cfg.InputCSVPath, "Domain\nok.com\njsonly.com\nblocked.com\nnowhere.com\n333903271\ncom.spotify.music\n")
	d := newRunDeps()
	d.scrapers.make = func(o ScraperOptions) *fakeScraper {
		return &fakeScraper{mode: o.Mode, scrape: func(domain string) ScrapeResult {
			switch {
			case domain == "ok.com", domain == "jsonly.com" && o.Mode == "deep":
				return goodPage(o.Mode)
			case domain == "jsonly.com":
				return ScrapeResult{Success: true, Content: "tiny", ContentLength: 4, StatusCode: 200, Mode: o.Mode}
			default:
				return ScrapeResult{Error: "Received HTTP status 403", Mode: o.Mode}
			}
		}}
	}
	d.llm.researchSite = func(domain string, _ ResearchOptions) (Research, error) {
		if domain == "nowhere.com" {
			return Research{Success: true}, nil
		}
		return Research{Success: true, ResearchContent: "Blocked is a well known publisher with a large audience."}, nil
	}
	deps := d.deps()
	deps.DeepAvailable = func() bool { return true }
	var out bytes.Buffer
	o, err := New(Options{Config: cfg, Quiet: true, ScrapeMode: "auto", RejectRedirects: true, Deps: deps, Out: &out})
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(d.scrapers.built, []ScraperOptions{{Mode: "direct", RejectRedirects: true}, {Mode: "deep", RejectRedirects: false}}) {
		t.Fatalf("scrapers = %v", d.scrapers.built)
	}
	// blocked.com/nowhere.com are anti-bot blocks: never sent to the deep pass.
	if got := d.scrapers.last.scraped; !slices.Equal(got, []string{"jsonly.com"}) {
		t.Fatalf("deep pass scraped %v", got)
	}
	if !slices.Equal(d.llm.researchCalls, []string{"blocked.com", "nowhere.com"}) && !slices.Equal(d.llm.researchCalls, []string{"nowhere.com", "blocked.com"}) {
		t.Fatalf("research calls = %v", d.llm.researchCalls)
	}
	header, rows := readCSV(t, cfg.OutputCSVPath)
	if !slices.Equal(header, config.CSVFieldnames) {
		t.Fatalf("header = %v", header)
	}
	byID := map[string]map[string]string{}
	var order []string
	for _, r := range rows {
		byID[r["Domain"]] = r
		order = append(order, r["Domain"])
	}
	if len(rows) != 6 {
		t.Fatalf("rows not deduped: %v", order)
	}
	checks := map[string][3]string{ // Quality, Scrape_Mode, Bot_Protection
		"ok.com":            {"Premium", "direct", "None Detected"},
		"jsonly.com":        {"Premium", "deep", "None Detected"},
		"blocked.com":       {"Premium", "research", "Moderate"},
		"nowhere.com":       {"Failed", "research", "Moderate"},
		"333903271":         {"Premium", "ios_api", ""},
		"com.spotify.music": {"Premium", "android_scrape", ""},
	}
	for id, want := range checks {
		r := byID[id]
		if r["Quality"] != want[0] || r["Scrape_Mode"] != want[1] || r["Bot_Protection"] != want[2] {
			t.Errorf("%s: %s/%s/%s, want %v", id, r["Quality"], r["Scrape_Mode"], r["Bot_Protection"], want)
		}
	}
	s := o.progress.Summary()
	if s.Total != 6 || s.Processed != 6 || s.Successful != 5 || s.Errors != 1 || s.Retrying != 0 {
		t.Fatalf("summary = %+v", s)
	}
	if !strings.Contains(out.String(), "--- Lens run complete ---") || !strings.Contains(out.String(), "📊 Progress Summary:") {
		t.Fatalf("output = %s", out.String())
	}
	if d.llm.connectionRuns != 1 {
		t.Fatal("connection test must run once")
	}
}

func TestAutoRunFinalizesPendingWhenNoLaterPassRuns(t *testing.T) {
	cfg := testConfig(t)
	cfg.ResearchFallbackEnabled = false
	writeFile(t, cfg.InputCSVPath, "Domain\nbad.com\n")
	d := newRunDeps()
	d.scrapers.make = func(o ScraperOptions) *fakeScraper {
		return &fakeScraper{mode: o.Mode, scrape: func(string) ScrapeResult { return ScrapeResult{Error: "timeout"} }}
	}
	deps := d.deps()
	deps.DeepAvailable = func() bool { return true }
	deps.NewScraper = func(ctx context.Context, opts ScraperOptions) (ScraperBackend, error) {
		if opts.Mode == "deep" {
			return nil, os.ErrNotExist // Chrome container failed to start
		}
		return d.scrapers.new(ctx, opts)
	}
	o, err := New(Options{Config: cfg, Quiet: true, ScrapeMode: "auto", Deps: deps, Out: &bytes.Buffer{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := o.progress.Summary()
	if s.Retrying != 0 || s.Errors != 1 || s.CompletionPercent != 100 {
		t.Fatalf("summary = %+v", s)
	}
}

func TestRunFailsWhenConnectionTestFails(t *testing.T) {
	cfg := testConfig(t)
	writeFile(t, cfg.InputCSVPath, "Domain\nexample.com\n")
	d := newRunDeps()
	d.llm.connectionOK = false
	o, err := New(Options{Config: cfg, Quiet: true, ScrapeMode: "direct", Deps: d.deps(), Out: &bytes.Buffer{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "Failed to initialise OpenRouter client") {
		t.Fatalf("err = %v", err)
	}
}

func TestCTVRunWritesCTVSchema(t *testing.T) {
	cfg := testConfig(t)
	cfg.CTVRequestDelay = 0
	writeFile(t, cfg.InputCSVPath, ctvCSV)
	d := newRunDeps()
	var gotOpts LLMOptions
	deps := d.deps()
	deps.NewLLM = func(_ context.Context, opts LLMOptions) (LLM, error) {
		gotOpts = opts
		return d.llm, nil
	}
	o, err := New(Options{Config: cfg, Quiet: true, ScrapeMode: "auto", Deps: deps, Out: &bytes.Buffer{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	header, rows := readCSV(t, cfg.OutputCSVPath)
	if !slices.Equal(header, config.CTVCSVFieldnames) || len(rows) != 1 || rows[0]["Bundle_ID"] != "512001" ||
		rows[0]["Platform"] != "Roku" || rows[0]["Publisher"] != "Northwind Broadcasting" || rows[0]["SSP"] != "ExampleSSP" {
		t.Fatalf("header=%v rows=%v", header, rows)
	}
	if gotOpts.Model != cfg.CTVClassificationModel || gotOpts.MaxTokens != cfg.CTVClassificationMaxTokens || gotOpts.FallbackModel != cfg.CTVClassificationFallback {
		t.Fatalf("CTV client options = %+v", gotOpts)
	}
	if !o.progress.IsProcessed("512001") {
		t.Fatal("not processed")
	}
}

// ---- cost bookkeeping ----

func TestRunMeterRestoredSavedAndLogged(t *testing.T) {
	cfg := testConfig(t)
	writeFile(t, cfg.InputCSVPath, "Domain\nexample.com\n")
	writeFile(t, cfg.ProgressFilePath, `{"processed_domains": {}, "cost": {"usd": 0.5, "calls": 4, "unpriced_calls": 1, "by_model": {"m": 0.5}}}`)
	d := newRunDeps()
	d.scrapers.make = func(o ScraperOptions) *fakeScraper {
		return &fakeScraper{mode: o.Mode, scrape: func(string) ScrapeResult { return goodPage(o.Mode) }}
	}
	deps := d.deps()
	deps.NewLLM = func(context.Context, LLMOptions) (LLM, error) { return costingFullLLM{d.llm}, nil }
	var logs bytes.Buffer
	logx.SetOutput(&logs, 0)
	defer logx.SetOutput(&bytes.Buffer{}, 8)
	o, err := New(Options{Config: cfg, Quiet: true, ScrapeMode: "direct", Deps: deps, Out: &bytes.Buffer{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Cost struct {
			USD   float64 `json:"usd"`
			Calls int     `json:"calls"`
		} `json:"cost"`
		Processed map[string]struct {
			Result map[string]any `json:"result"`
		} `json:"processed_domains"`
	}
	raw, _ := os.ReadFile(cfg.ProgressFilePath)
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	// Restored 4 calls + the connection test + one classification.
	if saved.Cost.Calls != 6 || saved.Cost.USD != 0.52 {
		t.Fatalf("cost = %+v", saved.Cost)
	}
	if got := saved.Processed["example.com"].Result["Cost_USD"]; got != "0.010000" {
		t.Fatalf("progress result Cost_USD = %v", got)
	}
	if !strings.Contains(logs.String(), "OpenRouter spend for this run: $0.5200 over 6 calls (1 without a reported cost)") {
		t.Fatalf("logs = %s", logs.String())
	}
	_, rows := readCSV(t, cfg.OutputCSVPath)
	if rows[0]["Cost_USD"] != "0.010000" {
		t.Fatalf("row cost = %v", rows[0]["Cost_USD"])
	}
}

// costingFullLLM bills 0.01 per call, like OpenRouter's usage.cost.
type costingFullLLM struct{ *fakeLLM }

func bill(ctx context.Context) {
	v := 0.01
	cost.Record(ctx, &v, "m")
}

func (c costingFullLLM) TestConnection(ctx context.Context) bool {
	bill(ctx)
	return c.fakeLLM.TestConnection(ctx)
}

func (c costingFullLLM) ClassifySite(ctx context.Context, in SiteInput) (Classification, error) {
	bill(ctx)
	return c.fakeLLM.ClassifySite(ctx, in)
}

// ---- graceful shutdown ----

func TestCancelStopsDispatchAndKeepsBookkeeping(t *testing.T) {
	cfg := testConfig(t)
	cfg.ConcurrentSessions = 1
	var domains []string
	for n := range 20 {
		domains = append(domains, "site"+string(rune('a'+n))+".com")
	}
	writeFile(t, cfg.InputCSVPath, "Domain\n"+strings.Join(domains, "\n")+"\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := newRunDeps()
	var mu sync.Mutex
	count := 0
	d.scrapers.make = func(o ScraperOptions) *fakeScraper {
		return &fakeScraper{mode: o.Mode, scrape: func(string) ScrapeResult {
			mu.Lock()
			count++
			if count == 3 {
				cancel() // SIGINT arrives while the third item is in flight
			}
			mu.Unlock()
			return goodPage(o.Mode)
		}}
	}
	o, err := New(Options{Config: cfg, Quiet: true, ScrapeMode: "auto", Deps: d.deps(), Out: &bytes.Buffer{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Run(ctx); err != nil {
		t.Fatal(err)
	}
	_, rows := readCSV(t, cfg.OutputCSVPath)
	if len(rows) < 2 || len(rows) > 3 || count > 3 {
		t.Fatalf("rows=%d scraped=%d", len(rows), count)
	}
	if s := o.progress.Summary(); s.Processed != len(rows) {
		t.Fatalf("summary = %+v rows=%d", s, len(rows))
	}
	if !d.scrapers.last.closed {
		t.Fatal("scraper not closed on shutdown")
	}
}
