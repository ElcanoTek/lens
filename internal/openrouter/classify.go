// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package openrouter

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ElcanoTek/lens/internal/categories/pyjson"
	"github.com/ElcanoTek/lens/internal/config"
)

// Content sources for [SiteInput.Source].
const (
	// SourceScrape: Content is the site's own page text.
	SourceScrape = "scrape"
	// SourceResearch: Content is an external research summary about a site
	// that blocks every crawler.
	SourceResearch = "research"
)

// ResearchInsufficient is the sentinel the research model is told to emit
// when it finds nothing; its presence keeps the item Failed instead of
// fabricating a classification.
const ResearchInsufficient = "INSUFFICIENT INFORMATION"

// Classification is the result of ClassifySite, ClassifyApp and
// ClassifyCTVApp. Field mapping from the Python release's result dict:
//
//	success          -> Success           error            -> Error
//	quality          -> Quality           justification    -> Justification
//	vertical         -> Vertical          vertical_tier_1..3 -> VerticalTier1..3
//	description      -> Description       confidence       -> Confidence
//	language         -> Language          political_leaning -> PoliticalLeaning
//	audience_size    -> AudienceSize      processing_time  -> ProcessingTime
//	tokens_used      -> TokensUsed        source           -> Source (website/app: "openrouter")
//	target_audience  -> TargetAudience    content_type     -> ContentType      (CTV only)
//	network_affiliation -> NetworkAffiliation  model       -> Model            (CTV only)
//
// On failure Success is false, Error holds the reason, Quality is "Error"
// and Justification is "Classification failed: <reason>"; the processors
// record such items as Failed. Successful results always have every field
// set (validated against the allowed values).
type Classification struct {
	Success       bool
	Error         string
	Quality       string
	Justification string
	// Vertical is the resolved hierarchy ("Sports > Baseball"), or the
	// model's raw label when it matched nothing.
	Vertical         string
	VerticalTier1    string
	VerticalTier2    string
	VerticalTier3    string
	Description      string
	Confidence       string
	Language         string
	PoliticalLeaning string
	AudienceSize     string

	TargetAudience     string
	ContentType        string
	NetworkAffiliation string

	ProcessingTime time.Duration
	TokensUsed     int
	Source         string
	Model          string
}

// Research is the result of ResearchWebsite and ResearchCTVApp. Python
// keys: success, error, research_content (Content), processing_time,
// tokens_used, model.
type Research struct {
	Success bool
	Error   string
	// Content is the research summary. ResearchWebsite leaves it empty when
	// the model found nothing meaningful; ResearchCTVApp returns it as is
	// and the CTV processor must reject empty/insufficient evidence itself
	// (see IsInsufficientResearch).
	Content        string
	ProcessingTime time.Duration
	TokensUsed     int
	Model          string
}

// SiteInput is what ClassifySite classifies.
type SiteInput struct {
	Domain          string
	Content         string
	Title           string
	MetaDescription string
	// Source is SourceScrape (default) or SourceResearch.
	Source string
}

// AppInput is what ClassifyApp classifies (store metadata).
type AppInput struct {
	AppID         string
	AppName       string
	Developer     string
	StoreCategory string
	Description   string
	Rating        float64
	RatingCount   int
	// Downloads is Android only.
	Downloads     string
	ContentForLLM string
	// Platform is "ios" or "android".
	Platform string
}

// CTVApp identifies a CTV app for research and classification. Publisher is
// the input file's publisher, passed into both prompts.
type CTVApp struct {
	AppName   string
	BundleID  string
	Platform  string
	URL       string
	Publisher string
}

// ResearchOptions configure a research request. Model "" is
// config.DefaultResearchModel; MaxTokens 0 is 1500 for websites and 2000 for
// CTV. Temperature is used as given (config: research_temperature 0.2,
// ctv_research_temperature 0.3).
type ResearchOptions struct {
	Model       string
	Temperature float64
	MaxTokens   int
	// CustomQuestions are the custom categories' evidence needs
	// (categories.DecisionCategories.ResearchQuestions).
	CustomQuestions []string
}

