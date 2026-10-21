// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ElcanoTek/lens/internal/config"
	"github.com/ElcanoTek/lens/internal/cost"
	"github.com/ElcanoTek/lens/internal/item"
	"github.com/ElcanoTek/lens/internal/logx"
	"github.com/ElcanoTek/lens/internal/progress"
	"github.com/ElcanoTek/lens/internal/report"
)

var domainLog = logx.New("domain_processing")

// Bot_Protection levels.
const (
	BotProtectionNone       = "None Detected"
	BotProtectionModerate   = "Moderate"
	BotProtectionAggressive = "Aggressive"
	BotProtectionUnknown    = "Unknown"
)

// browserScrapeModes are the crawler rungs that drive a real rendering
// browser. A block at one of these means the site rejects even browser-like
// automation, not just plain HTTP clients.
var browserScrapeModes = map[string]bool{"deep": true}

// DeriveBotProtection classifies a site's observed bot protection from how
// scraping went.
//
// Sites with bot mitigation tend to carry less invalid ad traffic, so this
// is a useful buying signal. Levels are strictly evidence-based — derived
// from which crawler rungs the site blocked during this run, never guessed
// by the LLM:
//
//   - None Detected — no crawler rung was ever blocked.
//   - Moderate — the plain-HTTP crawler was blocked (403/401/429 or a
//     challenge page), but no rendering browser was proven blocked: the site
//     filters basic bots.
//   - Aggressive — the rendering-browser rung (headless Chrome) was also
//     blocked: the site rejects automated traffic from this host outright.
//   - Unknown — the site could not be scraped but for reasons that are not
//     block signatures (timeouts, DNS failures, dead site).
//
// scrapeMode is the rung that produced the row; failed/currentError describe
// a Failed row; priorError/priorErrorMode are the last failure an earlier
// pass recorded (mode "" on state files from before modes were tracked,
// treated as plain-HTTP, the conservative reading).
func DeriveBotProtection(scrapeMode string, failed bool, currentError, priorError, priorErrorMode string) string {
	currentBlocked := failed && currentError != "" && item.IsAntibotError(currentError)
	var blockedRung string
	switch {
	case currentBlocked && scrapeMode != "research":
		blockedRung = scrapeMode
	case item.IsAntibotError(priorError) || currentBlocked:
		// Research-rung records only echo earlier scrape errors, so block
		// markers there are attributed to the rung that actually hit them.
		blockedRung = priorErrorMode
		if blockedRung == "" {
			blockedRung = "direct"
		}
	default:
		if failed {
			return BotProtectionUnknown
		}
		// Scraped successfully without ever being blocked; a research-rung
		// success means the site was never fetched, so nothing was observed.
		if scrapeMode == "research" {
			return BotProtectionUnknown
		}
		return BotProtectionNone
	}
	if browserScrapeModes[blockedRung] {
		return BotProtectionAggressive
	}
	return BotProtectionModerate
}

