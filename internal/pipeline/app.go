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

	"github.com/ElcanoTek/lens/internal/cost"
	"github.com/ElcanoTek/lens/internal/item"
	"github.com/ElcanoTek/lens/internal/logx"
	"github.com/ElcanoTek/lens/internal/progress"
	"github.com/ElcanoTek/lens/internal/report"
)

var appLog = logx.New("app_processor")

// AppProcessor processes iOS and Android apps: fetch the store metadata
// (iTunes Search API / Play Store page), classify it, record the result.
type AppProcessor struct {
	Progress   *progress.Tracker
	IOS        StoreClient // nil when the input has no iOS apps
	Android    StoreClient // nil when the input has no Android apps
	Classifier AppClassifier
	Reporter   *report.Reporter
	Writer     *ResultsWriter
	Categories CategoryClassifier
}

// ProcessApp routes an app item to its platform. Non-app items are a
// programming error.
func (p *AppProcessor) ProcessApp(ctx context.Context, it item.WorkItem) error {
	switch it.Type {
	case item.IOSApp:
		p.ProcessIOSApp(ctx, it.Identifier)
	case item.AndroidApp:
		p.ProcessAndroidApp(ctx, it.Identifier)
	default:
		return fmt.Errorf("Invalid content type for app processing: %s", it.Type) //nolint:staticcheck // message kept from the Python release
	}
	return nil
}

// ProcessIOSApp processes one App Store ID.
func (p *AppProcessor) ProcessIOSApp(ctx context.Context, id string) {
	p.process(cost.WithItem(ctx), id, "ios")
}

// ProcessAndroidApp processes one Play Store package name.
func (p *AppProcessor) ProcessAndroidApp(ctx context.Context, id string) {
	p.process(cost.WithItem(ctx), id, "android")
}

// platformLabels are the display prefix, missing-client message and fetch
// error prefix per platform.
var platformLabels = map[string][3]string{
	"ios":     {"iOS", "iOS client not initialized", "Failed to fetch iOS app data"},
	"android": {"Android", "Android scraper not initialized", "Failed to fetch Android app data"},
}

func (p *AppProcessor) process(ctx context.Context, id, platform string) {
	labels := platformLabels[platform]
	client := p.IOS
	if platform == "android" {
		client = p.Android
	}
	start := time.Now()
	var meta AppMetadata

	err := func() error {
		appLog.Debug(fmt.Sprintf("Processing %s app: %s", labels[0], id))
		if client == nil {
			return errors.New(labels[1])
		}
		var err error
		meta, err = client.FetchAppMetadata(ctx, id)
		if err != nil {
			return err
		}
		if !meta.Success {
			return fmt.Errorf("%s: %s", labels[2], orDefault(meta.Error, "Unknown error"))
		}
		in := AppInput{
			AppID:         id,
			AppName:       meta.AppName,
			Developer:     meta.Developer,
			StoreCategory: meta.PrimaryGenre,
			Description:   meta.Description,
			ContentForLLM: meta.ContentForLLM,
			Platform:      platform,
		}
		if platform == "android" {
			in.StoreCategory = meta.Category
			in.Downloads = meta.Downloads
		}
		if meta.Rating != nil {
			in.Rating = *meta.Rating
		}
		if meta.RatingCount != nil {
			in.RatingCount = *meta.RatingCount
		}
		cls, err := p.Classifier.ClassifyApp(ctx, in)
		if err != nil {
			return err
		}
		if !cls.Success {
			return errors.New("Classification failed: " + orDefault(cls.Error, "Unknown error"))
		}
		elapsed := time.Since(start)
		if err := p.recordSuccess(ctx, id, meta, cls, elapsed, platform); err != nil {
			return err
		}
		p.Reporter.Log(report.Attempt{
			Domain:     labels[0] + ":" + id,
			OK:         true,
			StatusCode: 200,
			Elapsed:    elapsed,
			SizeKB:     sizeKB(utf8.RuneCountInString(meta.ContentForLLM)),
			Cache:      "miss",
		})
		return nil
	}()
	if err == nil || ctx.Err() != nil {
		return
	}

	elapsed := time.Since(start)
	message := err.Error()
	appLog.Error(fmt.Sprintf("Failed to process %s app %s: %s", labels[0], id, message))
	if err := p.recordFailure(ctx, id, message, elapsed, platform, orDefault(meta.FetchedAt, isoNow())); err != nil {
		appLog.Error(fmt.Sprintf("Task failed: %v", err))
		return
	}
	p.Reporter.Log(report.Attempt{
		Domain:  labels[0] + ":" + id,
		Error:   report.Shorten(message),
		Elapsed: elapsed,
	})
}

