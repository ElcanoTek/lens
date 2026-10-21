// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ElcanoTek/lens/internal/categories"
	"github.com/ElcanoTek/lens/internal/config"
	"github.com/ElcanoTek/lens/internal/cost"
	"github.com/ElcanoTek/lens/internal/item"
	"github.com/ElcanoTek/lens/internal/logx"
	"github.com/ElcanoTek/lens/internal/progress"
	"github.com/ElcanoTek/lens/internal/report"
	"github.com/ElcanoTek/lens/internal/scrape"
)

var logger = logx.New("orchestration")

// Deps builds the external clients. A nil field means the real client
// built from config.Config (adapters.go); tests pass fakes. Every factory is
// called at most once per pass, with the run's context; a returned value
// that implements io.Closer is closed when the run's work is done (the
// Python AsyncExitStack).
type Deps struct {
	NewLLM        func(ctx context.Context, opts LLMOptions) (LLM, error)
	NewScraper    func(ctx context.Context, opts ScraperOptions) (ScraperBackend, error)
	NewIOS        func(ctx context.Context) (StoreClient, error)
	NewAndroid    func(ctx context.Context) (StoreClient, error)
	NewCategories func(spec *categories.Spec, model string) (CategoryClassifier, error)
	// DeepAvailable overrides the deep-crawl availability probe (default:
	// deep_retry_enabled and the podman binary is on PATH).
	DeepAvailable func() bool
}

// Options configures an [Orchestrator].
type Options struct {
	Config    *config.Config
	Quiet     bool
	Verbose   bool
	JSONLPath string
	// ScrapeMode is auto, direct or deep; "" means Config.ScrapeMode.
	ScrapeMode      string
	RejectRedirects bool
	CTVMode         bool
	// CustomCategories enables the optional custom-category columns.
	CustomCategories *categories.Spec
	Deps             Deps
	// Out receives the progress lines and summaries (default os.Stdout).
	Out io.Writer
}

// Orchestrator is the site-analysis workflow (SiteAnalysisOrchestrator):
// load the input, route websites / iOS apps / Android apps / CTV apps to
// their processors with bounded concurrency, run auto mode's retry ladder
// (direct -> deep -> research) and keep the output CSV and progress file
// resumable.
type Orchestrator struct {
	cfg       *config.Config
	deps      Deps
	out       io.Writer
	quiet     bool
	verbose   bool
	jsonlPath string

	spec       *categories.Spec
	categories CategoryClassifier
	progress   *progress.Tracker
	reporter   *report.Reporter

	// scrapeMode is the current pass's backend. Auto mode starts at direct.
	scrapeMode      string
	autoMode        bool
	rejectRedirects bool
	ctvMode         bool
	// deferWebsiteFailures is set per pass: earlier-pass website failures
	// defer to a later pass (status retry_pending) only when that pass can
	// actually run.
	deferWebsiteFailures bool

	llm     LLM
	scraper ScraperBackend
	ios     StoreClient
	android StoreClient
	writer  *ResultsWriter

	hasWebsites, hasIOSApps, hasAndroidApps bool
	isCTVInput, hasCTVApps                  bool
	ctvItems                                []item.CTVWorkItem

	// Seams the tests replace (the Python tests monkeypatched the
	// corresponding methods).
	processItems   func(ctx context.Context, items []item.WorkItem)
	researchDomain func(ctx context.Context, p *DomainProcessor, domain string)
}

// New validates the options and opens the progress file.
func New(opts Options) (*Orchestrator, error) {
	cfg := opts.Config
	if cfg == nil {
		cfg = config.Default()
	}
	mode := strings.ToLower(opts.ScrapeMode)
	if mode == "" {
		mode = strings.ToLower(cfg.ScrapeMode)
	}
	if mode != "auto" && mode != "direct" && mode != "deep" {
		return nil, fmt.Errorf("Unsupported scrape mode: %s", mode) //nolint:staticcheck // message kept from the Python release
	}
	o := &Orchestrator{
		cfg:             cfg,
		deps:            opts.Deps,
		out:             opts.Out,
		quiet:           opts.Quiet,
		verbose:         opts.Verbose,
		jsonlPath:       opts.JSONLPath,
		scrapeMode:      mode,
		rejectRedirects: opts.RejectRedirects,
		ctvMode:         opts.CTVMode,
		progress:        progress.Open(cfg.ProgressFilePath),
	}
	if o.out == nil {
		o.out = os.Stdout
	}
	// Auto mode: fast crawl first, deeper rungs only for websites that fail.
	if mode == "auto" {
		o.autoMode = true
		o.scrapeMode = "direct"
	}
	if opts.CustomCategories != nil {
		spec, err := categories.Validate(opts.CustomCategories)
		if err != nil {
			return nil, err
		}
		o.spec = spec
		newCategories := o.deps.NewCategories
		if newCategories == nil {
			newCategories = defaultNewCategories
		}
		client, err := newCategories(spec, cfg.DecisionModel)
		if err != nil {
			return nil, err
		}
		o.categories = client
	}
	o.processItems = o.processItemsConcurrently
	o.researchDomain = func(ctx context.Context, p *DomainProcessor, domain string) {
		p.ProcessDomainResearch(ctx, domain)
	}
	return o, nil
}

