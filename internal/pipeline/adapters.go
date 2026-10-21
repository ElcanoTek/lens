// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package pipeline

import (
	"context"
	"time"

	"github.com/ElcanoTek/lens/internal/categories"
	"github.com/ElcanoTek/lens/internal/config"
	"github.com/ElcanoTek/lens/internal/openrouter"
	"github.com/ElcanoTek/lens/internal/scrape"
)

// This file adapts the real clients to the pipeline's interfaces.

// closeTimeout bounds teardown work (stopping the Chrome container) that
// must still run after the run's context was cancelled by a signal.
const closeTimeout = 2 * time.Minute

func secondsDuration(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// ---- OpenRouter ----

type llmAdapter struct{ c *openrouter.Client }

func defaultNewLLM(_ context.Context, opts LLMOptions) (LLM, error) {
	c, err := openrouter.New(openrouter.Options{
		APIKey:        opts.APIKey,
		Model:         opts.Model,
		FallbackModel: opts.FallbackModel,
		Temperature:   opts.Temperature,
		MaxTokens:     opts.MaxTokens,
		MaxRetries:    opts.MaxRetries,
		Timeout:       secondsDuration(opts.Timeout),
	})
	if err != nil {
		return nil, err
	}
	return NewLLMAdapter(c), nil
}

// NewLLMAdapter wraps an OpenRouter client as the pipeline's LLM (tests
// point the client at an httptest server).
func NewLLMAdapter(c *openrouter.Client) LLM { return llmAdapter{c: c} }

func fromClassification(c openrouter.Classification) Classification {
	return Classification{
		Success:            c.Success,
		Error:              c.Error,
		Quality:            c.Quality,
		Justification:      c.Justification,
		Tier1:              c.VerticalTier1,
		Tier2:              c.VerticalTier2,
		Tier3:              c.VerticalTier3,
		Description:        c.Description,
		Language:           c.Language,
		PoliticalLeaning:   c.PoliticalLeaning,
		AudienceSize:       c.AudienceSize,
		Source:             c.Source,
		TargetAudience:     c.TargetAudience,
		ContentType:        c.ContentType,
		NetworkAffiliation: c.NetworkAffiliation,
	}
}

func fromResearch(r openrouter.Research) Research {
	return Research{Success: r.Success, Error: r.Error, ResearchContent: r.Content}
}

func (a llmAdapter) TestConnection(ctx context.Context) bool { return a.c.TestConnection(ctx) }

func (a llmAdapter) ClassifySite(ctx context.Context, in SiteInput) (Classification, error) {
	source := openrouter.SourceScrape
	if in.ContentSource == "research" {
		source = openrouter.SourceResearch
	}
	return fromClassification(a.c.ClassifySite(ctx, openrouter.SiteInput{
		Domain:          in.Domain,
		Content:         in.Content,
		Title:           in.Title,
		MetaDescription: in.MetaDescription,
		Source:          source,
	})), nil
}

func (a llmAdapter) ResearchWebsite(ctx context.Context, domain string, opts ResearchOptions) (Research, error) {
	return fromResearch(a.c.ResearchWebsite(ctx, domain, openrouter.ResearchOptions{
		Model:           opts.Model,
		Temperature:     opts.Temperature,
		MaxTokens:       opts.MaxTokens,
		CustomQuestions: opts.CustomQuestions,
	})), nil
}

func (a llmAdapter) ClassifyApp(ctx context.Context, in AppInput) (Classification, error) {
	return fromClassification(a.c.ClassifyApp(ctx, openrouter.AppInput{
		AppID:         in.AppID,
		AppName:       in.AppName,
		Developer:     in.Developer,
		StoreCategory: in.StoreCategory,
		Description:   in.Description,
		Rating:        in.Rating,
		RatingCount:   in.RatingCount,
		Downloads:     in.Downloads,
		ContentForLLM: in.ContentForLLM,
		Platform:      in.Platform,
	})), nil
}

func (a llmAdapter) ResearchCTVApp(ctx context.Context, in CTVResearchInput) (Research, error) {
	return fromResearch(a.c.ResearchCTVApp(ctx, openrouter.CTVApp{
		AppName:   in.AppName,
		BundleID:  in.BundleID,
		Platform:  in.Platform,
		URL:       in.URL,
		Publisher: in.Publisher,
	}, openrouter.ResearchOptions{
		Model:           in.Model,
		Temperature:     in.Temperature,
		MaxTokens:       in.MaxTokens,
		CustomQuestions: in.CustomQuestions,
	})), nil
}

func (a llmAdapter) ClassifyCTVApp(ctx context.Context, in CTVClassifyInput) (Classification, error) {
	// The client classifies with its active model, which the orchestrator
	// configured as ctv_classification_model (with its fallback).
	return fromClassification(a.c.ClassifyCTVApp(ctx, openrouter.CTVApp{
		AppName:   in.AppName,
		BundleID:  in.BundleID,
		Platform:  in.Platform,
		URL:       in.URL,
		Publisher: in.Publisher,
	}, in.ResearchContent, openrouter.CTVClassifyOptions{
		Temperature: in.Temperature,
		MaxTokens:   in.MaxTokens,
	})), nil
}

// ---- Scraper ----

type scraperAdapter struct{ c *scrape.Client }

func defaultNewScraper(ctx context.Context, cfg *config.Config, opts ScraperOptions) (ScraperBackend, error) {
	return StartScraper(ctx, scrape.OptionsFromConfig(cfg, opts.Mode, opts.RejectRedirects))
}

// StartScraper creates and starts a scrape client (direct session, or the
// deep mode's Chrome container) wrapped as a ScraperBackend.
func StartScraper(ctx context.Context, o scrape.Options) (ScraperBackend, error) {
	c := scrape.New(o)
	if err := c.Start(ctx); err != nil {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
		defer cancel()
		_ = c.Close(closeCtx)
		return nil, err
	}
	return scraperAdapter{c: c}, nil
}

func (a scraperAdapter) AcquireSession(ctx context.Context) (Session, error) {
	return a.c.AcquireSession(ctx)
}

func (a scraperAdapter) ReleaseSession(s Session) {
	if sess, ok := s.(*scrape.Session); ok {
		a.c.ReleaseSession(sess)
	}
}

func (a scraperAdapter) Scrape(ctx context.Context, s Session, domain string) ScrapeResult {
	sess, _ := s.(*scrape.Session)
	r := a.c.Scrape(ctx, sess, domain)
	if r == nil {
		return ScrapeResult{Error: "Scraper returned no data"}
	}
	return ScrapeResult{
		Success:         r.Success,
		Error:           r.Error,
		URL:             r.URL,
		Title:           r.Title,
		Content:         r.Content,
		MetaDescription: r.MetaDescription,
		ContentLength:   r.ContentLength,
		StatusCode:      r.StatusCode,
		Mode:            r.Mode,
		ScrapedAt:       r.ScrapedAt,
		RetriesUsed:     r.RetriesUsed,
		CacheStatus:     r.CacheStatus,
	}
}

func (a scraperAdapter) Mode() string { return a.c.Mode() }

func (a scraperAdapter) CreateSessionPool(ctx context.Context, n int) error {
	return a.c.CreateSessionPool(ctx, n)
}

func (a scraperAdapter) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()
	return a.c.Close(ctx)
}

// ---- Custom categories ----

func defaultNewCategories(spec *categories.Spec, model string) (CategoryClassifier, error) {
	return categories.New(spec, model, categories.Options{})
}
