// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package pipeline

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ElcanoTek/lens/internal/cost"
	"github.com/ElcanoTek/lens/internal/detect"
	"github.com/ElcanoTek/lens/internal/item"
	"github.com/ElcanoTek/lens/internal/logx"
	"github.com/ElcanoTek/lens/internal/progress"
	"github.com/ElcanoTek/lens/internal/report"
)

var ctvLog = logx.New("ctv_processor")

// CTVProcessor processes CTV apps with a two-step AI pipeline:
//
//  1. Research: a web-search model (Perplexity Sonar by default) gathers
//     what is publicly known about the app or channel.
//  2. Classification: a fast model structures the research into the
//     advertising categorisation.
//
// The split is cost-effective: Perplexity for search and synthesis, a
// smaller model for structuring.
type CTVProcessor struct {
	Progress   *progress.Tracker
	Client     CTVClassifier
	Reporter   *report.Reporter
	Writer     *ResultsWriter
	Categories CategoryClassifier

	ResearchModel       string
	ResearchTemperature float64
	ResearchMaxTokens   int

	ClassificationModel       string
	ClassificationTemperature float64
	ClassificationMaxTokens   int

	// RequestDelay is the minimum gap between two model requests across all
	// items (both steps count).
	RequestDelay time.Duration

	rateMu      sync.Mutex
	lastRequest time.Time
}

