// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDefaultsWithoutFile(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", " sk-test ")
	cfg := Load(filepath.Join(t.TempDir(), "missing.json"))
	if cfg.ScrapeMode != "auto" || cfg.LLMModel != DefaultLLMModel || cfg.ResearchModel != DefaultResearchModel {
		t.Fatalf("defaults wrong: %+v", cfg)
	}
	if cfg.OpenRouterAPIKey != "sk-test" || cfg.RequireAPIKey() != nil || len(cfg.Warnings) != 0 {
		t.Fatalf("key/warnings: %q %v", cfg.OpenRouterAPIKey, cfg.Warnings)
	}
}

func TestMissingKeyIsReportedNotFatal(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "")
	if err := Load("nope.json").RequireAPIKey(); err == nil || !strings.Contains(err.Error(), "OPENROUTER_API_KEY") {
		t.Fatalf("RequireAPIKey = %v", err)
	}
}

func TestFileOverridesAndLenientNumbers(t *testing.T) {
	cfg := Load(write(t, `{"concurrent_sessions": 8.0, "retry_delay": 2, "deep_scrape_extra_args": ["--shm-size=2g"], "scrape_mode": "direct", "unknown_key": 1}`))
	if cfg.ConcurrentSessions != 8 || cfg.RetryDelay != 2 || cfg.ScrapeMode != "direct" || len(cfg.DeepScrapeExtraArgs) != 1 {
		t.Fatalf("overlay wrong: %+v", cfg)
	}
}

func TestBadFileFallsBackToDefaults(t *testing.T) {
	cfg := Load(write(t, `{"concurrent_sessions": "many"}`))
	if cfg.ConcurrentSessions != 5 || len(cfg.Warnings) != 1 {
		t.Fatalf("got %d, warnings %v", cfg.ConcurrentSessions, cfg.Warnings)
	}
	cfg = Load(write(t, `{broken`))
	if len(cfg.Warnings) != 1 {
		t.Fatalf("warnings %v", cfg.Warnings)
	}
}

func TestLegacyMigrations(t *testing.T) {
	cfg := Load(write(t, `{
		"scrape_mode": "firecrawl",
		"firecrawl_follow_redirects": false,
		"firecrawl_api_url": "http://x",
		"user_agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36",
		"llm_model": "~google/gemini-flash-latest",
		"ctv_classification_model": "x-ai/grok-4.1-fast"}`))
	if cfg.ScrapeMode != "auto" || cfg.RetryFollowRedirect || cfg.UserAgent != DefaultUserAgent {
		t.Fatalf("legacy not migrated: %+v", cfg)
	}
	if cfg.LLMModel != DefaultLLMModel || cfg.CTVClassificationModel != DefaultLLMModel {
		t.Fatalf("models not upgraded: %s %s", cfg.LLMModel, cfg.CTVClassificationModel)
	}
	if len(cfg.Warnings) != 3 {
		t.Fatalf("warnings = %v", cfg.Warnings)
	}
	// An explicit retry_follow_redirects wins over the legacy key.
	cfg = Load(write(t, `{"firecrawl_follow_redirects": false, "retry_follow_redirects": true}`))
	if !cfg.RetryFollowRedirect {
		t.Fatal("explicit retry_follow_redirects lost")
	}
	// A deliberate, live model choice is respected.
	if cfg = Load(write(t, `{"llm_model": "openai/gpt-5-mini"}`)); cfg.LLMModel != "openai/gpt-5-mini" {
		t.Fatalf("deliberate model overwritten: %s", cfg.LLMModel)
	}
}

func TestExampleConfigMatchesDefaults(t *testing.T) {
	cfg := Load("../../config.json.example")
	def := Default()
	cfg.OpenRouterAPIKey, cfg.Warnings = "", nil
	def.OpenRouterAPIKey = ""
	var a, b strings.Builder
	_ = cfg.WriteJSON(&a)
	_ = def.WriteJSON(&b)
	if a.String() != b.String() {
		t.Fatalf("config.json.example drifted from Default():\nexample:\n%s\ndefault:\n%s", a.String(), b.String())
	}
}

// A JSON null switches off the optional fallback models and the VNC port, as
// None did in the Python release; elsewhere it keeps the default.
func TestNullDisablesOptionalSettings(t *testing.T) {
	cfg := Load(write(t, `{"llm_fallback_model": null, "ctv_classification_fallback_model": null,
		"deep_scrape_vnc_port": null, "concurrent_sessions": null, "llm_model": null}`))
	if len(cfg.Warnings) != 0 {
		t.Fatalf("warnings: %v", cfg.Warnings)
	}
	if cfg.LLMFallbackModel != "" || cfg.CTVClassificationFallback != "" || cfg.DeepScrapeVNCPort != 0 {
		t.Fatalf("null did not disable: %q %q %d", cfg.LLMFallbackModel, cfg.CTVClassificationFallback, cfg.DeepScrapeVNCPort)
	}
	if cfg.ConcurrentSessions != 5 || cfg.LLMModel != DefaultLLMModel {
		t.Fatalf("null replaced a default: %d %q", cfg.ConcurrentSessions, cfg.LLMModel)
	}
}
