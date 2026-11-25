// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package pipeline

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ElcanoTek/lens/internal/config"
)

func TestCrossJobResultCacheReusesWebsitesAndApps(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "cache.db")
	inputPath := filepath.Join(dir, "input.csv")
	if err := os.WriteFile(inputPath, []byte("Domain\nexample.com\n389801252\ncom.example.app\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	llm := newFakeLLM()
	scraper := &fakeScraper{
		mode: "direct",
		scrape: func(domain string) ScrapeResult {
			body := strings.Repeat("Quality journalism and original reporting. ", 25)
			return ScrapeResult{
				Success: true, URL: "https://" + domain, Title: domain,
				Content: body, ContentLength: len(body), StatusCode: 200, Mode: "direct",
			}
		},
	}
	rIOS, cIOS := 4.5, 100
	iosStore := &fakeStore{meta: AppMetadata{
		Success: true, AppName: "Harbor Tides", Developer: "Dev", PrimaryGenre: "Utilities",
		Rating: &rIOS, RatingCount: &cIOS, ContentForLLM: "App Name: Harbor Tides",
	}}
	rAnd, cAnd := 4.2, 50
	androidStore := &fakeStore{meta: AppMetadata{
		Success: true, AppName: "Harbor Android", Developer: "Dev", Category: "Tools",
		Downloads: "10,000+", Rating: &rAnd, RatingCount: &cAnd, ContentForLLM: "App Name: Harbor Android",
	}}

	deps := Deps{
		NewLLM:     func(context.Context, LLMOptions) (LLM, error) { return llm, nil },
		NewScraper: func(context.Context, ScraperOptions) (ScraperBackend, error) { return scraper, nil },
		NewIOS:     func(context.Context) (StoreClient, error) { return iosStore, nil },
		NewAndroid: func(context.Context) (StoreClient, error) { return androidStore, nil },
	}

	runOnce := func(suffix string) {
		cfg := config.Default()
		cfg.InputCSVPath = inputPath
		cfg.OutputCSVPath = filepath.Join(dir, "out_"+suffix+".csv")
		cfg.ProgressFilePath = filepath.Join(dir, "prog_"+suffix+".json")
		cfg.CacheEnabled = true
		cfg.CacheDBPath = cachePath
		cfg.CacheTTLDays = 30
		o, err := New(Options{Config: cfg, ScrapeMode: "direct", Deps: deps, Out: io.Discard, Quiet: true})
		if err != nil {
			t.Fatal(err)
		}
		if err := o.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
	}

	// Job 1 populates the cache.
	runOnce("job1")
	if len(scraper.scraped) != 1 || len(llm.siteCalls) != 1 || len(iosStore.calls) != 1 || len(androidStore.calls) != 1 || len(llm.appCalls) != 2 {
		t.Fatalf("after job1: scrapes=%d siteLLM=%d ios=%d android=%d appLLM=%d",
			len(scraper.scraped), len(llm.siteCalls), len(iosStore.calls), len(androidStore.calls), len(llm.appCalls))
	}

	// Job 2 (fresh output/progress files) hits the cache for all 3 items: zero scrapes/fetches/LLM calls.
	runOnce("job2")
	if len(scraper.scraped) != 1 || len(llm.siteCalls) != 1 || len(iosStore.calls) != 1 || len(androidStore.calls) != 1 || len(llm.appCalls) != 2 {
		t.Fatalf("after job2 (expected cache hits): scrapes=%d siteLLM=%d ios=%d android=%d appLLM=%d",
			len(scraper.scraped), len(llm.siteCalls), len(iosStore.calls), len(androidStore.calls), len(llm.appCalls))
	}
}