// Progress returns the run's progress tracker.
func (o *Orchestrator) Progress() *progress.Tracker { return o.progress }

// closers is the Go stand-in for the Python AsyncExitStack.
type closers []func()

func (c *closers) push(v any) {
	if cl, ok := v.(io.Closer); ok && cl != nil {
		*c = append(*c, func() { _ = cl.Close() })
	}
}

func (c *closers) closeAll() {
	for i := len(*c) - 1; i >= 0; i-- {
		(*c)[i]()
	}
	*c = nil
}

// Run executes the workflow. Cancelling ctx (SIGINT/SIGTERM in `lens run`)
// is the graceful shutdown: no new items start, in-flight items stop without
// recording a result, and the progress/CSV bookkeeping still completes, so
// Run then returns nil.
func (o *Orchestrator) Run(ctx context.Context) (err error) {
	// A progress checkpoint that cannot be written stops the run, like a
	// SIGINT: items in flight record nothing, and the run fails. Carrying on
	// would leave outcomes that a resume cannot see, so it would process,
	// bill and write those items a second time.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		checkpointOnce sync.Once
		checkpointErr  error
	)
	o.progress.OnSaveError = func(err error) {
		checkpointOnce.Do(func() {
			checkpointErr = err
			logger.Error(fmt.Sprintf("Could not save progress to %s (%v); stopping the run so no item is processed twice on resume", o.progress.Path(), err))
			cancel()
		})
	}
	defer func() {
		if checkpointErr != nil {
			err = fmt.Errorf("progress checkpoint failed: %w", checkpointErr)
		}
	}()

	// Resumed runs keep counting from the spend already recorded.
	var saved any
	if raw := o.progress.Extra("cost"); raw != nil {
		_ = json.Unmarshal(raw, &saved)
	}
	meter := cost.Restore(saved)
	o.progress.Cost = meter
	ctx = cost.WithRun(ctx, meter)
	callsAtStart := meter.Calls()
	defer func() {
		if meter.Calls() == callsAtStart {
			return
		}
		// Calls made after the last item save (or with no items at all,
		// like the connection test) still belong to the run.
		_ = o.progress.Save()
		unpriced := ""
		if n := meter.UnpricedCalls(); n > 0 {
			unpriced = fmt.Sprintf(" (%d without a reported cost)", n)
		}
		logger.Info(fmt.Sprintf("OpenRouter spend for this run: $%.4f over %d calls%s", meter.USD(), meter.Calls(), unpriced))
	}()

	if err := o.run(ctx); err != nil {
		logger.Error(fmt.Sprintf("Fatal error in main workflow: %v", err))
		return err
	}
	return nil
}