// DomainProcessor runs one website through scrape -> validate -> classify ->
// record, or (for the research rung) research -> classify -> record.
type DomainProcessor struct {
	Config     *config.Config
	Progress   *progress.Tracker
	Scraper    Scraper
	Classifier SiteClassifier
	Reporter   *report.Reporter
	Writer     *ResultsWriter
	// Categories is the optional custom-category client.
	Categories CategoryClassifier
	// DeferFailures is set on auto mode's earlier passes: failures will be
	// retried by a later pass, so they are tracked as retry_pending (not yet
	// processed) rather than terminal errors.
	DeferFailures bool
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func sizeKB(chars int) *float64 {
	v := float64(chars) / 1024
	return &v
}

// ProcessDomain processes one domain through the complete workflow. It
// records the outcome (CSV row + progress entry) and never returns an error;
// a cancelled ctx stops it without recording anything, like the Python
// CancelledError did.
func (p *DomainProcessor) ProcessDomain(ctx context.Context, domain string) {
	ctx = cost.WithItem(ctx)
	start := time.Now()
	var scrape ScrapeResult
	scraped := false
	var cls Classification

	err := func() error {
		domainLog.Debug("Processing " + domain)
		session, err := p.Scraper.AcquireSession(ctx)
		if err != nil {
			return err
		}
		defer p.Scraper.ReleaseSession(session)

		scrape = p.Scraper.Scrape(ctx, session, domain)
		scraped = true
		mode := orDefault(scrape.Mode, p.Scraper.Mode())

		if problem := p.validateScrapeResult(scrape); problem != "" {
			return errors.New("Scrape validation failed: " + problem)
		}
		cls, err = p.Classifier.ClassifySite(ctx, SiteInput{
			Domain:          domain,
			Content:         scrape.Content,
			Title:           scrape.Title,
			MetaDescription: scrape.MetaDescription,
			ContentSource:   "scrape",
		})
		if err != nil {
			return err
		}
		if !cls.Success {
			return errors.New("Classification failed: " + orDefault(cls.Error, "Unknown error"))
		}
		elapsed := time.Since(start)
		if err := p.recordSuccess(ctx, domain, cls, scrape, mode, orDefault(cls.Source, "openrouter"), elapsed); err != nil {
			return err
		}
		p.Reporter.Log(report.Attempt{
			Domain:     domain,
			OK:         true,
			StatusCode: statusCode(scrape.StatusCode),
			Elapsed:    elapsed,
			SizeKB:     sizeKB(scrape.ContentLength),
			Retries:    scrape.RetriesUsed,
			Cache:      orDefault(scrape.CacheStatus, "miss"),
		})
		return nil
	}()
	if err == nil || ctx.Err() != nil {
		return
	}

	elapsed := time.Since(start)
	message := err.Error()
	domainLog.Error(fmt.Sprintf("❌ Failed to process %s: %s", domain, message))
	mode := orDefault(scrape.Mode, p.Scraper.Mode())
	scrapedAt := orDefault(scrape.ScrapedAt, isoNow())
	if err := p.recordFailure(ctx, domain, message, mode, orDefault(cls.Source, "openrouter"), elapsed, scrapedAt, scrape.ContentLength); err != nil {
		domainLog.Error(fmt.Sprintf("Task failed: %v", err))
		return
	}
	attempt := report.Attempt{
		Domain:     domain,
		StatusCode: statusCode(scrape.StatusCode),
		Error:      report.Shorten(message),
		Elapsed:    elapsed,
	}
	if scraped {
		attempt.SizeKB = sizeKB(scrape.ContentLength)
		attempt.Retries = scrape.RetriesUsed
		attempt.Cache = scrape.CacheStatus
	}
	p.Reporter.Log(attempt)
}

func statusCode(code int) any {
	if code == 0 {
		return nil
	}
	return code
}

// ProcessDomainResearch classifies a domain from external research when
// every scrape pass failed.
//
// Auto mode's final rung: a web-search-augmented model researches the
// domain (the target site is never contacted from this host), then the
// regular classifier runs on that research summary. Domains the research
// model knows nothing about stay Failed rather than being guessed at.
func (p *DomainProcessor) ProcessDomainResearch(ctx context.Context, domain string) {
	ctx = cost.WithItem(ctx)
	start := time.Now()
	var cls Classification
	researchContent := ""

	err := func() error {
		domainLog.Debug("Researching " + domain)
		opts := ResearchOptions{
			Model:       p.Config.ResearchModel,
			Temperature: p.Config.ResearchTemperature,
			MaxTokens:   p.Config.ResearchMaxTokens,
		}
		if p.Categories != nil {
			opts.CustomQuestions = p.Categories.ResearchQuestions()
		}
		research, err := p.Classifier.ResearchWebsite(ctx, domain, opts)
		if err != nil {
			return err
		}
		if !research.Success {
			return errors.New("Research failed: " + orDefault(research.Error, "unknown error"))
		}
		researchContent = research.ResearchContent
		if researchContent == "" {
			return errors.New("Research found no meaningful public information about this domain")
		}
		cls, err = p.Classifier.ClassifySite(ctx, SiteInput{Domain: domain, Content: researchContent, ContentSource: "research"})
		if err != nil {
			return err
		}
		if !cls.Success {
			return errors.New("Classification failed: " + orDefault(cls.Error, "Unknown error"))
		}
		elapsed := time.Since(start)
		scrape := ScrapeResult{
			Content:       researchContent,
			ContentLength: utf8.RuneCountInString(researchContent),
			ScrapedAt:     isoNow(),
		}
		if err := p.recordSuccess(ctx, domain, cls, scrape, "research", orDefault(cls.Source, "openrouter"), elapsed); err != nil {
			return err
		}
		p.Reporter.Log(report.Attempt{
			Domain:  domain,
			OK:      true,
			Elapsed: elapsed,
			SizeKB:  sizeKB(utf8.RuneCountInString(researchContent)),
			Cache:   "research",
		})
		return nil
	}()
	if err == nil || ctx.Err() != nil {
		return
	}

	elapsed := time.Since(start)
	// Keep the (more specific) scrape error visible alongside why the
	// research rescue could not finish the job either.
	message := err.Error()
	if prior := p.Progress.Error(domain); prior != "" {
		message = prior + "; research fallback: " + err.Error()
	}
	domainLog.Error(fmt.Sprintf("❌ Failed to process %s: %s", domain, message))
	if err := p.recordFailure(ctx, domain, message, "research", orDefault(cls.Source, "openrouter"), elapsed, isoNow(), utf8.RuneCountInString(researchContent)); err != nil {
		domainLog.Error(fmt.Sprintf("Research fallback task failed for %s: %v", domain, err))
		return
	}
	p.Reporter.Log(report.Attempt{
		Domain:  domain,
		Error:   report.Shorten(message),
		Elapsed: elapsed,
		Cache:   "research",
	})
}

// blockPhrases in the first 512 characters mark a challenge/denial page
// served with a 200.
var blockPhrases = []string{
	"access denied",
	"attention required",
	"captcha",
	"forbidden",
	"just a moment",
	"service unavailable",
	"temporarily unavailable",
}

// validateScrapeResult checks that the scraper returned meaningful page
// content; it returns the problem, or "".
func (p *DomainProcessor) validateScrapeResult(r ScrapeResult) string {
	if !r.Success {
		return orDefault(r.Error, "Scraper reported failure")
	}
	if r.StatusCode != 0 && (r.StatusCode < 200 || r.StatusCode >= 400) {
		return fmt.Sprintf("Received HTTP status %d", r.StatusCode)
	}
	if strings.TrimSpace(r.Content) == "" {
		return "Scraper returned empty content"
	}
	length := r.ContentLength
	if length == 0 {
		length = utf8.RuneCountInString(r.Content)
	}
	if length < p.Config.MinContentLength {
		return fmt.Sprintf("Scraped content too short (%d chars < %d)", length, p.Config.MinContentLength)
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

// websiteRecord starts a WEBSITE row with the app-only columns blank.
func websiteRecord(domain string) *Record {
	rec := NewRecord()
	rec.Set("Domain", domain)
	rec.Set("Type", "WEBSITE")
	for _, k := range []string{"App_Name", "Developer", "Store_Category", "Rating", "Rating_Count", "Downloads"} {
		rec.Set(k, "")
	}
	return rec
}

func (p *DomainProcessor) recordSuccess(ctx context.Context, domain string, cls Classification, scrape ScrapeResult, mode, classifierMode string, elapsed time.Duration) error {
	// An earlier pass's block (e.g. direct got a 403 before the deep crawler
	// got through) is the evidence for the bot-protection level; read it
	// before Mark overwrites it below.
	priorError, priorMode := p.Progress.FailureDetails(domain)
	rec := websiteRecord(domain)
	rec.Set("Quality", cls.Quality)
	rec.Set("Justification", cls.Justification)
	rec.Set("IAB Tier 1", cls.Tier1)
	rec.Set("IAB Tier 2", cls.Tier2)
	rec.Set("IAB Tier 3", cls.Tier3)
	rec.Set("Description", cls.Description)
	rec.Set("Language", orDefault(cls.Language, "Unknown"))
	rec.Set("Political_Leaning", orDefault(cls.PoliticalLeaning, "Non-Political"))
	rec.Set("Audience_Size", orDefault(cls.AudienceSize, "Unknown"))
	rec.Set("Bot_Protection", DeriveBotProtection(mode, false, "", priorError, priorMode))
	rec.Set("Content_Length", scrape.ContentLength)
	rec.Set("Processing_Time", pyRound2(seconds(elapsed)))
	rec.Set("Scrape_Mode", mode)
	rec.Set("Classifier_Mode", classifierMode)
	rec.Set("Scraped_At", orDefault(scrape.ScrapedAt, isoNow()))

	if p.Categories != nil {
		result, err := p.Categories.Classify(ctx, domain, scrape.Content, mode, scrape.Title)
		if err != nil {
			return err
		}
		rec.Update(result)
	}
	if err := writeResult(ctx, p.Writer, rec, p.Categories != nil); err != nil {
		return err
	}
	pt := seconds(elapsed)
	_ = p.Progress.Mark(domain, progress.StatusSuccess, rec, "", &pt, "")
	domainLog.Debug(fmt.Sprintf("✅ %s → %s", domain, cls.Quality))
	return nil
}

func (p *DomainProcessor) recordFailure(ctx context.Context, domain, message, mode, classifierMode string, elapsed time.Duration, scrapedAt string, contentLength int) error {
	priorError, priorMode := p.Progress.FailureDetails(domain)
	rec := websiteRecord(domain)
	rec.Set("Quality", "Failed")
	rec.Set("Justification", "Processing failed: "+message)
	failedClassification(rec)
	rec.Set("Language", "Unknown")
	rec.Set("Political_Leaning", "Non-Political")
	rec.Set("Audience_Size", "Unknown")
	rec.Set("Bot_Protection", DeriveBotProtection(mode, true, message, priorError, priorMode))
	rec.Set("Content_Length", contentLength)
	rec.Set("Processing_Time", pyRound2(seconds(elapsed)))
	rec.Set("Scrape_Mode", mode)
	rec.Set("Classifier_Mode", classifierMode)
	rec.Set("Scraped_At", scrapedAt)
	if err := writeResult(ctx, p.Writer, rec, p.Categories != nil); err != nil {
		return err
	}
	status := progress.StatusError
	if p.DeferFailures {
		status = progress.StatusRetryPending
	}
	pt := seconds(elapsed)
	_ = p.Progress.Mark(domain, status, nil, message, &pt, mode)
	return nil
}

func failedClassification(rec *Record) {
	for _, k := range []string{"IAB Tier 1", "IAB Tier 2", "IAB Tier 3", "Description"} {
		rec.Set(k, "N/A")
	}
}

// writeResult persists a result row: it fills the custom-category status
// for rows that never reached the decision model and stamps the item's
// OpenRouter spend, then appends the row.
func writeResult(ctx context.Context, w *ResultsWriter, rec *Record, customCategories bool) error {
	if w == nil {
		return errors.New("Results writer has not been initialised") //nolint:revive,staticcheck // message kept from the Python release
	}
	if customCategories {
		rec.SetDefault("Decision_Status", "skipped")
		rec.SetDefault("Decision_Error", "Primary analysis failed; custom categories not evaluated")
	}
	rec.Set("Cost_USD", cost.ItemUSD(ctx))
	return w.WriteRow(rec)
}
