// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package stores

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ElcanoTek/lens/internal/config"
	"github.com/ElcanoTek/lens/internal/htmldom"
	"github.com/ElcanoTek/lens/internal/logx"
	"github.com/ElcanoTek/lens/internal/scrape"
)

// AndroidBaseURL is the Play Store app page.
const AndroidBaseURL = "https://play.google.com/store/apps/details"

// DefaultAndroidUserAgent mimics a real browser.
const DefaultAndroidUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

var androidLog = logx.New("android_scraper")

// AndroidOptions configures an AndroidClient.
type AndroidOptions struct {
	RequestTimeout time.Duration
	// RequestDelay is the minimum gap between two requests (rate limit).
	RequestDelay  time.Duration
	MaxConcurrent int
	MaxRetries    int
	// RetryDelay is the base of the exponential backoff between attempts.
	RetryDelay time.Duration
	UserAgent  string
	// BaseURL replaces AndroidBaseURL for requests (tests). Results keep the
	// public Play Store link.
	BaseURL    string
	HTTPClient *http.Client
	Sleep      func(context.Context, time.Duration) error
}

// DefaultAndroidOptions are AndroidScraper's constructor defaults.
func DefaultAndroidOptions() AndroidOptions {
	return AndroidOptions{
		RequestTimeout: 30 * time.Second,
		RequestDelay:   4 * time.Second,
		MaxConcurrent:  3,
		MaxRetries:     3,
		RetryDelay:     2 * time.Second,
		UserAgent:      DefaultAndroidUserAgent,
	}
}

// AndroidOptionsFromConfig reads the android_* settings.
func AndroidOptionsFromConfig(cfg *config.Config) AndroidOptions {
	o := DefaultAndroidOptions()
	o.RequestTimeout = seconds(cfg.AndroidRequestTimeout)
	o.RequestDelay = seconds(cfg.AndroidRequestDelay)
	o.MaxConcurrent = cfg.AndroidMaxConcurrent
	o.MaxRetries = cfg.AndroidMaxRetries
	o.RetryDelay = seconds(cfg.AndroidRetryDelay)
	return o
}

// AndroidApp is one Play Store lookup. Failed lookups carry PackageName,
// Error, FetchedAt and ProcessingTime only.
type AndroidApp struct {
	Success       bool
	PackageName   string
	AppName       string
	Developer     string
	Category      string
	Rating        float64
	RatingCount   int
	Downloads     string
	Description   string
	ContentRating string
	LastUpdated   string
	PlayStoreURL  string
	// ContentForLLM is the text block classification reads.
	ContentForLLM string
	// FetchedAt is the local time in Python's isoformat (scrape.ISOTime).
	FetchedAt      string
	Platform       string // "android"
	Error          string
	ProcessingTime time.Duration
}

// AndroidClient scrapes app metadata from Play Store pages.
type AndroidClient struct {
	opts    AndroidOptions
	http    *http.Client
	sem     chan struct{}
	limiter *limiter
	sleep   func(context.Context, time.Duration) error
	counters
}

// NewAndroidClient returns a client for o.
func NewAndroidClient(o AndroidOptions) *AndroidClient {
	if o.RequestTimeout <= 0 {
		o.RequestTimeout = DefaultAndroidOptions().RequestTimeout
	}
	if o.MaxConcurrent < 1 {
		o.MaxConcurrent = 1
	}
	if o.MaxRetries < 1 {
		o.MaxRetries = 1
	}
	if o.UserAgent == "" {
		o.UserAgent = DefaultAndroidUserAgent
	}
	if o.BaseURL == "" {
		o.BaseURL = AndroidBaseURL
	}
	c := &AndroidClient{opts: o, http: o.HTTPClient, sem: make(chan struct{}, o.MaxConcurrent), sleep: o.Sleep}
	if c.http == nil {
		c.http = newHTTPClient(o.RequestTimeout)
	}
	if c.sleep == nil {
		c.sleep = scrape.Sleep
	}
	c.limiter = newLimiter(o.RequestDelay, c.sleep, androidLog)
	return c
}

// Close releases idle connections.
func (c *AndroidClient) Close() { c.http.CloseIdleConnections() }

// Stats returns the request counters.
func (c *AndroidClient) Stats() Stats { return c.stats() }

