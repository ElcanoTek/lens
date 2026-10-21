// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package pipeline

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/ElcanoTek/lens/internal/categories"
	"github.com/ElcanoTek/lens/internal/config"
	"github.com/ElcanoTek/lens/internal/logx"
)

var mainLog = logx.New("main")

// RunArgs are `lens run`'s flags (main.py's argparse options).
type RunArgs struct {
	Quiet            bool
	Verbose          bool
	JSONL            string
	ScrapeMode       string
	DeepScrape       bool
	AllowRedirects   bool
	CTV              bool
	LLMModel         string
	DeepRetry        string
	ResearchFallback string
	ResearchModel    string
	CustomCategories string
	DecisionModel    string
	InputCSV         string
	OutputCSV        string
	ProgressFile     string
	LogFile          string
}

// choiceFlag is a string flag restricted to fixed choices (argparse
// choices=...).
type choiceFlag struct {
	value   *string
	choices []string
}

func (c choiceFlag) String() string {
	if c.value == nil {
		return ""
	}
	return *c.value
}

func (c choiceFlag) Set(s string) error {
	for _, choice := range c.choices {
		if s == choice {
			*c.value = s
			return nil
		}
	}
	quoted := make([]string, len(c.choices))
	for i, choice := range c.choices {
		quoted[i] = "'" + choice + "'"
	}
	return fmt.Errorf("invalid choice: '%s' (choose from %s)", s, strings.Join(quoted, ", "))
}

// ParseRunArgs parses `lens run` arguments. Errors are argparse-style
// usage errors.
func ParseRunArgs(args []string, stderr io.Writer) (*RunArgs, error) {
	a := &RunArgs{}
	fs := flag.NewFlagSet("lens run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: lens run [flags]")
		fmt.Fprintln(stderr, "Run Lens: classify websites, apps and CTV inventory into advertising-quality tiers.")
		fs.PrintDefaults()
	}
	fs.BoolVar(&a.Quiet, "quiet", false, "Suppress per-domain lines; show only live metrics and summary.")
	fs.BoolVar(&a.Verbose, "verbose", false, "Show additional per-domain diagnostics (size, retries, cache status).")
	fs.StringVar(&a.JSONL, "jsonl", "", "Write structured JSONL attempt logs to the specified `PATH`.")
	fs.Var(choiceFlag{&a.ScrapeMode, []string{"auto", "direct", "deep"}}, "scrape-mode",
		"Select the scraping backend (overrides config.json): auto|direct|deep. 'auto' detects the input type "+
			"(websites, apps, CTV), starts with the fast crawler, retries failed websites with the deep crawler "+
			"(if Podman is available) and classifies what still fails from research.")
	fs.BoolVar(&a.DeepScrape, "deep-scrape", false, "Convenience flag equivalent to --scrape-mode=deep.")
	fs.BoolVar(&a.AllowRedirects, "allow-redirects", false, "Follow HTTP redirects instead of marking them as failed (default: reject redirects).")
	fs.BoolVar(&a.CTV, "ctv", false, "Enable CTV (Connected TV) processing mode. Uses two-step AI pipeline with Perplexity for research and fast model for classification.")
	fs.StringVar(&a.LLMModel, "llm-model", "", "Override the classification model for this run (an OpenRouter model ID, e.g. '~openai/gpt-luna-latest').")
	fs.Var(choiceFlag{&a.DeepRetry, []string{"on", "off"}}, "deep-retry",
		"Enable/disable auto mode's headless-Chrome retry pass for this run (on|off, overrides config.json). "+
			"Off sends sites the direct fetch could not read straight to the research fallback, which is much faster on large lists.")
	fs.Var(choiceFlag{&a.ResearchFallback, []string{"on", "off"}}, "research-fallback",
		"Enable/disable the research fallback for this run (on|off, overrides config.json). When a website blocks "+
			"every crawler, a web-search model researches the domain and classification runs on that summary instead of failing the site.")
	fs.StringVar(&a.ResearchModel, "research-model", "", "Override the research-fallback model for this run (an OpenRouter model ID with built-in web search, e.g. 'perplexity/sonar').")
	fs.StringVar(&a.CustomCategories, "custom-categories", "", "Add custom categories defined in a `JSON_FILE`, answered by an OpenRouter decision model.")
	fs.StringVar(&a.DecisionModel, "decision-model", "", "Override the custom-category decision model for this run (an OpenRouter decision model ID, e.g. '~typesafe/jev-latest').")
	fs.StringVar(&a.InputCSV, "input-csv", "", "Override input CSV path for this run.")
	fs.StringVar(&a.OutputCSV, "output-csv", "", "Override output CSV path for this run.")
	fs.StringVar(&a.ProgressFile, "progress-file", "", "Override progress JSON path for this run.")
	fs.StringVar(&a.LogFile, "log-file", "", "Override log file path for this run.")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		err := fmt.Errorf("unrecognized arguments: %s", strings.Join(fs.Args(), " "))
		fmt.Fprintln(stderr, "lens run: error:", err)
		return nil, err
	}
	return a, nil
}

