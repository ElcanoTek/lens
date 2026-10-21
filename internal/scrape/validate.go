// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package scrape

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/ElcanoTek/lens/internal/htmldom"
	"github.com/ElcanoTek/lens/internal/item"
)

// blockPhrases near the top of a page mean a challenge or error page, not
// content. The resulting message carries "block page detected", which
// item.IsAntibotError recognises.
var blockPhrases = []string{
	"access denied",
	"attention required",
	"captcha",
	"forbidden",
	"just a moment",
	"service unavailable",
	"temporarily unavailable",
}

// ValidateResult checks that a scrape returned meaningful page content and
// returns why not ("" when it did). The website processor records the
// message (prefixed "Scrape validation failed: "). A 401/403/429 yields
// "Received HTTP status N" and a challenge page "Potential block page
// detected (…)": both are the anti-bot evidence item.IsAntibotError and
// DeriveBotProtection read.
func ValidateResult(r *Result, minContentLength int) string {
	if r == nil {
		return "Scraper returned no data"
	}
	if !r.Success {
		if r.Error != "" {
			return r.Error
		}
		return "Scraper reported failure"
	}
	if r.StatusCode != 0 && (r.StatusCode < 200 || r.StatusCode >= 400) {
		return fmt.Sprintf("Received HTTP status %d", r.StatusCode)
	}
	if htmldom.TrimSpace(r.Content) == "" {
		return "Scraper returned empty content"
	}
	length := r.ContentLength
	if length == 0 {
		length = utf8.RuneCountInString(r.Content)
	}
	if length < minContentLength {
		return fmt.Sprintf("Scraped content too short (%d chars < %d)", length, minContentLength)
	}
	snippet := []rune(strings.ToLower(r.Content))
	if len(snippet) > 512 {
		snippet = snippet[:512]
	}
	lower := string(snippet)
	for _, phrase := range blockPhrases {
		if strings.Contains(lower, phrase) {
			return fmt.Sprintf("Potential block page detected ('%s')", phrase)
		}
	}
	return ""
}

// Bot_Protection levels.
const (
	BotProtectionNone       = "None Detected"
	BotProtectionModerate   = "Moderate"
	BotProtectionAggressive = "Aggressive"
	BotProtectionUnknown    = "Unknown"
)

// BotEvidence is what DeriveBotProtection reads about one site.
type BotEvidence struct {
	// ScrapeMode is the rung that produced the row being recorded
	// ("direct", "deep" or "research").
	ScrapeMode string
	// Failed marks a Failed row; CurrentError is its failure message.
	Failed       bool
	CurrentError string
	// PriorError is the last failure an earlier pass recorded ("" if none),
	// and PriorErrorMode the rung that produced it ("" on state files from
	// before modes were tracked: read as plain HTTP, the conservative
	// reading).
	PriorError     string
	PriorErrorMode string
}

// DeriveBotProtection classifies a site's observed bot protection from how
// scraping went. Sites with bot mitigation tend to carry less invalid ad
// traffic, so this is a useful buying signal. Levels are strictly
// evidence-based — derived from which crawler rungs the site blocked during
// this run, never guessed by the LLM:
//
//   - None Detected: no crawler rung was ever blocked.
//   - Moderate: the plain-HTTP crawler was blocked (401/403/429 or a
//     challenge page), but no rendering browser was proven blocked.
//   - Aggressive: the rendering-browser rung (headless Chrome) was also
//     blocked: the site rejects automated traffic from this host outright.
//   - Unknown: the site could not be scraped for reasons that are not block
//     signatures (timeouts, DNS failures, dead site), or it was only ever
//     researched.
func DeriveBotProtection(ev BotEvidence) string {
	currentBlocked := ev.Failed && ev.CurrentError != "" && item.IsAntibotError(ev.CurrentError)
	var rung string
	switch {
	case currentBlocked && ev.ScrapeMode != "research":
		rung = ev.ScrapeMode
	case item.IsAntibotError(ev.PriorError) || currentBlocked:
		// Research-rung records only echo earlier scrape errors, so block
		// markers there are attributed to the rung that actually hit them.
		rung = ev.PriorErrorMode
		if rung == "" {
			rung = ModeDirect
		}
	default:
		if ev.Failed {
			return BotProtectionUnknown
		}
		// Scraped successfully without ever being blocked; a research-rung
		// success means the site was never fetched, so nothing was observed.
		if ev.ScrapeMode == "research" {
			return BotProtectionUnknown
		}
		return BotProtectionNone
	}
	if rung == ModeDeep {
		return BotProtectionAggressive
	}
	return BotProtectionModerate
}
