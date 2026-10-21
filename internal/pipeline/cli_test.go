// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package pipeline

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ElcanoTek/lens/internal/config"
	"github.com/ElcanoTek/lens/internal/logx"
	"github.com/ElcanoTek/lens/internal/progress"
)

func TestParseRunArgsAndPrecedence(t *testing.T) {
	a, err := ParseRunArgs([]string{
		"--scrape-mode", "direct", "--deep-scrape", "--allow-redirects", "--llm-model", "x/llm",
		"--deep-retry", "off", "--research-fallback=off", "--research-model", "perplexity/sonar-pro",
		"--decision-model", "openai/x", "--custom-categories", "labels.json", "--input-csv", "in.csv",
		"--output-csv", "out.csv", "--progress-file", "p.json", "--log-file", "l.log", "--verbose", "--ctv", "--jsonl", "a.jsonl",
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if a.CustomCategories != "labels.json" || a.DecisionModel != "openai/x" || !a.CTV || a.JSONL != "a.jsonl" {
		t.Fatalf("args = %+v", a)
	}
	cfg := config.Default()
	mode, reject, verbose := a.Apply(cfg, "")
	// --deep-scrape beats --scrape-mode; --allow-redirects turns off rejection.
	if mode != "deep" || reject || !verbose {
		t.Fatalf("mode=%s reject=%v verbose=%v", mode, reject, verbose)
	}
	if cfg.LLMModel != "x/llm" || cfg.CTVClassificationModel != "x/llm" || cfg.DeepRetryEnabled || cfg.ResearchFallbackEnabled ||
		cfg.ResearchModel != "perplexity/sonar-pro" || cfg.CTVResearchModel != "perplexity/sonar-pro" || cfg.DecisionModel != "openai/x" ||
		cfg.InputCSVPath != "in.csv" || cfg.OutputCSVPath != "out.csv" || cfg.ProgressFilePath != "p.json" || cfg.LogFilePath != "l.log" {
		t.Fatalf("cfg = %+v", cfg)
	}

	// Defaults come from config; SCRAPER_VERBOSE enables verbose unless --quiet.
	a, _ = ParseRunArgs(nil, io.Discard)
	cfg = config.Default()
	cfg.ScrapeMode = "Direct"
	if mode, reject, verbose = a.Apply(cfg, "yes"); mode != "direct" || !reject || !verbose {
		t.Fatalf("mode=%s reject=%v verbose=%v", mode, reject, verbose)
	}
	a, _ = ParseRunArgs([]string{"--quiet", "--verbose"}, io.Discard)
	if _, _, verbose = a.Apply(config.Default(), "1"); verbose {
		t.Fatal("--quiet must win")
	}

	for _, bad := range [][]string{{"--scrape-mode", "firecrawl"}, {"--deep-retry", "maybe"}, {"stray"}, {"--nope"}} {
		var stderr bytes.Buffer
		if _, err := ParseRunArgs(bad, &stderr); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
	var stderr bytes.Buffer
	_, err = ParseRunArgs([]string{"--scrape-mode", "firecrawl"}, &stderr)
	if !strings.Contains(err.Error(), "invalid choice: 'firecrawl' (choose from 'auto', 'direct', 'deep')") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunMainExitCodes(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("OPENROUTER_API_KEY", "")
	var stdout, stderr lockedBuffer
	defer logx.SetOutput(io.Discard, slog.LevelError)

	if code := runMain(context.Background(), []string{"--bogus"}, Deps{}, &stdout, &stderr); code != 2 {
		t.Fatalf("usage error exit = %d", code)
	}
	// No API key: the run fails (exit 1) and says why.
	if code := runMain(context.Background(), []string{"--log-file", "run.log"}, Deps{}, &stdout, &stderr); code != 1 {
		t.Fatalf("missing key exit = %d", code)
	}
	if !strings.Contains(stdout.String(), "Application failed: OPENROUTER_API_KEY environment variable is required") {
		t.Fatalf("stdout = %s", stdout.String())
	}
	logText, _ := os.ReadFile("run.log")
	if !strings.Contains(string(logText), " - main - ERROR - Application failed:") {
		t.Fatalf("log = %s", logText)
	}
	// A missing input file is a failed run too.
	t.Setenv("OPENROUTER_API_KEY", "k")
	if code := runMain(context.Background(), []string{"--log-file", "run.log", "--input-csv", "missing.csv"}, Deps{}, &stdout, &stderr); code != 1 {
		t.Fatalf("missing input exit = %d", code)
	}
	// Bad custom categories fail before anything runs.
	writeFile(t, "labels.json", `{"categories": []}`)
	if code := runMain(context.Background(), []string{"--log-file", "run.log", "--custom-categories", "labels.json"}, Deps{}, &stdout, &stderr); code != 1 {
		t.Fatalf("bad categories exit = %d", code)
	}
	if !strings.Contains(stdout.String(), "Define between 1 and 12 custom categories.") {
		t.Fatalf("stdout = %s", stdout.String())
	}
}

func TestManageCommands(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("OPENROUTER_API_KEY", "")
	run := func(stdin string, args ...string) (int, string) {
		var out, errOut bytes.Buffer
		code := manage(args, strings.NewReader(stdin), &out, &errOut)
		return code, out.String() + errOut.String()
	}

	if code, out := run("", "bogus"); code != 2 || !strings.Contains(out, "invalid choice: 'bogus'") {
		t.Fatalf("bogus: %d %s", code, out)
	}
	if code, _ := run(""); code != 2 {
		t.Fatal("missing command accepted")
	}

	// setup: built-in defaults when no example files exist, never overwrite.
	if code, out := run("", "setup"); code != 0 || !strings.Contains(out, "Creating basic .env file") || !strings.Contains(out, "✅ Environment setup complete!") {
		t.Fatalf("setup: %d %s", code, out)
	}
	if env, _ := os.ReadFile(".env"); string(env) != "# Add your API keys here\nOPENROUTER_API_KEY=\n" {
		t.Fatalf(".env = %q", env)
	}
	if cfg := config.Load("config.json"); len(cfg.Warnings) != 0 || cfg.ConcurrentSessions != 5 {
		t.Fatalf("config.json defaults unreadable: %v", cfg.Warnings)
	}
	writeFile(t, ".env", "OPENROUTER_API_KEY=kept\n")
	run("", "setup")
	if env, _ := os.ReadFile(".env"); string(env) != "OPENROUTER_API_KEY=kept\n" {
		t.Fatal("setup overwrote .env")
	}
	// From the example files when present.
	sub := filepath.Join(dir, "fromexample")
	if err := os.Mkdir(sub, 0o750); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)
	writeFile(t, ".env.example", "OPENROUTER_API_KEY=\nAUTH_SIGNING_PUBKEY=\n")
	writeFile(t, "config.json.example", `{"concurrent_sessions": 2}`)
	if code, out := run("", "setup"); code != 0 || !strings.Contains(out, "Creating .env from .env.example") || !strings.Contains(out, "Creating config.json from config.json.example") {
		t.Fatalf("setup: %d %s", code, out)
	}
	if c, _ := os.ReadFile("config.json"); string(c) != `{"concurrent_sessions": 2}` {
		t.Fatalf("config.json = %s", c)
	}

	// validate
	if _, out := run("", "validate"); !strings.Contains(out, "❌ Missing required environment variables:\n   OPENROUTER_API_KEY") {
		t.Fatalf("validate: %s", out)
	}
	t.Setenv("OPENROUTER_API_KEY", "k")
	if _, out := run("", "validate"); !strings.Contains(out, "❌ Input file not found: input.csv") {
		t.Fatalf("validate: %s", out)
	}
	if code, out := run("", "sample"); code != 0 || !strings.Contains(out, "✅ Created sample_input.csv") {
		t.Fatalf("sample: %s", out)
	}
	if s, _ := os.ReadFile("sample_input.csv"); !strings.HasPrefix(string(s), "Domain\nexample.com\n") {
		t.Fatalf("sample = %q", s)
	}
	writeFile(t, "input.csv", "Domain\nexample.com\n")
	if _, out := run("", "validate"); !strings.Contains(out, "✅ Configuration validation passed") {
		t.Fatalf("validate: %s", out)
	}

	// progress / stats / reset-failed / reset on a real progress file.
	tr := progress.Open("progress.json")
	tr.SetTotal(3)
	for _, e := range []struct{ id, status, msg string }{
		{"b.com", progress.StatusError, "Received HTTP status 403"},
		{"a.com", progress.StatusSuccess, ""},
		{"c.com", progress.StatusError, ""},
	} {
		if err := tr.Mark(e.id, e.status, nil, e.msg, nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	_, out := run("", "progress")
	if !strings.Contains(out, "Total domains: 3") || !strings.Contains(out, "📋 Recent activity (last 10 domains):") {
		t.Fatalf("progress: %s", out)
	}
	// File order, which is sorted keys for files Go wrote.
	if strings.Index(out, "   a.com - success (") > strings.Index(out, "   b.com - error (") {
		t.Fatalf("progress order: %s", out)
	}
	_, out = run("", "stats")
	if !strings.Contains(out, "📊 Detailed Statistics:") || !strings.Contains(out, "❌ Failed domains (2):") ||
		!strings.Contains(out, "   b.com: Received HTTP status 403") || !strings.Contains(out, "   c.com: Unknown error") {
		t.Fatalf("stats: %s", out)
	}
	if _, out = run("n\n", "reset-failed"); !strings.Contains(out, "❌ Reset cancelled") {
		t.Fatalf("reset-failed: %s", out)
	}
	if _, out = run("y\n", "reset-failed"); !strings.Contains(out, "Found 2 failed domains:") || !strings.Contains(out, "✅ Failed domains reset successfully") {
		t.Fatalf("reset-failed: %s", out)
	}
	if got := progress.Open("progress.json").Failed(); len(got) != 0 {
		t.Fatalf("failed after reset = %v", got)
	}
	if _, out = run("", "reset-failed"); !strings.Contains(out, "ℹ️  No failed domains found") {
		t.Fatalf("reset-failed: %s", out)
	}
	if _, out = run("N\n", "reset"); !strings.Contains(out, "❌ Progress reset cancelled") || !fileExists("progress.json") {
		t.Fatalf("reset: %s", out)
	}
	if _, out = run("y\n", "reset"); !strings.Contains(out, "✅ Progress reset successfully") || fileExists("progress.json") {
		t.Fatalf("reset: %s", out)
	}
	if _, out = run("", "reset"); !strings.Contains(out, "ℹ️  No progress file found") {
		t.Fatalf("reset: %s", out)
	}
	if code, out := run("", "help"); code != 0 || !strings.Contains(out, "reset-failed- Reset failed domains for retry") {
		t.Fatalf("help: %s", out)
	}
}

func TestOrderedEntriesKeepsFileOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.json")
	writeFile(t, path, `{"processed_domains": {"z.com": {"status": "error", "timestamp": "t1", "error_message": "boom"}, "a.com": {"status": "success", "timestamp": "t2"}}}`)
	order, entries := orderedEntries(path)
	if strings.Join(order, ",") != "z.com,a.com" || entries["z.com"].ErrorMessage != "boom" {
		t.Fatalf("order=%v entries=%v", order, entries)
	}
}