// CTVClassifyOptions configure ClassifyCTVApp (config:
// ctv_classification_temperature, ctv_classification_max_tokens). MaxTokens
// 0 is 1500. The model is always the client's active model.
type CTVClassifyOptions struct {
	Temperature float64
	MaxTokens   int
}

func cutContent(content string, limit int) string {
	if cut, truncated := truncateRunes(content, limit); truncated {
		return cut + "..."
	}
	return content
}

func failedClassification(err error, elapsed time.Duration) Classification {
	return Classification{
		Error:            err.Error(),
		ProcessingTime:   elapsed,
		Quality:          "Error",
		Justification:    "Classification failed: " + err.Error(),
		Vertical:         "Unknown",
		Description:      "N/A",
		Language:         "Unknown",
		PoliticalLeaning: "Non-Political",
		AudienceSize:     "Unknown",
		Source:           "openrouter",
	}
}

func buildClassificationPrompt(in SiteInput) string {
	// Research summaries are denser than page text; give them more room.
	limit := 2000
	if in.Source == SourceResearch {
		limit = 3000
	}
	content := cutContent(in.Content, limit)
	var info string
	if in.Source == SourceResearch {
		info = siteResearchInfo(in.Domain, content)
	} else {
		info = siteScrapeInfo(in.Domain, in.Title, in.MetaDescription, content)
	}
	return sitePrompt(info, TaxonomyPrompt())
}

// classificationBody builds a function-calling request for the active model.
func (c *Client) classificationBody(system, prompt string, tool pyjson.Object, toolName string,
	temperature float64, budget int, sampling bool,
) pyjson.Object {
	model := c.Model()
	body := pyjson.Object{
		{Key: "model", Value: model},
		{Key: "messages", Value: messages(system, prompt)},
		{Key: "tools", Value: []any{tool}},
		{Key: "tool_choice", Value: toolChoice(toolName)},
		{Key: "temperature", Value: temperature},
		{Key: "max_tokens", Value: budget},
	}
	if sampling {
		body = append(body,
			pyjson.Member{Key: "top_p", Value: 0.9},
			pyjson.Member{Key: "frequency_penalty", Value: 0.1},
			pyjson.Member{Key: "presence_penalty", Value: 0.1})
	}
	return append(body, classificationOptions(model)...)
}

// ClassifySite classifies a website from its scraped page text or, with
// Source SourceResearch, from a research summary.
func (c *Client) ClassifySite(ctx context.Context, in SiteInput) Classification {
	start := time.Now()
	prompt := buildClassificationPrompt(in)
	result, resp, err := c.requestAndParse(ctx, func(budget int) pyjson.Object {
		return c.classificationBody(
			"You are a professional website quality analyst. Provide accurate, consistent classifications based on the given criteria.",
			prompt, siteSchema(), "classify_website", c.temperature, budget, true)
	}, func(r pyjson.Object) (Classification, error) {
		return parseClassification(r, "classify_website")
	}, fmt.Sprintf("Site classification (%s)", in.Domain), 0)
	if err != nil {
		log.Error(fmt.Sprintf("Failed to classify %s via OpenRouter: %v", in.Domain, err))
		return failedClassification(err, time.Since(start))
	}
	result.Success = true
	result.ProcessingTime = time.Since(start)
	result.TokensUsed = usageTokens(resp)
	result.Source = "openrouter"
	return result
}