// Apply folds the flags into cfg with main.py's precedence and returns the
// effective scrape mode, redirect policy and verbosity.
func (a *RunArgs) Apply(cfg *config.Config, scraperVerboseEnv string) (scrapeMode string, rejectRedirects, verbose bool) {
	switch strings.ToLower(scraperVerboseEnv) {
	case "1", "true", "yes", "on":
		verbose = true
	}
	verbose = verbose || a.Verbose
	if a.Quiet {
		verbose = false
	}
	scrapeMode = strings.ToLower(orDefault(cfg.ScrapeMode, "direct"))
	if a.ScrapeMode != "" {
		scrapeMode = a.ScrapeMode
	}
	if a.DeepScrape {
		scrapeMode = "deep"
	}
	// Reject redirects by default; --allow-redirects follows them.
	rejectRedirects = cfg.RejectRedirects && !a.AllowRedirects

	if a.LLMModel != "" {
		cfg.LLMModel = a.LLMModel
		cfg.CTVClassificationModel = a.LLMModel
	}
	if a.DeepRetry != "" {
		cfg.DeepRetryEnabled = a.DeepRetry == "on"
	}
	if a.ResearchFallback != "" {
		cfg.ResearchFallbackEnabled = a.ResearchFallback == "on"
	}
	if a.ResearchModel != "" {
		cfg.ResearchModel = a.ResearchModel
		cfg.CTVResearchModel = a.ResearchModel
	}
	if a.DecisionModel != "" {
		cfg.DecisionModel = a.DecisionModel
	}
	if a.InputCSV != "" {
		cfg.InputCSVPath = a.InputCSV
	}
	if a.OutputCSV != "" {
		cfg.OutputCSVPath = a.OutputCSV
	}
	if a.ProgressFile != "" {
		cfg.ProgressFilePath = a.ProgressFile
	}
	if a.LogFile != "" {
		cfg.LogFilePath = a.LogFile
	}
	return scrapeMode, rejectRedirects, verbose
}

// RunMain is `lens run` (main.py): it parses the flags, sets up logging
// (INFO+ to the log file, WARNING+ to stdout), runs the orchestrator and
// returns the process exit code: 0 on success or graceful shutdown, 1 when
// the run failed, 2 for a usage error.
//
// SIGINT/SIGTERM cancel the run's context; the orchestrator stops starting
// items, abandons in-flight ones and finishes its bookkeeping.
func RunMain(args []string) int {
	return runMain(context.Background(), args, Deps{}, os.Stdout, os.Stderr)
}

func runMain(parent context.Context, args []string, deps Deps, stdout, stderr io.Writer) int {
	parsed, err := ParseRunArgs(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	config.LoadDotenv()
	cfg := config.Load("config.json")
	for _, w := range cfg.Warnings {
		fmt.Fprintln(stdout, "Warning: "+w)
	}
	scrapeMode, rejectRedirects, verbose := parsed.Apply(cfg, os.Getenv("SCRAPER_VERBOSE"))

	logFile := orDefault(cfg.LogFilePath, "site_analysis.log")
	if err := logx.Setup(logFile, stdout, slog.LevelWarn); err != nil {
		fmt.Fprintf(stderr, "Could not open log file %s: %v\n", logFile, err)
		return 1
	}
	defer logx.Close()

	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	done := make(chan struct{})
	defer close(done)
	go func() {
		// Later signals are absorbed like the Python handler did; the run is
		// already winding down.
		for {
			select {
			case sig := <-signals:
				mainLog.Info(fmt.Sprintf("Received signal %v, initiating graceful shutdown...", sig))
				cancel()
			case <-done:
				return
			}
		}
	}()

	if err := run(ctx, cfg, parsed, scrapeMode, rejectRedirects, verbose, deps, stdout); err != nil {
		mainLog.Error(fmt.Sprintf("Application failed: %v", err))
		return 1
	}
	return 0
}

func run(ctx context.Context, cfg *config.Config, a *RunArgs, scrapeMode string, rejectRedirects, verbose bool, deps Deps, stdout io.Writer) error {
	// The Python config module refused to import without the key; fail the
	// run the same way, but after logging is up so the reason is in the log.
	if deps.NewLLM == nil {
		if err := cfg.RequireAPIKey(); err != nil {
			return err
		}
	}
	var spec *categories.Spec
	if a.CustomCategories != "" {
		var err error
		if spec, err = categories.Load(a.CustomCategories); err != nil {
			return err
		}
	}
	o, err := New(Options{
		Config:           cfg,
		Quiet:            a.Quiet,
		Verbose:          verbose,
		JSONLPath:        a.JSONL,
		ScrapeMode:       scrapeMode,
		RejectRedirects:  rejectRedirects,
		CTVMode:          a.CTV,
		CustomCategories: spec,
		Deps:             deps,
		Out:              stdout,
	})
	if err != nil {
		return err
	}
	return o.Run(ctx)
}
