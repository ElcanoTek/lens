// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package pipeline

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/ElcanoTek/lens/internal/cache"
	"github.com/ElcanoTek/lens/internal/config"
	"github.com/ElcanoTek/lens/internal/cost"
	"github.com/ElcanoTek/lens/internal/logx"
	"github.com/ElcanoTek/lens/internal/progress"
	"github.com/ElcanoTek/lens/internal/report"
	"github.com/ElcanoTek/lens/internal/scrape"
)

var domainLog = logx.New("domain_processing")

// DeriveBotProtection is scrape.DeriveBotProtection with positional
// arguments: the Bot_Protection level (None Detected / Moderate / Aggressive
// / Unknown) from which crawler rungs the site blocked during this run.
func DeriveBotProtection(scrapeMode string, failed bool, currentError, priorError, priorErrorMode string) string {
	return scrape.DeriveBotProtection(scrape.BotEvidence{
		ScrapeMode:     scrapeMode,
		Failed:         failed,
		CurrentError:   currentError,
		PriorError:     priorError,
		PriorErrorMode: priorErrorMode,
	})
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
	// Cache is the optional cross-job SQLite cache.
	Cache *cache.Store
	// AutoMode is true when running the auto ladder (direct -> deep -> research).
	AutoMode bool
	// RejectRedirects is true when cross-host redirects are rejected on this pass.
	RejectRedirects bool
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
		if p.Cache != nil {
			if entry, ok := p.Cache.Get(ctx, cache.TypeWebsite, domain); ok && p.cacheEntryUsable(entry) {
				return p.processFromCache(ctx, domain, entry, start)
			}
		}
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

// validateScrapeResult checks that the scraper returned meaningful page
// content (scrape.ValidateResult: status, emptiness, min_content_length,
// challenge-page phrases); it returns the problem, or "".
func (p *DomainProcessor) validateScrapeResult(r ScrapeResult) string {
	return scrape.ValidateResult(&scrape.Result{
		Success:       r.Success,
		Error:         r.Error,
		Content:       r.Content,
		ContentLength: r.ContentLength,
		StatusCode:    r.StatusCode,
	}, p.Config.MinContentLength)
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

func llmCacheKey(model string, temp float64, maxTokens int) string {
	return fmt.Sprintf("%s|t=%.4g|m=%d", model, temp, maxTokens)
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
	botProtection := DeriveBotProtection(mode, false, "", priorError, priorMode)
	if scrape.BotProtection != "" {
		botProtection = scrape.BotProtection
	}
	rec.Set("Bot_Protection", botProtection)
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
	if p.Cache != nil && scrape.CacheStatus != "hit" {
		var researchQuestions []string
		researchModel := ""
		if mode == "research" {
			researchModel = llmCacheKey(p.Config.ResearchModel, p.Config.ResearchTemperature, p.Config.ResearchMaxTokens)
			if p.Categories != nil {
				researchQuestions = p.Categories.ResearchQuestions()
			}
		}
		_ = p.Cache.Put(ctx, cache.Entry{
			ContentType:       cache.TypeWebsite,
			Identifier:        domain,
			LLMModel:          llmCacheKey(orDefault(cls.Model, p.Config.LLMModel), p.Config.LLMTemperature, p.Config.LLMMaxTokens),
			ScrapeMode:        mode,
			RejectRedirects:   p.RejectRedirects,
			BotProtection:     rec.String("Bot_Protection"),
			Title:             scrape.Title,
			MetaDescription:   scrape.MetaDescription,
			Content:           scrape.Content,
			ContentLength:     scrape.ContentLength,
			ResearchModel:     researchModel,
			ResearchQuestions: researchQuestions,
			Classification: cache.Classification{
				Quality:          cls.Quality,
				Justification:    cls.Justification,
				Tier1:            cls.Tier1,
				Tier2:            cls.Tier2,
				Tier3:            cls.Tier3,
				Description:      cls.Description,
				Language:         orDefault(cls.Language, "Unknown"),
				PoliticalLeaning: orDefault(cls.PoliticalLeaning, "Non-Political"),
				AudienceSize:     orDefault(cls.AudienceSize, "Unknown"),
				Source:           classifierMode,
			},
			ScrapedAt: orDefault(scrape.ScrapedAt, isoNow()),
			UpdatedAt: scrape.CachedAt,
		})
	}
	domainLog.Debug(fmt.Sprintf("✅ %s → %s", domain, cls.Quality))
	return nil
}

// cacheEntryUsable checks whether a cached website entry can satisfy the
// current run:
//   - Non-auto runs (--scrape-mode direct or deep) only reuse entries produced
//     by that exact backend.
//   - Research-mode entries are only reused in auto mode when research fallback
//     is enabled, the research model matches, and any custom-category questions
//     match.
func (p *DomainProcessor) cacheEntryUsable(entry *cache.Entry) bool {
	if !p.AutoMode {
		if p.Scraper == nil || entry.ScrapeMode != p.Scraper.Mode() {
			return false
		}
		// Never serve a redirect-following entry to a run that rejects redirects.
		if p.RejectRedirects && !entry.RejectRedirects {
			return false
		}
		return true
	}
	if entry.ScrapeMode != "research" {
		if entry.ScrapeMode == "direct" && p.RejectRedirects && !entry.RejectRedirects {
			return false
		}
		return true
	}
	wantResearch := llmCacheKey(p.Config.ResearchModel, p.Config.ResearchTemperature, p.Config.ResearchMaxTokens)
	if !p.Config.ResearchFallbackEnabled || entry.ResearchModel != wantResearch {
		return false
	}
	if p.Categories == nil {
		return len(entry.ResearchQuestions) == 0
	}
	return slices.Equal(entry.ResearchQuestions, p.Categories.ResearchQuestions())
}

func (p *DomainProcessor) processFromCache(ctx context.Context, domain string, entry *cache.Entry, start time.Time) error {
	sr := ScrapeResult{
		Success:         true,
		Title:           entry.Title,
		MetaDescription: entry.MetaDescription,
		Content:         entry.Content,
		ContentLength:   entry.ContentLength,
		Mode:            orDefault(entry.ScrapeMode, "direct"),
		ScrapedAt:       orDefault(entry.ScrapedAt, isoNow()),
		BotProtection:   entry.BotProtection,
		CacheStatus:     "hit",
		CachedAt:        entry.UpdatedAt,
	}
	var cls Classification
	wantKey := llmCacheKey(p.Config.LLMModel, p.Config.LLMTemperature, p.Config.LLMMaxTokens)
	if entry.LLMModel == wantKey && entry.Classification.Quality != "" {
		cls = Classification{
			Success:          true,
			Quality:          entry.Classification.Quality,
			Justification:    entry.Classification.Justification,
			Tier1:            entry.Classification.Tier1,
			Tier2:            entry.Classification.Tier2,
			Tier3:            entry.Classification.Tier3,
			Description:      entry.Classification.Description,
			Language:         entry.Classification.Language,
			PoliticalLeaning: entry.Classification.PoliticalLeaning,
			AudienceSize:     entry.Classification.AudienceSize,
			Source:           orDefault(entry.Classification.Source, "openrouter"),
		}
	} else {
		source := "scrape"
		if sr.Mode == "research" {
			source = "research"
		}
		var err error
		cls, err = p.Classifier.ClassifySite(ctx, SiteInput{
			Domain:          domain,
			Content:         sr.Content,
			Title:           sr.Title,
			MetaDescription: sr.MetaDescription,
			ContentSource:   source,
		})
		if err != nil {
			return err
		}
		if !cls.Success {
			return errors.New("Classification failed: " + orDefault(cls.Error, "Unknown error"))
		}
		// Refresh cache entry with the new model's classification.
		sr.CacheStatus = ""
	}
	elapsed := time.Since(start)
	if err := p.recordSuccess(ctx, domain, cls, sr, sr.Mode, orDefault(cls.Source, "openrouter"), elapsed); err != nil {
		return err
	}
	p.Reporter.Log(report.Attempt{
		Domain:  domain,
		OK:      true,
		Elapsed: elapsed,
		SizeKB:  sizeKB(sr.ContentLength),
		Cache:   "hit",
	})
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
		return errors.New("Results writer has not been initialised") //nolint:staticcheck // message kept from the Python release
	}
	if customCategories {
		rec.SetDefault("Decision_Status", "skipped")
		rec.SetDefault("Decision_Error", "Primary analysis failed; custom categories not evaluated")
	}
	rec.Set("Cost_USD", cost.ItemUSD(ctx))
	return w.WriteRow(rec)
}