func (o *Orchestrator) run(ctx context.Context) error {
	if err := o.prepareCustomCategories(); err != nil {
		return err
	}
	if o.ctvMode {
		return o.runCTVWorkflow(ctx)
	}

	runStart := time.Now()
	logger.Info("Starting Elcano Lens Site Analysis")
	logger.Info(fmt.Sprintf("Configuration: %s with %d concurrent sessions", o.cfg.LLMModel, o.cfg.ConcurrentSessions))
	logger.Info("Scrape mode: " + o.scrapeMode)

	items, err := o.loadInputData()
	if err != nil {
		return err
	}
	if o.isCTVInput {
		return o.runCTVWorkflow(ctx)
	}

	if err := o.startReporter(len(items)); err != nil {
		return err
	}
	defer o.reporter.Stop()

	if len(items) == 0 {
		logger.Error("No valid items to process")
		o.finishSummary(runStart)
		return nil
	}
	var toProcess []item.WorkItem
	for _, it := range items {
		if !o.progress.IsProcessed(it.Identifier) {
			toProcess = append(toProcess, it)
		}
	}
	if len(toProcess) == 0 {
		logger.Info("All items have already been processed!")
		o.finishSummary(runStart)
		return nil
	}
	logger.Info(fmt.Sprintf("Processing %d items (skipping %d already processed)", len(toProcess), len(items)-len(toProcess)))

	if err := o.processAll(ctx, toProcess); err != nil {
		return err
	}
	if o.autoMode {
		dedupeOutputCSV(o.cfg.OutputCSVPath)
	}
	o.finishSummary(runStart)
	logger.Info("Site analysis completed successfully!")
	return nil
}

// processAll is the body of the Python `async with AsyncExitStack()` block.
func (o *Orchestrator) processAll(ctx context.Context, toProcess []item.WorkItem) error {
	var stack closers
	defer stack.closeAll()
	if err := o.initializeClients(ctx, &stack); err != nil {
		return err
	}
	writer, err := openResultsFile(o.cfg.OutputCSVPath, o.outputColumns(false))
	if err != nil {
		return err
	}
	o.writer = writer
	defer o.teardownOutputFile()

	o.deferWebsiteFailures = o.autoMode && o.hasWebsites && (o.deepCrawlAvailable() || o.researchFallbackAvailable())
	o.processItems(ctx, toProcess)
	if o.autoMode && ctx.Err() == nil {
		o.retryFailedWebsitesDeep(ctx, &stack, toProcess)
	}
	if o.autoMode && ctx.Err() == nil {
		o.researchFailedWebsites(ctx, toProcess)
	}
	if o.autoMode {
		// Anything still pending (deep crawler failed to start, run
		// interrupted) becomes a terminal error so progress can reach 100%.
		_, _ = o.progress.FinalizePendingRetries()
	}
	return nil
}

func (o *Orchestrator) startReporter(itemCount int) error {
	before := o.progress.Summary()
	total := before.Total
	if total == 0 {
		total = itemCount
	}
	o.reporter = report.New(report.Options{
		Total:     total,
		Completed: before.Processed,
		Successes: before.Successful,
		Failures:  before.Errors,
		Quiet:     o.quiet,
		Verbose:   o.verbose,
		JSONLPath: o.jsonlPath,
		Out:       o.out,
	})
	return o.reporter.Start()
}

func (o *Orchestrator) finishSummary(runStart time.Time) {
	s := o.progress.Summary()
	s.Print(o.out)
	o.reporter.Summary(s, time.Since(runStart))
}

func (o *Orchestrator) teardownOutputFile() {
	if o.writer != nil {
		_ = o.writer.Close()
	}
	o.writer = nil
}

func (o *Orchestrator) outputColumns(ctv bool) []string {
	base := config.CSVFieldnames
	if ctv {
		base = config.CTVCSVFieldnames
	}
	return append(slices.Clone(base), categories.Columns(o.spec)...)
}

// specValue is the spec as a decoded JSON value (nil for none), so it can
// be compared with what an earlier run stored, like the Python dict
// comparison.
func (o *Orchestrator) specValue() any {
	if o.spec == nil {
		return nil
	}
	var v any
	_ = json.Unmarshal([]byte(o.spec.PythonJSON(0)), &v)
	return v
}

// prepareCustomCategories binds resumable output to its original
// definitions; a run never mixes rubrics.
func (o *Orchestrator) prepareCustomCategories() error {
	current := o.specValue()
	var previous any
	if raw := o.progress.Extra("custom_categories"); raw != nil {
		_ = json.Unmarshal(raw, &previous)
	}
	if (o.progress.HasProcessedEntries() || truthy(previous)) && !reflect.DeepEqual(previous, current) {
		return errors.New("Custom categories changed. Use new output and progress files for a new categorization run.") //nolint:revive,staticcheck // message kept from the Python release
	}
	output := o.cfg.OutputCSVPath
	sidecar := output + ".categories.json"
	if st, err := os.Stat(output); err == nil && st.Size() > 0 {
		var existing any
		if raw, err := os.ReadFile(sidecar); err == nil { //nolint:gosec // sidecar of the operator-chosen output
			if err := json.Unmarshal(raw, &existing); err != nil {
				return fmt.Errorf("reading %s: %w", sidecar, err)
			}
		}
		if !reflect.DeepEqual(existing, current) {
			return errors.New("Output belongs to different custom categories. Use new output and progress files.") //nolint:revive,staticcheck // message kept from the Python release
		}
	}
	if o.spec == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil { //nolint:gosec // managed-files is world-readable by design
		return err
	}
	if err := os.WriteFile(sidecar, []byte(o.spec.PythonJSON(2)), 0o644); err != nil { //nolint:gosec // read by the dashboard
		return err
	}
	if err := o.progress.SetExtra("custom_categories", json.RawMessage(o.spec.PythonJSON(0))); err != nil {
		return err
	}
	if err := o.progress.EnsureDir(); err != nil {
		return err
	}
	return o.progress.Save()
}

