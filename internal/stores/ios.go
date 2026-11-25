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

// DefaultIOSBatchSize is the number of App Store IDs fetched per iTunes
// /lookup?id=id1,id2,... request. Apple accepts up to 200; 50 keeps URLs
// short and responses bounded while cutting HTTP volume 50x.
const DefaultIOSBatchSize = 50

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
	// BatchSize is the max IDs per iTunes multi-ID lookup request (default 50, max 100).
	BatchSize int
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

	batchMu       sync.Mutex
	prefetchQueue []string
	prefetchSet   map[string]bool
	prefetched    map[string]*IOSApp
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
	if o.BatchSize <= 0 {
		o.BatchSize = DefaultIOSBatchSize
	}
	if o.BatchSize > 100 {
		o.BatchSize = 100
	}
	if o.BaseURL == "" {
		o.BaseURL = IOSBaseURL
	}
	c := &IOSClient{
		opts: o, http: o.HTTPClient, sem: make(chan struct{}, o.MaxConcurrent), sleep: o.Sleep,
		prefetchSet: map[string]bool{}, prefetched: map[string]*IOSApp{},
	}
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

// SetPrefetchQueue registers upcoming App Store IDs so FetchAppMetadata can
// fetch them in multi-ID iTunes /lookup batches (up to BatchSize per HTTP
// request) on demand, keeping memory bounded to one batch at a time.
func (c *IOSClient) SetPrefetchQueue(ids []string) {
	c.batchMu.Lock()
	defer c.batchMu.Unlock()
	c.prefetchQueue = make([]string, 0, len(ids))
	c.prefetchSet = make(map[string]bool, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || c.prefetchSet[id] {
			continue
		}
		c.prefetchSet[id] = true
		c.prefetchQueue = append(c.prefetchQueue, id)
	}
}

// FetchAppMetadata looks up one app by its numeric App Store ID. It never
// returns nil. When a prefetch queue is registered, it fetches up to
// BatchSize queued IDs in one iTunes lookup request and serves subsequent
// calls from memory.
func (c *IOSClient) FetchAppMetadata(ctx context.Context, appID string) *IOSApp {
	if app, ok := c.takePrefetchedOrFetchBatch(ctx, appID); ok {
		return app
	}
	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		return &IOSApp{AppID: appID, Error: ctx.Err().Error(), FetchedAt: scrape.ISOTime(time.Now())}
	}
	return c.fetchWithRetry(ctx, appID)
}

func (c *IOSClient) takePrefetchedOrFetchBatch(ctx context.Context, appID string) (*IOSApp, bool) {
	c.batchMu.Lock()
	defer c.batchMu.Unlock()
	if app, ok := c.prefetched[appID]; ok {
		delete(c.prefetched, appID)
		return app, true
	}
	if !c.prefetchSet[appID] {
		return nil, false
	}
	// Build a chunk starting with appID plus up to BatchSize-1 remaining queued IDs.
	chunk := make([]string, 0, c.opts.BatchSize)
	chunk = append(chunk, appID)
	delete(c.prefetchSet, appID)
	remaining := c.prefetchQueue[:0]
	for _, id := range c.prefetchQueue {
		if !c.prefetchSet[id] {
			continue
		}
		if len(chunk) < c.opts.BatchSize {
			chunk = append(chunk, id)
			delete(c.prefetchSet, id)
		} else {
			remaining = append(remaining, id)
		}
	}
	c.prefetchQueue = remaining
	if len(chunk) == 1 {
		// Only one ID left; fall back to single fetch outside batchMu.
		return nil, false
	}
	batch := c.fetchBatchChunk(ctx, chunk)
	for id, app := range batch {
		if id != appID {
			c.prefetched[id] = app
		}
	}
	if app, ok := batch[appID]; ok {
		return app, true
	}
	return &IOSApp{AppID: appID, Error: "App not found in App Store", FetchedAt: scrape.ISOTime(time.Now())}, true
}

// FetchLookupBatch looks up appIDs in multi-ID iTunes /lookup requests
// (up to BatchSize IDs per HTTP request).
func (c *IOSClient) FetchLookupBatch(ctx context.Context, appIDs []string) map[string]*IOSApp {
	out := make(map[string]*IOSApp, len(appIDs))
	for start := 0; start < len(appIDs); start += c.opts.BatchSize {
		end := min(start+c.opts.BatchSize, len(appIDs))
		for id, app := range c.fetchBatchChunk(ctx, appIDs[start:end]) {
			out[id] = app
		}
	}
	return out
}

