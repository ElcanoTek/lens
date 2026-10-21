// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package stores

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ElcanoTek/lens/internal/config"
	"github.com/ElcanoTek/lens/internal/logx"
	"github.com/ElcanoTek/lens/internal/scrape"
)

// IOSBaseURL is the iTunes Search API lookup endpoint
// (https://developer.apple.com/library/archive/documentation/AudioVideo/Conceptual/iTuneSearchAPI/).
const IOSBaseURL = "https://itunes.apple.com/lookup"

var iosLog = logx.New("ios_api_client")

// IOSOptions configures an IOSClient.
type IOSOptions struct {
	RequestTimeout time.Duration
	// RequestDelay is the minimum gap between two requests (rate limit).
	RequestDelay  time.Duration
	MaxConcurrent int
	MaxRetries    int
	// RetryDelay is the base of the exponential backoff between attempts.
	RetryDelay time.Duration
	// BaseURL replaces IOSBaseURL (tests).
	BaseURL string
	// HTTPClient replaces the default client (tests).
	HTTPClient *http.Client
	// Sleep replaces the context-aware wait (tests).
	Sleep func(context.Context, time.Duration) error
}

// DefaultIOSOptions are iOSAPIClient's constructor defaults.
func DefaultIOSOptions() IOSOptions {
	return IOSOptions{
		RequestTimeout: 30 * time.Second,
		RequestDelay:   3 * time.Second,
		MaxConcurrent:  3,
		MaxRetries:     3,
		RetryDelay:     2 * time.Second,
	}
}

func seconds(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// IOSOptionsFromConfig reads the ios_* settings.
func IOSOptionsFromConfig(cfg *config.Config) IOSOptions {
	o := DefaultIOSOptions()
	o.RequestTimeout = seconds(cfg.IOSRequestTimeout)
	o.RequestDelay = seconds(cfg.IOSRequestDelay)
	o.MaxConcurrent = cfg.IOSMaxConcurrent
	o.MaxRetries = cfg.IOSMaxRetries
	o.RetryDelay = seconds(cfg.IOSRetryDelay)
	return o
}

// IOSApp is one App Store lookup. Failed lookups carry AppID, Error,
// FetchedAt and ProcessingTime only.
type IOSApp struct {
	Success          bool
	AppID            string
	BundleID         string
	AppName          string
	Developer        string
	PrimaryGenre     string
	Genres           []string
	Rating           float64 // rounded to two decimals
	RatingCount      int
	Description      string
	ContentRating    string
	Price            float64
	Currency         string
	Version          string
	ReleaseDate      string
	LastUpdated      string
	AppStoreURL      string
	IconURL          string
	FileSizeMB       float64
	SupportedDevices []string
	// ContentForLLM is the text block classification reads.
	ContentForLLM string
	// FetchedAt is the local time in Python's isoformat (scrape.ISOTime).
	FetchedAt      string
	Platform       string // "ios"
	Error          string
	ProcessingTime time.Duration
}

// IOSClient fetches app metadata from the iTunes Search API.
type IOSClient struct {
	opts    IOSOptions
	http    *http.Client
	sem     chan struct{}
	limiter *limiter
	sleep   func(context.Context, time.Duration) error
	counters
}

// NewIOSClient returns a client for o.
func NewIOSClient(o IOSOptions) *IOSClient {
	if o.RequestTimeout <= 0 {
		o.RequestTimeout = DefaultIOSOptions().RequestTimeout
	}
	if o.MaxConcurrent < 1 {
		o.MaxConcurrent = 1
	}
	if o.MaxRetries < 1 {
		o.MaxRetries = 1
	}
	if o.BaseURL == "" {
		o.BaseURL = IOSBaseURL
	}
	c := &IOSClient{opts: o, http: o.HTTPClient, sem: make(chan struct{}, o.MaxConcurrent), sleep: o.Sleep}
	if c.http == nil {
		c.http = newHTTPClient(o.RequestTimeout)
	}
	if c.sleep == nil {
		c.sleep = scrape.Sleep
	}
	c.limiter = newLimiter(o.RequestDelay, c.sleep, iosLog)
	return c
}

// Close releases idle connections.
func (c *IOSClient) Close() { c.http.CloseIdleConnections() }

// Stats returns the request counters.
func (c *IOSClient) Stats() Stats { return c.stats() }

// FetchAppMetadata looks up one app by its numeric App Store ID. It never
// returns nil.
func (c *IOSClient) FetchAppMetadata(ctx context.Context, appID string) *IOSApp {
	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		return &IOSApp{AppID: appID, Error: ctx.Err().Error(), FetchedAt: scrape.ISOTime(time.Now())}
	}
	return c.fetchWithRetry(ctx, appID)
}

// FetchBatch looks up several apps concurrently (within MaxConcurrent);
// results are in input order.
func (c *IOSClient) FetchBatch(ctx context.Context, appIDs []string) []*IOSApp {
	out := make([]*IOSApp, len(appIDs))
	var wg sync.WaitGroup
	for i, id := range appIDs {
		wg.Go(func() { out[i] = c.FetchAppMetadata(ctx, id) })
	}
	wg.Wait()
	return out
}