// applyRateLimit spaces requests RequestDelay apart. The lock is held while
// sleeping on purpose: it serialises the waiters, as the Python asyncio.Lock
// did.
func (p *CTVProcessor) applyRateLimit(ctx context.Context) error {
	p.rateMu.Lock()
	defer p.rateMu.Unlock()
	if wait := p.RequestDelay - time.Since(p.lastRequest); wait > 0 {
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
	p.lastRequest = time.Now()
	return nil
}

// ProcessCTVApp runs one CTV app through research and classification. The
// two steps fail separately so the row says which one broke.
func (p *CTVProcessor) ProcessCTVApp(ctx context.Context, it item.CTVWorkItem) {
	ctx = cost.WithItem(ctx)
	start := time.Now()
	ctvLog.Debug("Processing CTV app: " + it.AppName)
	display := "CTV:" + it.AppName

	fail := func(message, reported string, research Research) {
		if ctx.Err() != nil {
			return
		}
		elapsed := time.Since(start)
		if err := p.recordFailure(ctx, &it, message, elapsed, research); err != nil {
			ctvLog.Error(fmt.Sprintf("CTV task failed: %v", err))
			return
		}
		p.Reporter.Log(report.Attempt{Domain: display, Error: reported, Elapsed: elapsed})
	}

	// Step 1: research.
	research, err := func() (Research, error) {
		if err := p.applyRateLimit(ctx); err != nil {
			return Research{}, err
		}
		r, err := p.researchStep(ctx, &it)
		if err != nil {
			return r, err
		}
		if !r.Success {
			return r, errors.New(orDefault(r.Error, "Unknown error"))
		}
		evidence := strings.TrimSpace(r.ResearchContent)
		if evidence == "" || strings.ToUpper(evidence) == "INSUFFICIENT INFORMATION" {
			return r, errors.New("No meaningful research evidence available") //nolint:staticcheck // message kept from the Python release
		}
		return r, nil
	}()
	if err != nil {
		if ctx.Err() == nil {
			ctvLog.Error(fmt.Sprintf("CTV research failed for %s: %s", it.AppName, err))
		}
		fail("Research failed: "+err.Error(), "Research: "+report.Shorten(err.Error()), research)
		return
	}

	// Step 2: classification from the research.
	cls, err := func() (Classification, error) {
		if err := p.applyRateLimit(ctx); err != nil {
			return Classification{}, err
		}
		c, err := p.Client.ClassifyCTVApp(ctx, CTVClassifyInput{
			AppName:         it.AppName,
			ResearchContent: research.ResearchContent,
			BundleID:        it.BundleID,
			Platform:        it.Platform,
			URL:             it.URL,
			Publisher:       it.Publisher,
			Model:           p.ClassificationModel,
			Temperature:     p.ClassificationTemperature,
			MaxTokens:       p.ClassificationMaxTokens,
		})
		if err != nil {
			return c, err
		}
		if !c.Success {
			return c, errors.New(orDefault(c.Error, "Unknown error"))
		}
		return c, nil
	}()
	if err != nil {
		if ctx.Err() == nil {
			ctvLog.Error(fmt.Sprintf("CTV classification failed for %s: %s", it.AppName, err))
		}
		fail("Classification failed: "+err.Error(), "Classification: "+report.Shorten(err.Error()), research)
		return
	}

	elapsed := time.Since(start)
	if err := p.recordSuccess(ctx, &it, research, cls, elapsed); err != nil {
		if ctx.Err() != nil {
			return
		}
		ctvLog.Error(fmt.Sprintf("Unexpected error processing CTV app %s: %v", it.AppName, err))
		fail("Unexpected error: "+err.Error(), report.Shorten(err.Error()), research)
		return
	}
	p.Reporter.Log(report.Attempt{
		Domain:     display,
		OK:         true,
		StatusCode: 200,
		Elapsed:    elapsed,
		SizeKB:     sizeKB(utf8.RuneCountInString(research.ResearchContent)),
		Cache:      "miss",
	})
}

func (p *CTVProcessor) researchStep(ctx context.Context, it *item.CTVWorkItem) (Research, error) {
	ctvLog.Debug("Starting research step for CTV app: " + it.AppName)
	if it.Platform == "" && it.BundleID != "" {
		if detected := DetectCTVPlatform(it.BundleID); detected != "Unknown" {
			it.Platform = detected
		}
	}
	in := CTVResearchInput{
		AppName:   it.AppName,
		BundleID:  it.BundleID,
		Platform:  it.Platform,
		URL:       it.URL,
		Publisher: it.Publisher,
		ResearchOptions: ResearchOptions{
			Model:       p.ResearchModel,
			Temperature: p.ResearchTemperature,
			MaxTokens:   p.ResearchMaxTokens,
		},
	}
	if p.Categories != nil {
		in.CustomQuestions = p.Categories.ResearchQuestions()
	}
	return p.Client.ResearchCTVApp(ctx, in)
}

func (p *CTVProcessor) baseRecord(it *item.CTVWorkItem, platform string) *Record {
	rec := NewRecord()
	rec.Set("App_Name", it.AppName)
	rec.Set("Type", "CTV")
	rec.Set("Bundle_ID", it.BundleID)
	rec.Set("SSP", it.SSP)
	rec.Set("Publisher", it.Publisher)
	rec.Set("Platform", platform)
	rec.Set("URL", it.URL)
	return rec
}

func (p *CTVProcessor) recordSuccess(ctx context.Context, it *item.CTVWorkItem, research Research, cls Classification, elapsed time.Duration) error {
	rec := p.baseRecord(it, orDefault(it.Platform, DetectCTVPlatform(it.BundleID)))
	rec.Set("Quality", cls.Quality)
	rec.Set("Justification", cls.Justification)
	rec.Set("IAB Tier 1", cls.Tier1)
	rec.Set("IAB Tier 2", cls.Tier2)
	rec.Set("IAB Tier 3", cls.Tier3)
	rec.Set("Description", cls.Description)
	rec.Set("Target_Audience", cls.TargetAudience)
	rec.Set("Content_Type", cls.ContentType)
	rec.Set("Language", orDefault(cls.Language, "Unknown"))
	rec.Set("Political_Leaning", orDefault(cls.PoliticalLeaning, "Non-Political"))
	rec.Set("Network_Affiliation", cls.NetworkAffiliation)
	rec.Set("Audience_Size", orDefault(cls.AudienceSize, "Unknown"))
	rec.Set("Research_Summary", TruncateResearch(StripMarkdown(research.ResearchContent), 1000))
	rec.Set("Processing_Time", pyRound2(seconds(elapsed)))
	rec.Set("Research_Model", p.ResearchModel)
	rec.Set("Classification_Model", p.ClassificationModel)
	rec.Set("Processed_At", isoNow())

	if p.Categories != nil {
		result, err := p.Categories.Classify(ctx, it.Identifier(), research.ResearchContent, "ctv_research", it.AppName)
		if err != nil {
			return err
		}
		rec.Update(result)
	}
	if err := writeResult(ctx, p.Writer, rec, p.Categories != nil); err != nil {
		return err
	}
	pt := seconds(elapsed)
	_ = p.Progress.Mark(it.Identifier(), progress.StatusSuccess, rec, "", &pt, "")
	ctvLog.Debug(fmt.Sprintf("Processed CTV app: %s -> %s (%s)", it.AppName, cls.Quality, orDefault(cls.ContentType, "Unknown")))
	return nil
}

func (p *CTVProcessor) recordFailure(ctx context.Context, it *item.CTVWorkItem, message string, elapsed time.Duration, research Research) error {
	rec := p.baseRecord(it, it.Platform)
	rec.Set("Quality", "Failed")
	rec.Set("Justification", message)
	for _, k := range []string{"IAB Tier 1", "IAB Tier 2", "IAB Tier 3", "Description", "Target_Audience", "Content_Type"} {
		rec.Set(k, "N/A")
	}
	rec.Set("Language", "Unknown")
	rec.Set("Political_Leaning", "Non-Political")
	rec.Set("Network_Affiliation", "N/A")
	rec.Set("Audience_Size", "Unknown")
	rec.Set("Research_Summary", TruncateResearch(StripMarkdown(research.ResearchContent), 1000))
	rec.Set("Processing_Time", pyRound2(seconds(elapsed)))
	rec.Set("Research_Model", p.ResearchModel)
	rec.Set("Classification_Model", p.ClassificationModel)
	rec.Set("Processed_At", isoNow())
	if err := writeResult(ctx, p.Writer, rec, p.Categories != nil); err != nil {
		return err
	}
	pt := seconds(elapsed)
	_ = p.Progress.Mark(it.Identifier(), progress.StatusError, nil, message, &pt, "")
	return nil
}

var (
	mdHeaderRe   = regexp.MustCompile(`(?m)^#{1,6}\s+`)
	mdLinkRe     = regexp.MustCompile(`\[([^\]]+)\]\([^)]+\)`)
	mdCitationRe = regexp.MustCompile(`\[\d+\]`)
	mdBoldStarRe = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	mdBoldUndRe  = regexp.MustCompile(`__([^_]+)__`)
	mdCodeRe     = regexp.MustCompile("`([^`]+)`")
	mdNewlinesRe = regexp.MustCompile(`\n{3,}`)
	mdIndentRe   = regexp.MustCompile(`(?m)^[ \t]+`)
)

// StripMarkdown removes common Markdown formatting from research text:
// header markers, [text](url) links (keeping the text), [n] citation
// markers, bold/italic markers, inline-code backticks, runs of 3+ newlines
// (collapsed to a blank line) and leading indentation.
func StripMarkdown(text string) string {
	result := mdHeaderRe.ReplaceAllString(text, "")
	result = mdLinkRe.ReplaceAllString(result, "$1")
	result = mdCitationRe.ReplaceAllString(result, "")
	result = mdBoldStarRe.ReplaceAllString(result, "$1")
	result = mdBoldUndRe.ReplaceAllString(result, "$1")
	// Single-marker italics; a doubled marker is left alone.
	result = stripSingleMarker(result, '*')
	result = stripSingleMarker(result, '_')
	result = mdCodeRe.ReplaceAllString(result, "$1")
	result = mdNewlinesRe.ReplaceAllString(result, "\n\n")
	result = mdIndentRe.ReplaceAllString(result, "")
	return strings.TrimSpace(result)
}

// stripSingleMarker is Python's re.sub(r"(?<!M)M([^M]+)M(?!M)", r"\1", s),
// which RE2 cannot express (no lookaround). Lookbehind reads the original
// string, as re.sub does.
func stripSingleMarker(s string, m byte) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		if s[i] == m && (i == 0 || s[i-1] != m) {
			if j := strings.IndexByte(s[i+1:], m); j > 0 {
				end := i + 1 + j
				if end+1 >= len(s) || s[end+1] != m {
					b.WriteString(s[i+1 : end])
					i = end + 1
					continue
				}
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// TruncateResearch shortens research text for the CSV to at most maxLength
// characters, preferring to end at the last sentence boundary (. ! ?
// followed by whitespace or the end) when that keeps at least half; else it
// cuts hard and appends "...".
func TruncateResearch(content string, maxLength int) string {
	runes := []rune(content)
	if len(runes) <= maxLength {
		return content
	}
	truncated := runes[:maxLength]
	cut := -1
	for k, r := range truncated {
		if (r == '.' || r == '!' || r == '?') && (k+1 == len(truncated) || unicode.IsSpace(truncated[k+1])) {
			cut = k + 1
		}
	}
	if cut != -1 && float64(cut) >= float64(maxLength)*0.5 {
		return strings.TrimSpace(string(runes[:cut]))
	}
	return strings.TrimSpace(string(truncated)) + "..."
}

// DetectCTVPlatform guesses the CTV platform from a bundle ID's shape
// (detect.CTVPlatform), or returns "Unknown".
func DetectCTVPlatform(bundleID string) string { return detect.CTVPlatform(bundleID) }