func (c *IOSClient) fetchBatchChunk(ctx context.Context, chunk []string) map[string]*IOSApp {
	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		now := scrape.ISOTime(time.Now())
		out := make(map[string]*IOSApp, len(chunk))
		for _, id := range chunk {
			out[id] = &IOSApp{AppID: id, Error: ctx.Err().Error(), FetchedAt: now}
		}
		return out
	}
	start := time.Now()
	lastError := ""
	for attempt := 0; attempt < c.opts.MaxRetries; attempt++ {
		if err := c.limiter.wait(ctx); err != nil {
			lastError = err.Error()
			break
		}
		c.request()
		res, status, errMsg := c.doFetchBatch(ctx, chunk)
		if errMsg == "" {
			c.limiter.recordSuccess()
			c.success()
			elapsed := time.Since(start)
			for _, app := range res {
				app.ProcessingTime = elapsed
			}
			return res
		}
		lastError = errMsg
		if status == http.StatusTooManyRequests || status == http.StatusForbidden {
			c.limiter.penalize()
		}
		iosLog.Warn(fmt.Sprintf("Batch attempt %d/%d (%d apps) failed: %s", attempt+1, c.opts.MaxRetries, len(chunk), lastError))
		if ctx.Err() != nil {
			break
		}
		if attempt < c.opts.MaxRetries-1 {
			if err := c.sleep(ctx, backoff(c.opts.RetryDelay, attempt)); err != nil {
				break
			}
		}
	}
	if lastError == "" {
		lastError = "Max retries exceeded"
	}
	c.failure()
	now := scrape.ISOTime(time.Now())
	elapsed := time.Since(start)
	out := make(map[string]*IOSApp, len(chunk))
	for _, id := range chunk {
		out[id] = &IOSApp{AppID: id, Error: lastError, ProcessingTime: elapsed, FetchedAt: now}
	}
	return out
}

func (c *IOSClient) doFetchBatch(ctx context.Context, chunk []string) (map[string]*IOSApp, int, string) {
	fetchedAt := scrape.ISOTime(time.Now())
	joined := strings.Join(chunk, ",")
	status, body, err := get(ctx, c.http, c.opts.BaseURL+"?id="+url.QueryEscape(joined), nil)
	if err != nil && status == 0 {
		return nil, 0, "Network error: " + err.Error()
	}
	if status != http.StatusOK {
		return nil, status, fmt.Sprintf("HTTP %d", status)
	}
	if err != nil {
		return nil, status, "Network error: " + err.Error()
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(body), &data); err != nil {
		return nil, status, "Unexpected error: " + err.Error()
	}
	byID := make(map[string]*IOSApp, len(chunk))
	results, _ := data["results"].([]any)
	for _, raw := range results {
		app, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		tid := str(app, "trackId")
		if tid == "" {
			continue
		}
		byID[tid] = parseIOSApp(tid, app, fetchedAt)
	}
	out := make(map[string]*IOSApp, len(chunk))
	for _, id := range chunk {
		if app, ok := byID[id]; ok {
			out[id] = app
		} else {
			out[id] = &IOSApp{AppID: id, Error: "App not found in App Store", FetchedAt: fetchedAt}
		}
	}
	return out, status, ""
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
		result, status := c.doFetch(ctx, appID)
		if result.Success {
			c.limiter.recordSuccess()
			c.success()
			result.ProcessingTime = time.Since(start)
			return result
		}
		lastError = result.Error
		if status == http.StatusTooManyRequests || status == http.StatusForbidden {
			c.limiter.penalize()
		}
		// "App not found" is terminal; retrying wastes API quota and time.
		if lastError == "App not found in App Store" {
			c.limiter.recordSuccess()
			break
		}
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

func (c *IOSClient) doFetch(ctx context.Context, appID string) (*IOSApp, int) {
	fetchedAt := scrape.ISOTime(time.Now())
	fail := func(msg string) *IOSApp { return &IOSApp{AppID: appID, Error: msg, FetchedAt: fetchedAt} }
	status, body, err := get(ctx, c.http, c.opts.BaseURL+"?id="+url.QueryEscape(appID), nil)
	if err != nil && status == 0 {
		return fail("Network error: " + err.Error()), 0
	}
	if status != http.StatusOK {
		return fail(fmt.Sprintf("HTTP %d", status)), status
	}
	if err != nil {
		return fail("Network error: " + err.Error()), status
	}
	// The iTunes API answers text/javascript, not application/json: parse
	// the body as JSON whatever the content type says.
	var data map[string]any
	if err := json.Unmarshal([]byte(body), &data); err != nil {
		return fail("Unexpected error: " + err.Error()), status
	}
	if n, _ := data["resultCount"].(float64); n == 0 {
		return fail("App not found in App Store"), status
	}
	results, _ := data["results"].([]any)
	if len(results) == 0 {
		return fail("Unexpected error: list index out of range"), status
	}
	app, _ := results[0].(map[string]any)
	return parseIOSApp(appID, app, fetchedAt), status
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