// truthy is Python truthiness for a decoded JSON value.
func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case float64:
		return t != 0
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	}
	return true
}

func (o *Orchestrator) llmFactory() func(context.Context, LLMOptions) (LLM, error) {
	if o.deps.NewLLM != nil {
		return o.deps.NewLLM
	}
	return defaultNewLLM
}

func (o *Orchestrator) scraperFactory() func(context.Context, ScraperOptions) (ScraperBackend, error) {
	if o.deps.NewScraper != nil {
		return o.deps.NewScraper
	}
	return func(ctx context.Context, opts ScraperOptions) (ScraperBackend, error) {
		return defaultNewScraper(ctx, o.cfg, opts)
	}
}

// initializeClients builds the OpenRouter client (always), the website
// scraper, and the iOS / Android store clients the input needs.
func (o *Orchestrator) initializeClients(ctx context.Context, stack *closers) error {
	logger.Info("Initializing API clients...")
	llm, err := o.llmFactory()(ctx, LLMOptions{
		APIKey:        o.cfg.OpenRouterAPIKey,
		Model:         o.cfg.LLMModel,
		Temperature:   o.cfg.LLMTemperature,
		MaxTokens:     o.cfg.LLMMaxTokens,
		MaxRetries:    o.cfg.LLMRequestRetries,
		Timeout:       o.cfg.LLMRequestTimeout,
		FallbackModel: o.cfg.LLMFallbackModel,
	})
	if err != nil {
		return err
	}
	stack.push(llm)
	o.llm = llm

	logger.Info("Testing OpenRouter API connection...")
	if !llm.TestConnection(ctx) {
		return errors.New("Failed to initialise OpenRouter client after retry attempts. " + //nolint:staticcheck // message kept from the Python release
			"Check network connectivity and OPENROUTER_API_KEY configuration.")
	}

	if o.hasWebsites {
		logger.Info("Initializing website scraper...")
		scraper, err := o.scraperFactory()(ctx, ScraperOptions{Mode: o.scrapeMode, RejectRedirects: o.rejectRedirects})
		if err != nil {
			return err
		}
		stack.push(scraper)
		if err := scraper.CreateSessionPool(ctx, o.cfg.ConcurrentSessions); err != nil {
			return err
		}
		o.scraper = scraper
	}
	if o.hasIOSApps {
		logger.Info("Initializing iOS API client...")
		newIOS := o.deps.NewIOS
		if newIOS == nil {
			newIOS = func(ctx context.Context) (StoreClient, error) { return defaultNewIOS(ctx, o.cfg) }
		}
		client, err := newIOS(ctx)
		if err != nil {
			return err
		}
		stack.push(client)
		o.ios = client
	}
	if o.hasAndroidApps {
		logger.Info("Initializing Android scraper...")
		newAndroid := o.deps.NewAndroid
		if newAndroid == nil {
			newAndroid = func(ctx context.Context) (StoreClient, error) { return defaultNewAndroid(ctx, o.cfg) }
		}
		client, err := newAndroid(ctx)
		if err != nil {
			return err
		}
		stack.push(client)
		o.android = client
	}
	logger.Info("✅ All required clients initialised successfully")
	return nil
}

func (o *Orchestrator) domainProcessor(deferFailures bool) *DomainProcessor {
	return &DomainProcessor{
		Config:        o.cfg,
		Progress:      o.progress,
		Scraper:       o.scraper,
		Classifier:    o.llm,
		Reporter:      o.reporter,
		Writer:        o.writer,
		Categories:    o.categories,
		DeferFailures: deferFailures,
	}
}