// FetchAppMetadata scrapes one app by package name (e.g.
// com.instagram.android). It never returns nil.
func (c *AndroidClient) FetchAppMetadata(ctx context.Context, packageName string) *AndroidApp {
	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		return &AndroidApp{PackageName: packageName, Error: ctx.Err().Error(), FetchedAt: scrape.ISOTime(time.Now())}
	}
	return c.fetchWithRetry(ctx, packageName)
}

// FetchBatch scrapes several apps concurrently (within MaxConcurrent);
// results are in input order.
func (c *AndroidClient) FetchBatch(ctx context.Context, packageNames []string) []*AndroidApp {
	out := make([]*AndroidApp, len(packageNames))
	var wg sync.WaitGroup
	for i, pkg := range packageNames {
		wg.Go(func() { out[i] = c.FetchAppMetadata(ctx, pkg) })
	}
	wg.Wait()
	return out
}

func (c *AndroidClient) fetchWithRetry(ctx context.Context, packageName string) *AndroidApp {
	start := time.Now()
	lastError := ""
	for attempt := 0; attempt < c.opts.MaxRetries; attempt++ {
		if err := c.limiter.wait(ctx); err != nil {
			lastError = err.Error()
			break
		}
		c.request()
		result := c.doFetch(ctx, packageName)
		if result.Success {
			c.success()
			result.ProcessingTime = time.Since(start)
			return result
		}
		lastError = result.Error
		androidLog.Warn(fmt.Sprintf("Attempt %d/%d for package %s failed: %s", attempt+1, c.opts.MaxRetries, packageName, lastError))
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
	return &AndroidApp{
		PackageName:    packageName,
		Error:          lastError,
		ProcessingTime: time.Since(start),
		FetchedAt:      scrape.ISOTime(time.Now()),
	}
}

func (c *AndroidClient) headers() map[string]string {
	// No Accept-Encoding: Go's transport then asks for gzip and decodes it
	// itself (the Python client also advertised br, which needs a decoder
	// outside the standard library).
	return map[string]string{
		"User-Agent":                c.opts.UserAgent,
		"Accept":                    "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8",
		"Accept-Language":           "en-US,en;q=0.5",
		"DNT":                       "1",
		"Upgrade-Insecure-Requests": "1",
	}
}

func (c *AndroidClient) doFetch(ctx context.Context, packageName string) *AndroidApp {
	fetchedAt := scrape.ISOTime(time.Now())
	fail := func(msg string) *AndroidApp {
		return &AndroidApp{PackageName: packageName, Error: msg, FetchedAt: fetchedAt}
	}
	target := c.opts.BaseURL + "?id=" + url.QueryEscape(packageName) + "&hl=en&gl=US"
	status, html, err := get(ctx, c.http, target, c.headers())
	switch {
	case err != nil && status == 0:
		return fail("Network error: " + err.Error())
	case status == http.StatusNotFound:
		return fail("App not found on Play Store")
	case status != http.StatusOK:
		return fail(fmt.Sprintf("HTTP %d", status))
	case err != nil:
		return fail("Network error: " + err.Error())
	}
	if IsBlockedPage(html) {
		return fail("Blocked by Google Play Store")
	}
	return ParsePlayStorePage(packageName, html, fetchedAt)
}

var blockPhrases = []string{
	"captcha",
	"unusual traffic",
	"automated queries",
	"please try again",
	"access denied",
	"we're sorry",
}

// IsBlockedPage reports whether a Play Store response is a block page
// (judged on its first 1000 characters).
func IsBlockedPage(html string) bool {
	head := html
	n := 0
	for i := range html {
		if n == 1000 {
			head = html[:i]
			break
		}
		n++
	}
	lower := strings.ToLower(head)
	for _, phrase := range blockPhrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

// ParsePlayStorePage extracts app metadata from a Play Store page.
func ParsePlayStorePage(packageName, html, fetchedAt string) *AndroidApp {
	doc := htmldom.Parse(html)
	name := ExtractAppName(doc)
	if name == "" {
		return &AndroidApp{
			PackageName: packageName,
			Error:       "Could not extract app name - page might be blocked or app not found",
			FetchedAt:   fetchedAt,
		}
	}
	rating, ratingCount := ExtractRating(doc)
	app := &AndroidApp{
		Success:       true,
		PackageName:   packageName,
		AppName:       name,
		Developer:     ExtractDeveloper(doc),
		Category:      ExtractCategory(doc),
		Rating:        rating,
		RatingCount:   ratingCount,
		Downloads:     ExtractDownloads(doc),
		Description:   ExtractDescription(doc),
		LastUpdated:   ExtractLastUpdated(doc),
		ContentRating: ExtractContentRating(doc),
		PlayStoreURL:  AndroidBaseURL + "?id=" + packageName,
		FetchedAt:     fetchedAt,
		Platform:      "android",
	}
	app.ContentForLLM = androidContentForLLM(app)
	return app
}

func androidContentForLLM(a *AndroidApp) string {
	parts := []string{"App Name: " + a.AppName, "Developer: " + a.Developer}
	if a.Category != "" {
		parts = append(parts, "Category: "+a.Category)
	}
	switch {
	case a.Rating != 0 && a.RatingCount != 0:
		parts = append(parts, fmt.Sprintf("Rating: %s/5.0 (%s reviews)", formatRating(a.Rating), thousands(a.RatingCount)))
	case a.Rating != 0:
		parts = append(parts, fmt.Sprintf("Rating: %s/5.0", formatRating(a.Rating)))
	}
	if a.Downloads != "" {
		parts = append(parts, "Downloads: "+a.Downloads)
	}
	if a.ContentRating != "" {
		parts = append(parts, "Content Rating: "+a.ContentRating)
	}
	if a.Description != "" {
		parts = append(parts, "\nDescription:\n"+truncateDescription(a.Description))
	}
	return strings.Join(parts, "\n")
}

// The extractors' patterns were written for Python's re, whose \s, \d and
// \w are Unicode-aware; Go's are ASCII. These classes restore Python's
// meaning: \s is str.isspace(), \d a decimal digit, \w alphanumeric or "_".
const (
	pyS = `[\t-\r\x1c-\x1f\x85\p{Z}]`
	pyD = `\p{Nd}`
	pyW = `[\p{L}\p{N}_]`
)

func pyRegexp(pattern string) *regexp.Regexp {
	pattern = strings.NewReplacer(`\s`, pyS, `\d`, pyD, `\w`, pyW).Replace(pattern)
	return regexp.MustCompile(pattern)
}

var (
	devLinkRe      = regexp.MustCompile(`/store/apps/dev`)
	categoryLinkRe = regexp.MustCompile(`/store/apps/category`)
	ratedRe        = regexp.MustCompile(`[Rr]ated` + pyS + `+([\p{Nd}.]+)` + pyS + `+stars`)
	reviewsRe      = regexp.MustCompile(`([\p{Nd},]+)` + pyS + `+reviews`)
	bareRatingRe   = regexp.MustCompile(`^([\p{Nd}.]+)$`)
	downloadRes    = []*regexp.Regexp{
		regexp.MustCompile(`(?i)([\p{Nd},]+\+?)` + pyS + `+downloads`),
		regexp.MustCompile(`(?i)([\p{Nd},]+\+?)` + pyS + `+installs`),
		regexp.MustCompile(`(?i)(` + pyD + `+[KMB]?\+?)` + pyS + `+downloads`),
		regexp.MustCompile(`(?i)(` + pyD + `+[KMB]?\+?)` + pyS + `+installs`),
	}
	lastUpdatedRes = []*regexp.Regexp{
		pyRegexp(`(?i)Updated\s+on?\s*(\w+\s+\d+,?\s+\d+)`),
		pyRegexp(`(?i)Last updated:?\s*(\w+\s+\d+,?\s+\d+)`),
	}
	contentRatingRes = []*regexp.Regexp{
		regexp.MustCompile(`(?i)(Everyone|Teen|Mature|Adults Only)`),
		pyRegexp(`(?i)Rated for\s+(\d+\+?)`),
		pyRegexp(`(?i)Content rating:?\s*(\w+)`),
	}
)

// ExtractAppName reads the app name: the h1 (its span when it has one),
// else the <title> without " - Apps on Google Play", else og:title.
func ExtractAppName(doc *htmldom.Node) string {
	if h1 := doc.Find("h1", htmldom.AttrEquals("itemprop", "name")); h1 != nil {
		if span := h1.Find("span"); span != nil {
			return span.GetText("", true)
		}
		return h1.GetText("", true)
	}
	if title := doc.Find("title"); title != nil {
		text := title.GetText("", true)
		if strings.Contains(text, " - Apps on Google Play") {
			return htmldom.TrimSpace(strings.ReplaceAll(text, " - Apps on Google Play", ""))
		}
		return text
	}
	if og := doc.Find("meta", htmldom.AttrEquals("property", "og:title")); og != nil && og.Attr("content") != "" {
		return og.Attr("content")
	}
	return ""
}

// ExtractDeveloper reads the developer link, else a short div whose next
// sibling mentions a category.
func ExtractDeveloper(doc *htmldom.Node) string {
	if link := doc.Find("a", htmldom.AttrMatches("href", devLinkRe)); link != nil {
		if span := link.Find("span"); span != nil {
			return span.GetText("", true)
		}
		return link.GetText("", true)
	}
	for _, div := range doc.FindAll("div") {
		text := div.GetText("", true)
		if text != "" && utf8.RuneCountInString(text) < 100 { // developer names are short
			if next := div.NextElementSibling(); next != nil && strings.Contains(strings.ToLower(next.String()), "category") {
				return text
			}
		}
	}
	return ""
}

// ExtractCategory reads the category link, else itemprop="genre".
func ExtractCategory(doc *htmldom.Node) string {
	if link := doc.Find("a", htmldom.AttrMatches("href", categoryLinkRe)); link != nil {
		if span := link.Find("span"); span != nil {
			return span.GetText("", true)
		}
		return link.GetText("", true)
	}
	if genre := doc.Find("", htmldom.AttrEquals("itemprop", "genre")); genre != nil {
		return genre.GetText("", true)
	}
	return ""
}

// ExtractRating reads the star rating and review count from aria-labels
// ("Rated 4.5 stars out of five stars", "12,487 reviews"; the last match
// wins), falling back to a div holding just a number from 1 to 5.
func ExtractRating(doc *htmldom.Node) (rating float64, count int) {
	for _, el := range doc.FindAll("", htmldom.AttrPresent("aria-label")) {
		label := el.Attr("aria-label")
		if m := ratedRe.FindStringSubmatch(label); m != nil {
			if v, ok := pyFloat(m[1]); ok {
				rating = v
			}
		}
		if m := reviewsRe.FindStringSubmatch(label); m != nil {
			if v, ok := pyInt(strings.ReplaceAll(m[1], ",", "")); ok {
				count = v
			}
		}
	}
	if rating == 0 {
		for _, div := range doc.FindAll("div") {
			m := bareRatingRe.FindStringSubmatch(div.GetText("", true))
			if m == nil {
				continue
			}
			if v, ok := pyFloat(m[1]); ok && v >= 1 && v <= 5 {
				rating = v
				break
			}
		}
	}
	return rating, count
}

// ExtractDownloads reads "10,000+ downloads" style counts from the page
// text, then from aria-labels.
func ExtractDownloads(doc *htmldom.Node) string {
	text := doc.Text()
	for _, re := range downloadRes {
		if m := re.FindStringSubmatch(text); m != nil {
			return m[1]
		}
	}
	for _, el := range doc.FindAll("", htmldom.AttrPresent("aria-label")) {
		label := el.Attr("aria-label")
		for _, re := range downloadRes {
			if m := re.FindStringSubmatch(label); m != nil {
				return m[1]
			}
		}
	}
	return ""
}

// ExtractDescription reads data-g-id="description", then itemprop
// description, og:description and the meta description.
func ExtractDescription(doc *htmldom.Node) string {
	if div := doc.Find("", htmldom.AttrEquals("data-g-id", "description")); div != nil {
		return div.GetText(" ", true)
	}
	if desc := doc.Find("", htmldom.AttrEquals("itemprop", "description")); desc != nil {
		if content := desc.Attr("content"); content != "" {
			return content
		}
		return desc.GetText(" ", true)
	}
	if og := doc.Find("meta", htmldom.AttrEquals("property", "og:description")); og != nil && og.Attr("content") != "" {
		return og.Attr("content")
	}
	if meta := doc.Find("meta", htmldom.AttrEquals("name", "description")); meta != nil && meta.Attr("content") != "" {
		return meta.Attr("content")
	}
	return ""
}

// ExtractLastUpdated reads "Updated on Mar 3, 2026" from the page text.
func ExtractLastUpdated(doc *htmldom.Node) string {
	text := doc.Text()
	for _, re := range lastUpdatedRes {
		if m := re.FindStringSubmatch(text); m != nil {
			return m[1]
		}
	}
	return ""
}

// ExtractContentRating reads the content rating from the page text.
func ExtractContentRating(doc *htmldom.Node) string {
	text := doc.Text()
	for _, re := range contentRatingRes {
		if m := re.FindStringSubmatch(text); m != nil {
			return m[1]
		}
	}
	return ""
}