func (c *IOSClient) fetchWithRetry(ctx context.Context, appID string) *IOSApp {
	start := time.Now()
	lastError := ""
	for attempt := 0; attempt < c.opts.MaxRetries; attempt++ {
		if err := c.limiter.wait(ctx); err != nil {
			lastError = err.Error()
			break
		}
		c.request()
		result := c.doFetch(ctx, appID)
		if result.Success {
			c.success()
			result.ProcessingTime = time.Since(start)
			return result
		}
		lastError = result.Error
		iosLog.Warn(fmt.Sprintf("Attempt %d/%d for app %s failed: %s", attempt+1, c.opts.MaxRetries, appID, lastError))
		if ctx.Err() != nil {
			break
		}
		if attempt < c.opts.MaxRetries-1 {
			if err := c.sleep(ctx, backoff(c.opts.RetryDelay, attempt)); err != nil {
				break
			}
		}
	}
	c.failure()
	if lastError == "" {
		lastError = "Max retries exceeded"
	}
	return &IOSApp{
		AppID:          appID,
		Error:          lastError,
		ProcessingTime: time.Since(start),
		FetchedAt:      scrape.ISOTime(time.Now()),
	}
}

func (c *IOSClient) doFetch(ctx context.Context, appID string) *IOSApp {
	fetchedAt := scrape.ISOTime(time.Now())
	fail := func(msg string) *IOSApp { return &IOSApp{AppID: appID, Error: msg, FetchedAt: fetchedAt} }
	status, body, err := get(ctx, c.http, c.opts.BaseURL+"?id="+url.QueryEscape(appID), nil)
	if err != nil && status == 0 {
		return fail("Network error: " + err.Error())
	}
	if status != http.StatusOK {
		return fail(fmt.Sprintf("HTTP %d", status))
	}
	if err != nil {
		return fail("Network error: " + err.Error())
	}
	// The iTunes API answers text/javascript, not application/json: parse
	// the body as JSON whatever the content type says.
	var data map[string]any
	if err := json.Unmarshal([]byte(body), &data); err != nil {
		return fail("Unexpected error: " + err.Error())
	}
	if n, _ := data["resultCount"].(float64); n == 0 {
		return fail("App not found in App Store")
	}
	results, _ := data["results"].([]any)
	if len(results) == 0 {
		return fail("Unexpected error: list index out of range")
	}
	app, _ := results[0].(map[string]any)
	return parseIOSApp(appID, app, fetchedAt)
}

func str(m map[string]any, key string) string {
	switch v := m[key].(type) {
	case nil:
		return ""
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return fmt.Sprint(v)
	}
}

func num(m map[string]any, key string) float64 {
	switch v := m[key].(type) {
	case float64:
		return v
	case string:
		f, _ := strconv.ParseFloat(v, 64)
		return f
	}
	return 0
}

func strList(m map[string]any, key string) []string {
	items, _ := m[key].([]any)
	out := make([]string, 0, len(items))
	for _, it := range items {
		if s, ok := it.(string); ok {
			out = append(out, s)
		} else {
			out = append(out, fmt.Sprint(it))
		}
	}
	return out
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }

func parseIOSApp(appID string, app map[string]any, fetchedAt string) *IOSApp {
	if app == nil {
		app = map[string]any{}
	}
	rating := num(app, "averageUserRating")
	ratingCount := int(num(app, "userRatingCount"))
	currency := "USD"
	if _, ok := app["currency"]; ok {
		currency = str(app, "currency")
	}
	icon := str(app, "artworkUrl512")
	if _, ok := app["artworkUrl512"]; !ok {
		icon = str(app, "artworkUrl100")
	}
	sizeMB := 0.0
	if raw, ok := app["fileSizeBytes"]; ok {
		var s string
		switch v := raw.(type) {
		case string:
			s = v
		case float64:
			s = strconv.FormatFloat(v, 'f', -1, 64)
		}
		if n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
			sizeMB = float64(n) / (1024 * 1024)
		}
	}
	result := &IOSApp{
		Success:          true,
		AppID:            appID,
		BundleID:         str(app, "bundleId"),
		AppName:          str(app, "trackName"),
		Developer:        str(app, "artistName"),
		PrimaryGenre:     str(app, "primaryGenreName"),
		Genres:           strList(app, "genres"),
		Rating:           round2(rating),
		RatingCount:      ratingCount,
		Description:      str(app, "description"),
		ContentRating:    str(app, "contentAdvisoryRating"),
		Price:            num(app, "price"),
		Currency:         currency,
		Version:          str(app, "version"),
		ReleaseDate:      str(app, "releaseDate"),
		LastUpdated:      str(app, "currentVersionReleaseDate"),
		AppStoreURL:      str(app, "trackViewUrl"),
		IconURL:          icon,
		FileSizeMB:       round2(sizeMB),
		SupportedDevices: strList(app, "supportedDevices"),
		FetchedAt:        fetchedAt,
		Platform:         "ios",
	}
	result.ContentForLLM = iosContentForLLM(result, rating)
	return result
}

// iosContentForLLM builds the text block classification reads. rating is
// the unrounded API value, as the Python release formatted it.
func iosContentForLLM(a *IOSApp, rating float64) string {
	parts := []string{
		"App Name: " + a.AppName,
		"Developer: " + a.Developer,
		"Primary Category: " + a.PrimaryGenre,
	}
	if len(a.Genres) > 0 {
		parts = append(parts, "All Categories: "+strings.Join(a.Genres, ", "))
	}
	if rating != 0 && a.RatingCount != 0 {
		parts = append(parts, fmt.Sprintf("Rating: %s/5.0 (%s reviews)", formatRating(rating), thousands(a.RatingCount)))
	}
	if a.ContentRating != "" {
		parts = append(parts, "Content Rating: "+a.ContentRating)
	}
	if a.Description != "" {
		parts = append(parts, "\nDescription:\n"+truncateDescription(a.Description))
	}
	return strings.Join(parts, "\n")
}