// formatThousands is Python's format(n, ",").
func formatThousands(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

func buildAppPrompt(in AppInput) string {
	description := cutContent(in.Description, 2000)
	platformName := "Google Play Store"
	if in.Platform == "ios" {
		platformName = "iOS App Store"
	}
	downloads := ""
	if in.Downloads != "" {
		downloads = "\nDownloads: " + in.Downloads
	}
	descriptionText := description
	if descriptionText == "" {
		descriptionText = "(No description available)"
	}
	additional := ""
	if in.ContentForLLM != "" && in.ContentForLLM != description {
		additional = "Additional Context:\n" + in.ContentForLLM
	}
	return appPrompt(platformName, in.AppID, in.AppName, in.Developer, in.StoreCategory,
		strconv.FormatFloat(in.Rating, 'f', 1, 64), formatThousands(in.RatingCount),
		downloads, descriptionText, additional, TaxonomyPrompt())
}

// ClassifyApp classifies an iOS or Android app from its store metadata.
func (c *Client) ClassifyApp(ctx context.Context, in AppInput) Classification {
	start := time.Now()
	prompt := buildAppPrompt(in)
	result, resp, err := c.requestAndParse(ctx, func(budget int) pyjson.Object {
		return c.classificationBody(
			"You are a professional mobile app quality analyst. Provide accurate, consistent classifications based on the given criteria.",
			prompt, appSchema(), "classify_app", c.temperature, budget, true)
	}, func(r pyjson.Object) (Classification, error) {
		return parseClassification(r, "classify_app")
	}, fmt.Sprintf("App classification (%s)", in.AppID), 0)
	if err != nil {
		log.Error(fmt.Sprintf("Failed to classify app %s via OpenRouter: %v", in.AppID, err))
		return failedClassification(err, time.Since(start))
	}
	result.Success = true
	result.ProcessingTime = time.Since(start)
	result.TokensUsed = usageTokens(resp)
	result.Source = "openrouter"
	return result
}

// customResearchInstructions asks the research model for evidence on the
// custom questions, without letting it decide the labels.
func customResearchInstructions(questions []string) string {
	if len(questions) == 0 {
		return ""
	}
	return "\n\nAlso gather factual evidence relevant to these user-defined categorization questions " +
		"within the summary budget. Do not assign labels; a separate step evaluates them. " +
		"Distinguish observed facts from missing information; explicitly say when evidence " +
		"is unavailable. If the entity itself cannot be identified, follow the insufficient-information rule.\n" +
		pyjson.DumpsUnicode(questions)
}

func pyStrip(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) })
}

// IsInsufficientResearch reports whether a research summary is unusable:
// empty, whitespace or exactly the INSUFFICIENT INFORMATION sentinel (case
// and surrounding whitespace ignored). A longer summary that merely
// mentions the sentinel about one detail is usable. The CTV processor uses
// this before classifying ("No meaningful research evidence available").
func IsInsufficientResearch(content string) bool {
	s := pyStrip(content)
	return s == "" || strings.ToUpper(s) == ResearchInsufficient
}

func (c *Client) research(ctx context.Context, system, prompt string, opts ResearchOptions, defaultTokens int, op string) (string, pyjson.Object, error) {
	model := opts.Model
	if model == "" {
		model = config.DefaultResearchModel
	}
	maxTokens := opts.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultTokens
	}
	prompt += customResearchInstructions(opts.CustomQuestions)
	resp, err := c.callWithRetry(ctx, func() pyjson.Object {
		return pyjson.Object{
			{Key: "model", Value: model},
			{Key: "messages", Value: messages(system, prompt)},
			{Key: "temperature", Value: opts.Temperature},
			{Key: "max_tokens", Value: maxTokens},
		}
	}, op)
	if err != nil {
		return model, nil, err
	}
	c.countRequest(usageTokens(resp))
	return model, resp, nil
}

// ResearchWebsite researches a website via a web-search-augmented model.
//
// It is the last rung of the auto-mode ladder: for sites every crawler
// failed on (typically hardened anti-bot publishers), it gathers public
// information about the domain so it can still be classified. The target
// site is never contacted from this host, so this pass adds zero ban risk.
// Content is empty when the model found nothing meaningful (the sentinel,
// or under 100 characters), and the item then stays Failed.
func (c *Client) ResearchWebsite(ctx context.Context, domain string, opts ResearchOptions) Research {
	start := time.Now()
	model, resp, err := c.research(ctx,
		"You are a digital-media research analyst. Provide accurate, sourced information about websites, their publishers, content, reputation, and audience.",
		websiteResearchPrompt(domain), opts, 1500, fmt.Sprintf("Website research (%s)", domain))
	if err != nil {
		log.Error(fmt.Sprintf("Failed to research website %s: %v", domain, err))
		return Research{Error: err.Error(), ProcessingTime: time.Since(start), Model: model}
	}
	content := messageContent(extractMessage(resp))
	stripped := pyStrip(content)
	if strings.ToUpper(stripped) == ResearchInsufficient || utf8.RuneCountInString(stripped) < 100 {
		log.Info(fmt.Sprintf("Research found no meaningful information for %s", domain))
		content = ""
	}
	return Research{
		Success: true, Content: content, ProcessingTime: time.Since(start),
		TokensUsed: usageTokens(resp), Model: model,
	}
}

