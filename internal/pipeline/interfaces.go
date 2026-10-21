// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package pipeline

import (
	"context"

	"github.com/ElcanoTek/lens/internal/categories"
)

// The processors and the orchestrator depend on the small interfaces below,
// shaped after what the Python processors called on their clients, so they
// can be exercised with fakes. adapters.go builds the real implementations
// (internal/scrape, internal/stores, internal/openrouter,
// internal/categories) from config.Config.
//
// Python clients reported most failures as {"success": False, "error": ...}
// dicts and raised only for unexpected problems. The Go results keep that
// split: Success=false with Error is a reported failure (the processor wraps
// it, e.g. "Classification failed: <error>"); a returned error is the
// "raised" case and its text is used as is.

// Session is one slot of a scraper's session pool (a headless-Chrome
// session in deep mode, a plain token in direct mode).
type Session any

// ScrapeResult is what one page fetch produced.
type ScrapeResult struct {
	Success         bool
	Error           string
	URL             string
	Title           string
	Content         string
	MetaDescription string
	// ContentLength is the page text's length in characters (Python len()).
	// Zero with non-empty Content means "not reported": validation then
	// measures Content itself.
	ContentLength int
	// StatusCode is the HTTP status; 0 when not reported.
	StatusCode  int
	Mode        string // "direct" or "deep"; "" means the scraper's Mode()
	ScrapedAt   string // Python isoformat; "" means now
	RetriesUsed int
	CacheStatus string
}

// Scraper fetches websites through a bounded session pool.
type Scraper interface {
	// AcquireSession blocks for a free pool slot (get_available_session).
	AcquireSession(ctx context.Context) (Session, error)
	ReleaseSession(s Session)
	// Scrape fetches domain. Fetch failures are reported in the result.
	Scrape(ctx context.Context, s Session, domain string) ScrapeResult
	// Mode is "direct" or "deep".
	Mode() string
}

// ScraperBackend is a started scraper the orchestrator owns.
type ScraperBackend interface {
	Scraper
	// CreateSessionPool sizes the pool (deep mode caps it at one).
	CreateSessionPool(ctx context.Context, n int) error
	// Close releases sessions and stops a private Chrome container.
	Close() error
}

// ScraperOptions selects a backend and its redirect policy for one pass.
type ScraperOptions struct {
	Mode            string
	RejectRedirects bool
}

// Classification is the classifier's structured answer.
type Classification struct {
	Success          bool
	Error            string
	Quality          string
	Justification    string
	Tier1            string
	Tier2            string
	Tier3            string
	Description      string
	Language         string
	PoliticalLeaning string
	AudienceSize     string
	// Source is the classifier backend ("openrouter").
	Source string
	// CTV-only fields.
	TargetAudience     string
	ContentType        string
	NetworkAffiliation string
}

// SiteInput is classify_site's arguments.
type SiteInput struct {
	Domain          string
	Content         string
	Title           string
	MetaDescription string
	// ContentSource is "scrape" (the site's own text) or "research" (an
	// external research summary about the site).
	ContentSource string
}

// AppInput is classify_app's arguments.
type AppInput struct {
	AppID         string
	AppName       string
	Developer     string
	StoreCategory string
	Description   string
	Rating        float64
	RatingCount   int
	Downloads     string
	ContentForLLM string
	Platform      string // "ios" or "android"
}

// Research is a research step's outcome. ResearchContent is empty when the
// model found nothing meaningful.
type Research struct {
	Success         bool
	Error           string
	ResearchContent string
}

// ResearchOptions configures a research call.
type ResearchOptions struct {
	Model           string
	Temperature     float64
	MaxTokens       int
	CustomQuestions []string
}

// CTVResearchInput is research_ctv_app's arguments.
type CTVResearchInput struct {
	AppName   string
	BundleID  string
	Platform  string
	URL       string
	Publisher string
	ResearchOptions
}

// CTVClassifyInput is classify_ctv_app's arguments.
type CTVClassifyInput struct {
	AppName         string
	ResearchContent string
	BundleID        string
	Platform        string
	URL             string
	Publisher       string
	Model           string
	Temperature     float64
	MaxTokens       int
}

// SiteClassifier is what the website processor needs.
type SiteClassifier interface {
	ClassifySite(ctx context.Context, in SiteInput) (Classification, error)
	ResearchWebsite(ctx context.Context, domain string, opts ResearchOptions) (Research, error)
}

// AppClassifier is what the app processor needs.
type AppClassifier interface {
	ClassifyApp(ctx context.Context, in AppInput) (Classification, error)
}

// CTVClassifier is what the CTV processor needs.
type CTVClassifier interface {
	ResearchCTVApp(ctx context.Context, in CTVResearchInput) (Research, error)
	ClassifyCTVApp(ctx context.Context, in CTVClassifyInput) (Classification, error)
}

// LLM is the full OpenRouter client the orchestrator initialises.
type LLM interface {
	SiteClassifier
	AppClassifier
	CTVClassifier
	// TestConnection sends a tiny request (swapping to the fallback model
	// when the primary is unavailable) and reports whether it worked.
	TestConnection(ctx context.Context) bool
}

// LLMOptions configures the OpenRouter client for a workflow.
type LLMOptions struct {
	APIKey        string
	Model         string
	Temperature   float64
	MaxTokens     int
	MaxRetries    int
	Timeout       float64 // seconds; 0 = client default
	FallbackModel string
}

// AppMetadata is what a store client fetched about one app.
type AppMetadata struct {
	Success      bool
	Error        string
	AppName      string
	Developer    string
	PrimaryGenre string // iOS
	Category     string // Android
	Description  string
	// Rating and RatingCount are nil when the store reported none (the
	// cells are then blank).
	Rating        *float64
	RatingCount   *int
	Downloads     string // Android
	ContentForLLM string
	FetchedAt     string
}

// StoreClient fetches app metadata (iTunes API or Play Store page).
type StoreClient interface {
	FetchAppMetadata(ctx context.Context, id string) (AppMetadata, error)
}

// CategoryClassifier answers custom-category questions for one item; the
// real implementation is *categories.DecisionCategories.
type CategoryClassifier interface {
	ResearchQuestions() []string
	Classify(ctx context.Context, identifier, content, source, title string) (categories.Result, error)
}