// guard runs fn, logging a panic the way the Python orchestrator logged a
// task that raised, so one bad item never takes the run down.
func guard(format, id string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			if id == "" {
				logger.Error(fmt.Sprintf(format, r))
			} else {
				logger.Error(fmt.Sprintf(format, id, r))
			}
		}
	}()
	fn()
}

// runBounded runs fn for every element with at most limit in flight,
// dispatching in order, and stops dispatching once ctx is cancelled.
// It returns whether dispatch was cut short.
func runBounded[T any](ctx context.Context, limit int, elems []T, fn func(T)) bool {
	sem := make(chan struct{}, max(1, limit))
	var wg sync.WaitGroup
	defer wg.Wait()
	for _, e := range elems {
		select {
		case <-ctx.Done():
			return true
		case sem <- struct{}{}:
		}
		if ctx.Err() != nil {
			<-sem
			return true
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			fn(e)
		}()
	}
	return false
}

// processItemsConcurrently processes items with at most concurrent_sessions
// in flight, routed by content type.
func (o *Orchestrator) processItemsConcurrently(ctx context.Context, items []item.WorkItem) {
	var domains *DomainProcessor
	if o.hasWebsites && o.scraper != nil {
		domains = o.domainProcessor(o.deferWebsiteFailures)
	}
	var apps *AppProcessor
	if o.hasIOSApps || o.hasAndroidApps {
		apps = &AppProcessor{
			Progress:   o.progress,
			IOS:        o.ios,
			Android:    o.android,
			Classifier: o.llm,
			Reporter:   o.reporter,
			Writer:     o.writer,
			Categories: o.categories,
		}
	}
	cut := runBounded(ctx, o.cfg.ConcurrentSessions, items, func(it item.WorkItem) {
		if ctx.Err() != nil {
			return
		}
		guard("Task failed: %v", "", func() {
			switch {
			case it.Type == item.Website && domains != nil:
				domains.ProcessDomain(ctx, it.Identifier)
			case it.Type == item.Website:
				logger.Error("Website scraper not initialized for " + it.Identifier)
			case it.Type == item.IOSApp && apps != nil:
				apps.ProcessIOSApp(ctx, it.Identifier)
			case it.Type == item.IOSApp:
				logger.Error("iOS client not initialized for " + it.Identifier)
			case it.Type == item.AndroidApp && apps != nil:
				apps.ProcessAndroidApp(ctx, it.Identifier)
			case it.Type == item.AndroidApp:
				logger.Error("Android scraper not initialized for " + it.Identifier)
			default:
				logger.Error("Unknown content type for " + it.Identifier)
			}
		})
	})
	if cut {
		logger.Info("Shutdown requested, cancelling remaining tasks...")
	}
}

// collectRetryItems returns the websites whose previous pass failed
// (deferred or terminal), in input order.
func (o *Orchestrator) collectRetryItems(items []item.WorkItem) []item.WorkItem {
	// Deferred failures are tracked as retry_pending; include plain errors
	// too for resumed runs with older state.
	failed := map[string]bool{}
	for _, id := range o.progress.RetryPending() {
		failed[id] = true
	}
	for _, id := range o.progress.Failed() {
		failed[id] = true
	}
	var out []item.WorkItem
	for _, it := range items {
		if it.Type == item.Website && failed[it.Identifier] {
			out = append(out, it)
		}
	}
	return out
}

// runRetryPass re-crawls retryItems with the given backend (auto-mode
// ladder). A backend that fails to start skips the pass.
func (o *Orchestrator) runRetryPass(ctx context.Context, stack *closers, retryItems []item.WorkItem, mode string, poolSize int, deferFailures, rejectRedirects bool) {
	logger.Info(fmt.Sprintf("Auto mode: retrying %d failed websites with the %s crawler", len(retryItems), mode))
	scraper, err := o.scraperFactory()(ctx, ScraperOptions{Mode: mode, RejectRedirects: rejectRedirects})
	if err == nil {
		stack.push(scraper)
		err = scraper.CreateSessionPool(ctx, poolSize)
	}
	if err != nil {
		logger.Warn(fmt.Sprintf("%s crawler failed to start (%v); skipping this pass", mode, err))
		return
	}
	o.scraper = scraper
	o.scrapeMode = mode
	o.deferWebsiteFailures = deferFailures
	o.processItems(ctx, retryItems)
}

