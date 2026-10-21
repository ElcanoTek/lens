// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package scrape

import (
	"strings"
	"testing"

	"github.com/ElcanoTek/lens/internal/item"
)

func TestValidateResult(t *testing.T) {
	long := strings.Repeat("word ", 200)
	cases := []struct {
		name string
		r    *Result
		want string
	}{
		{"nil", nil, "Scraper returned no data"},
		{"failed", &Result{Error: "boom"}, "boom"},
		{"failed without message", &Result{}, "Scraper reported failure"},
		{"forbidden", &Result{Success: true, StatusCode: 403, Content: long}, "Received HTTP status 403"},
		{"server error", &Result{Success: true, StatusCode: 503, Content: long}, "Received HTTP status 503"},
		{"redirect status is fine", &Result{Success: true, StatusCode: 301, Content: long, ContentLength: 1000}, ""},
		{"empty", &Result{Success: true, StatusCode: 200, Content: "  \n"}, "Scraper returned empty content"},
		{"short", &Result{Success: true, StatusCode: 200, Content: strings.Repeat("a", 499), ContentLength: 499},
			"Scraped content too short (499 chars < 500)"},
		{"challenge", &Result{Success: true, StatusCode: 200, Content: "Just a moment... " + long, ContentLength: 1017},
			"Potential block page detected ('just a moment')"},
		{"captcha", &Result{Success: true, StatusCode: 200, Content: "Please solve the CAPTCHA " + long, ContentLength: 1025},
			"Potential block page detected ('captcha')"},
		{"phrase past the snippet", &Result{Success: true, StatusCode: 200, Content: long + "access denied", ContentLength: 1013}, ""},
		{"ok", &Result{Success: true, StatusCode: 200, Content: long, ContentLength: 1000}, ""},
	}
	for _, c := range cases {
		if got := ValidateResult(c.r, 500); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
	// The block messages are what the anti-bot check keys on.
	for _, msg := range []string{"Received HTTP status 403", "Potential block page detected ('captcha')"} {
		if !item.IsAntibotError("Scrape validation failed: " + msg) {
			t.Errorf("%q not recognised as anti-bot", msg)
		}
	}
}

func TestDeriveBotProtection(t *testing.T) {
	combined := "Received HTTP status 403; research fallback: Research found no meaningful public information about this domain"
	cases := []struct {
		name string
		ev   BotEvidence
		want string
	}{
		{"direct success", BotEvidence{ScrapeMode: "direct"}, "None Detected"},
		{"browser rescue after 403", BotEvidence{ScrapeMode: "deep",
			PriorError: "Scrape validation failed: Received HTTP status 403", PriorErrorMode: "direct"}, "Moderate"},
		{"browser rescue after a non-block failure", BotEvidence{ScrapeMode: "deep",
			PriorError: "Scraped content too short (101 chars < 500)", PriorErrorMode: "direct"}, "None Detected"},
		{"research rescue after a browser block", BotEvidence{ScrapeMode: "research",
			PriorError: "Potential block page detected ('just a moment')", PriorErrorMode: "deep"}, "Aggressive"},
		{"research rescue after a plain HTTP block", BotEvidence{ScrapeMode: "research",
			PriorError: "Received HTTP status 403", PriorErrorMode: "direct"}, "Moderate"},
		{"research rescue without block evidence", BotEvidence{ScrapeMode: "research",
			PriorError: "Chrome scraping failed: timeout", PriorErrorMode: "deep"}, "Unknown"},
		{"research success with no history", BotEvidence{ScrapeMode: "research"}, "Unknown"},
		{"direct failure with 403", BotEvidence{ScrapeMode: "direct", Failed: true,
			CurrentError: "Scrape validation failed: Received HTTP status 403"}, "Moderate"},
		{"browser failure with block", BotEvidence{ScrapeMode: "deep", Failed: true,
			CurrentError: "Potential block page detected ('captcha')",
			PriorError:   "Received HTTP status 403", PriorErrorMode: "direct"}, "Aggressive"},
		{"research failure echoing a deep block", BotEvidence{ScrapeMode: "research", Failed: true,
			CurrentError: combined, PriorError: "Received HTTP status 403", PriorErrorMode: "deep"}, "Aggressive"},
		{"research failure echoing a direct block", BotEvidence{ScrapeMode: "research", Failed: true,
			CurrentError: combined, PriorError: "Received HTTP status 403", PriorErrorMode: "direct"}, "Moderate"},
		{"failure without block evidence", BotEvidence{ScrapeMode: "direct", Failed: true,
			CurrentError: "Chrome scraping failed: timeout"}, "Unknown"},
		{"prior mode missing reads as plain HTTP", BotEvidence{ScrapeMode: "research",
			PriorError: "Received HTTP status 429"}, "Moderate"},
	}
	for _, c := range cases {
		if got := DeriveBotProtection(c.ev); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