func optionalLine(label, value string) string {
	if value == "" {
		return ""
	}
	return label + value
}

func buildCTVResearchPrompt(app CTVApp) string {
	return ctvResearchPrompt(app.AppName,
		optionalLine("Bundle ID: ", app.BundleID),
		optionalLine("Platform: ", app.Platform),
		optionalLine("Publisher/Network: ", app.Publisher),
		optionalLine("Associated URL: ", app.URL))
}

func buildCTVClassificationPrompt(app CTVApp, research string) string {
	return ctvClassificationPrompt(app.AppName,
		optionalLine("Bundle ID: ", app.BundleID),
		optionalLine("Platform: ", app.Platform),
		optionalLine("Publisher: ", app.Publisher),
		optionalLine("URL: ", app.URL),
		research, TaxonomyPrompt())
}

// ResearchCTVApp is step one of the two-step CTV pipeline: a web-search
// model gathers the app's content, network affiliation, audience and brand
// safety, so a cheaper model can structure it.
func (c *Client) ResearchCTVApp(ctx context.Context, app CTVApp, opts ResearchOptions) Research {
	start := time.Now()
	model, resp, err := c.research(ctx,
		"You are a CTV (Connected TV) industry research analyst. Provide detailed, accurate information about CTV applications, their content offerings, network affiliations, and audience reach.",
		buildCTVResearchPrompt(app), opts, 2000, fmt.Sprintf("CTV research (%s)", app.AppName))
	if err != nil {
		log.Error(fmt.Sprintf("Failed to research CTV app %s: %v", app.AppName, err))
		return Research{Error: err.Error(), ProcessingTime: time.Since(start), Model: model}
	}
	return Research{
		Success: true, Content: messageContent(extractMessage(resp)), ProcessingTime: time.Since(start),
		TokensUsed: usageTokens(resp), Model: model,
	}
}

// ClassifyCTVApp is step two of the CTV pipeline: it structures the
// research into a classification with the client's active model, then
// applies the deterministic news routing (ApplyCTVRouting).
func (c *Client) ClassifyCTVApp(ctx context.Context, app CTVApp, research string, opts CTVClassifyOptions) Classification {
	start := time.Now()
	maxTokens := opts.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 1500
	}
	prompt := buildCTVClassificationPrompt(app, research)
	result, resp, err := c.requestAndParse(ctx, func(budget int) pyjson.Object {
		return c.classificationBody(
			"You are a CTV (Connected TV) advertising quality analyst. Classify CTV apps for advertising suitability based on the research provided.",
			prompt, ctvSchema(), "classify_ctv_app", opts.Temperature, budget, false)
	}, parseCTVClassification, fmt.Sprintf("CTV classification (%s)", app.AppName), maxTokens)
	if err != nil {
		log.Error(fmt.Sprintf("Failed to classify CTV app %s: %v", app.AppName, err))
		failed := failedClassification(err, time.Since(start))
		failed.TargetAudience = "Unknown"
		failed.ContentType = "Unknown"
		failed.Confidence = "Low"
		failed.Source = ""
		failed.Model = c.Model()
		return failed
	}
	result = ApplyCTVRouting(app.AppName, research, result)
	result.Success = true
	result.ProcessingTime = time.Since(start)
	result.TokensUsed = usageTokens(resp)
	result.Model = c.Model()
	return result
}