// deepCrawlAvailable reports whether the deep (headless Chrome) crawler can
// run on this host.
func (o *Orchestrator) deepCrawlAvailable() bool {
	if o.deps.DeepAvailable != nil {
		return o.deps.DeepAvailable()
	}
	if !o.cfg.DeepRetryEnabled {
		return false
	}
	// The WebDriver client is in-tree, so podman is the only thing this
	// host can be missing.
	return scrape.PodmanAvailable(orDefault(o.cfg.DeepScrapePodmanBinary, "podman"))
}

// isAntibotFailure: a second headless-Chrome pass from the same datacenter
// IP can't beat an anti-bot block — it just re-hammers the site.
func (o *Orchestrator) isAntibotFailure(domain string) bool {
	return item.IsAntibotError(o.progress.Error(domain))
}

// retryFailedWebsitesDeep is auto mode's second pass: re-crawl failed
// websites with headless Chrome.
func (o *Orchestrator) retryFailedWebsitesDeep(ctx context.Context, stack *closers, items []item.WorkItem) {
	retryItems := o.collectRetryItems(items)
	if len(retryItems) == 0 {
		return
	}
	if !o.deepCrawlAvailable() {
		logger.Info(fmt.Sprintf("%d websites still failed, but the deep crawler is unavailable on this host (podman missing) — keeping earlier results", len(retryItems)))
		return
	}

	// Skip domains the direct pass already proved are blocking this IP.
	// Rendering them again from the same IP fails identically and only adds
	// load and ban risk; leave their earlier failure in place.
	if o.cfg.SkipDeepRetryOnBlk {
		var blocked []string
		var rest []item.WorkItem
		for _, it := range retryItems {
			if o.isAntibotFailure(it.Identifier) {
				blocked = append(blocked, it.Identifier)
			} else {
				rest = append(rest, it)
			}
		}
		if len(blocked) > 0 {
			slices.Sort(blocked)
			logger.Info(fmt.Sprintf("Skipping the deep crawler for %d anti-bot-blocked site(s) (same datacenter IP would be blocked again): %s", len(blocked), strings.Join(blocked, ", ")))
			retryItems = rest
		}
		if len(retryItems) == 0 {
			logger.Info("All remaining failures are anti-bot blocks — nothing the deep crawler can rescue. The research fallback will classify them from public sources.")
			return
		}
	}

	o.runRetryPass(ctx, stack, retryItems, "deep", 1,
		// Deep-pass failures stay retryable only while the research
		// fallback can still run after this pass.
		o.researchFallbackAvailable(),
		// Retry passes follow cross-host redirects so publishers that
		// redirect a brand domain to a parent site classify on the
		// destination's content.
		!o.cfg.RetryFollowRedirect,
	)
}

func (o *Orchestrator) researchFallbackAvailable() bool { return o.cfg.ResearchFallbackEnabled }

// researchFailedWebsites is auto mode's final pass: classify still-failed
// websites from research.
//
// Every scrape backend has had its shot by now; what's left is mostly
// hardened anti-bot publishers this host's IP cannot fetch. A
// web-search-augmented model researches each domain (never touching the
// site from here — zero ban risk) and the regular classifier runs on that
// summary. Domains with no meaningful public footprint stay Failed.
func (o *Orchestrator) researchFailedWebsites(ctx context.Context, items []item.WorkItem) {
	retryItems := o.collectRetryItems(items)
	if len(retryItems) == 0 {
		return
	}
	if !o.researchFallbackAvailable() {
		logger.Info(fmt.Sprintf("%d websites still failed and the research fallback is disabled — keeping earlier results", len(retryItems)))
		return
	}
	logger.Info(fmt.Sprintf("Auto mode: classifying %d unscrapeable website(s) from external research (%s)", len(retryItems), o.cfg.ResearchModel))

	o.deferWebsiteFailures = false
	processor := o.domainProcessor(false)
	// API-bound: not limited by the crawl pool size (concurrent_sessions).
	runBounded(ctx, o.cfg.ResearchMaxConcurrent, retryItems, func(it item.WorkItem) {
		if ctx.Err() != nil {
			return
		}
		guard("Research fallback task failed for %s: %v", it.Identifier, func() {
			o.researchDomain(ctx, processor, it.Identifier)
		})
	})
}