func appScrapeMode(platform string) string {
	if platform == "ios" {
		return "ios_api"
	}
	return platform + "_scrape"
}

func (p *AppProcessor) recordSuccess(ctx context.Context, id string, meta AppMetadata, cls Classification, elapsed time.Duration, platform string) error {
	appName := orDefault(meta.AppName, id)
	downloads := ""
	if platform == "android" {
		downloads = meta.Downloads
	}
	rec := NewRecord()
	rec.Set("Domain", id) // the Domain column holds every identifier
	rec.Set("Type", strings.ToUpper(platform))
	rec.Set("App_Name", appName)
	rec.Set("Developer", meta.Developer)
	rec.Set("Store_Category", orDefault(meta.PrimaryGenre, meta.Category))
	rec.Set("Rating", optionalCell(meta.Rating))
	rec.Set("Rating_Count", optionalCell(meta.RatingCount))
	rec.Set("Downloads", downloads)
	rec.Set("Quality", cls.Quality)
	rec.Set("Justification", cls.Justification)
	rec.Set("IAB Tier 1", cls.Tier1)
	rec.Set("IAB Tier 2", cls.Tier2)
	rec.Set("IAB Tier 3", cls.Tier3)
	rec.Set("Description", cls.Description)
	rec.Set("Language", orDefault(cls.Language, "Unknown"))
	rec.Set("Political_Leaning", orDefault(cls.PoliticalLeaning, "Non-Political"))
	rec.Set("Audience_Size", orDefault(cls.AudienceSize, "Unknown"))
	rec.Set("Bot_Protection", "") // site-level signal; not applicable to store apps
	rec.Set("Content_Length", utf8.RuneCountInString(meta.ContentForLLM))
	rec.Set("Processing_Time", pyRound2(seconds(elapsed)))
	rec.Set("Scrape_Mode", appScrapeMode(platform))
	rec.Set("Classifier_Mode", "openrouter")
	rec.Set("Scraped_At", orDefault(meta.FetchedAt, isoNow()))

	if p.Categories != nil {
		result, err := p.Categories.Classify(ctx, id, orDefault(meta.ContentForLLM, meta.Description), platform+"_store", appName)
		if err != nil {
			return err
		}
		rec.Update(result)
	}
	if err := writeResult(ctx, p.Writer, rec, p.Categories != nil); err != nil {
		return err
	}
	pt := seconds(elapsed)
	_ = p.Progress.Mark(id, progress.StatusSuccess, rec, "", &pt, "")
	appLog.Debug(fmt.Sprintf("Processed %s app: %s → %s", strings.ToUpper(platform), appName, cls.Quality))
	return nil
}

// optionalCell keeps a store value's Python type in the row (float rating,
// int count) and leaves the cell blank when the store reported none.
func optionalCell[T float64 | int](v *T) any {
	if v == nil {
		return ""
	}
	return *v
}

func (p *AppProcessor) recordFailure(ctx context.Context, id, message string, elapsed time.Duration, platform, fetchedAt string) error {
	rec := NewRecord()
	rec.Set("Domain", id)
	rec.Set("Type", strings.ToUpper(platform))
	for _, k := range []string{"App_Name", "Developer", "Store_Category", "Rating", "Rating_Count", "Downloads"} {
		rec.Set(k, "")
	}
	rec.Set("Quality", "Failed")
	rec.Set("Justification", "Processing failed: "+message)
	failedClassification(rec)
	rec.Set("Language", "Unknown")
	rec.Set("Political_Leaning", "Non-Political")
	rec.Set("Audience_Size", "Unknown")
	rec.Set("Bot_Protection", "")
	rec.Set("Content_Length", 0)
	rec.Set("Processing_Time", pyRound2(seconds(elapsed)))
	rec.Set("Scrape_Mode", appScrapeMode(platform))
	rec.Set("Classifier_Mode", "openrouter")
	rec.Set("Scraped_At", fetchedAt)
	if err := writeResult(ctx, p.Writer, rec, p.Categories != nil); err != nil {
		return err
	}
	pt := seconds(elapsed)
	_ = p.Progress.Mark(id, progress.StatusError, nil, message, &pt, "")
	return nil
}