// runCTVWorkflow is the CTV (Connected TV) two-step workflow.
func (o *Orchestrator) runCTVWorkflow(ctx context.Context) error {
	if err := o.runCTV(ctx); err != nil {
		logger.Error(fmt.Sprintf("Fatal error in CTV workflow: %v", err))
		return err
	}
	return nil
}

func (o *Orchestrator) runCTV(ctx context.Context) error {
	runStart := time.Now()
	logger.Info("Starting CTV (Connected TV) Analysis")
	logger.Info(fmt.Sprintf("Research model: %s, Classification model: %s", o.cfg.CTVResearchModel, o.cfg.CTVClassificationModel))
	logger.Info(fmt.Sprintf("CTV concurrency: %d", o.cfg.CTVMaxConcurrent))

	ctvItems, err := o.loadCTVInputData()
	if err != nil {
		return err
	}
	if err := o.startReporter(len(ctvItems)); err != nil {
		return err
	}
	defer o.reporter.Stop()

	if len(ctvItems) == 0 {
		logger.Error("No valid CTV items to process")
		o.finishSummary(runStart)
		return nil
	}
	var toProcess []item.CTVWorkItem
	for _, it := range ctvItems {
		if !o.progress.IsProcessed(it.Identifier()) {
			toProcess = append(toProcess, it)
		}
	}
	if len(toProcess) == 0 {
		logger.Info("All CTV items have already been processed!")
		o.finishSummary(runStart)
		return nil
	}
	logger.Info(fmt.Sprintf("Processing %d CTV apps (skipping %d already processed)", len(toProcess), len(ctvItems)-len(toProcess)))

	if err := o.processAllCTV(ctx, toProcess); err != nil {
		return err
	}
	o.finishSummary(runStart)
	logger.Info("CTV analysis completed successfully!")
	return nil
}

func (o *Orchestrator) processAllCTV(ctx context.Context, toProcess []item.CTVWorkItem) error {
	var stack closers
	defer stack.closeAll()

	logger.Info("Initializing CTV API clients...")
	llm, err := o.llmFactory()(ctx, LLMOptions{
		APIKey:        o.cfg.OpenRouterAPIKey,
		Model:         o.cfg.CTVClassificationModel,
		Temperature:   o.cfg.CTVClassificationTemp,
		MaxTokens:     o.cfg.CTVClassificationMaxTokens,
		MaxRetries:    o.cfg.LLMRequestRetries,
		Timeout:       o.cfg.LLMRequestTimeout,
		FallbackModel: o.cfg.CTVClassificationFallback,
	})
	if err != nil {
		return err
	}
	stack.push(llm)
	o.llm = llm
	logger.Info("Testing OpenRouter API connection...")
	if !llm.TestConnection(ctx) {
		return errors.New("Failed to initialise OpenRouter client. " + //nolint:staticcheck // message kept from the Python release
			"Check network connectivity and OPENROUTER_API_KEY configuration.")
	}
	logger.Info("CTV clients initialised successfully")

	writer, err := openResultsFile(o.cfg.OutputCSVPath, o.outputColumns(true))
	if err != nil {
		return err
	}
	o.writer = writer
	defer o.teardownOutputFile()

	processor := &CTVProcessor{
		Progress:                  o.progress,
		Client:                    llm,
		Reporter:                  o.reporter,
		Writer:                    writer,
		Categories:                o.categories,
		ResearchModel:             o.cfg.CTVResearchModel,
		ResearchTemperature:       o.cfg.CTVResearchTemperature,
		ResearchMaxTokens:         o.cfg.CTVResearchMaxTokens,
		ClassificationModel:       o.cfg.CTVClassificationModel,
		ClassificationTemperature: o.cfg.CTVClassificationTemp,
		ClassificationMaxTokens:   o.cfg.CTVClassificationMaxTokens,
		RequestDelay:              time.Duration(o.cfg.CTVRequestDelay * float64(time.Second)),
	}
	cut := runBounded(ctx, o.cfg.CTVMaxConcurrent, toProcess, func(it item.CTVWorkItem) {
		if ctx.Err() != nil {
			return
		}
		guard("CTV task failed: %v", "", func() { processor.ProcessCTVApp(ctx, it) })
	})
	if cut {
		logger.Info("Shutdown requested, cancelling remaining CTV tasks...")
	}
	return nil
}
