// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package stores fetches mobile-app metadata: the iTunes Search API for iOS
// apps (IOSClient) and the Google Play Store web page for Android apps
// (AndroidClient). Both are rate limited (a minimum delay between requests,
// shared by every goroutine), bounded in concurrency, and retry failed
// lookups with exponential backoff. Clients are safe for concurrent use.
//
// Delays are real: the stores ban hosts that hammer them. Tests pass a Sleep
// hook (and zero delays) so they never wait for real.
package stores

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/ElcanoTek/lens/internal/scrape"
)

// maxBodyBytes caps a store response (Play Store pages run to ~1 MB).
const maxBodyBytes = 16 << 20

// maxDescriptionChars is how much of a description goes to the LLM.
const maxDescriptionChars = 2000

// Stats are a client's request counters.
type Stats struct {
	RequestCount, SuccessCount, ErrorCount int
	// SuccessRate is SuccessCount/RequestCount as a percentage.
	SuccessRate float64
}

type counters struct {
	mu                         sync.Mutex
	requests, successes, fails int
}

func (c *counters) request() { c.mu.Lock(); c.requests++; c.mu.Unlock() }
func (c *counters) success() { c.mu.Lock(); c.successes++; c.mu.Unlock() }
func (c *counters) failure() { c.mu.Lock(); c.fails++; c.mu.Unlock() }

func (c *counters) stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := Stats{RequestCount: c.requests, SuccessCount: c.successes, ErrorCount: c.fails}
	if c.requests > 0 {
		s.SuccessRate = float64(c.successes) / float64(c.requests) * 100
	}
	return s
}

// limiter spaces requests at least delay apart across all goroutines. The
// lock is a channel so a waiter can give up when its context ends.
type limiter struct {
	lock  chan struct{}
	last  time.Time
	delay time.Duration
	sleep func(context.Context, time.Duration) error
	log   *slog.Logger
}

func newLimiter(delay time.Duration, sleep func(context.Context, time.Duration) error, log *slog.Logger) *limiter {
	return &limiter{lock: make(chan struct{}, 1), delay: delay, sleep: sleep, log: log}
}

func (l *limiter) wait(ctx context.Context) error {
	select {
	case l.lock <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-l.lock }()
	if elapsed := time.Since(l.last); elapsed < l.delay {
		wait := l.delay - elapsed
		l.log.Debug(fmt.Sprintf("Rate limiting: waiting %.2f seconds", wait.Seconds()))
		if err := l.sleep(ctx, wait); err != nil {
			return err
		}
	}
	l.last = time.Now()
	return nil
}

// backoff is the delay after the given zero-based failed attempt.
func backoff(base time.Duration, attempt int) time.Duration {
	return time.Duration(float64(base) * math.Pow(2, float64(attempt)))
}

func newHTTPClient(timeout time.Duration) *http.Client {
	// Environment proxies are ignored, as the aiohttp sessions ignored them.
	return &http.Client{Timeout: timeout, Transport: &http.Transport{
		Proxy:               nil,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 15 * time.Second,
		ForceAttemptHTTP2:   true,
	}}
}

// get fetches url and returns the status and the decoded body (capped).
func get(ctx context.Context, client *http.Client, url string, headers map[string]string) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, "", err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return resp.StatusCode, "", err
	}
	return resp.StatusCode, scrape.DecodeBody(raw, resp.Header.Get("Content-Type")), nil
}

// truncateDescription keeps maxDescriptionChars characters plus "...".
func truncateDescription(s string) string {
	n := 0
	for i := range s {
		if n == maxDescriptionChars {
			return s[:i] + "..."
		}
		n++
	}
	return s
}

// formatRating is Python's f"{rating:.1f}".
func formatRating(r float64) string { return strconv.FormatFloat(r, 'f', 1, 64) }

// thousands is Python's f"{n:,}".
func thousands(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, ch := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(ch)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// asciiDigits maps every Unicode decimal digit to its ASCII digit, as
// Python's int() and float() accept them.
func asciiDigits(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x80 || !unicode.Is(unicode.Nd, r) {
			return r
		}
		for _, rng := range unicode.Nd.R16 {
			if r >= rune(rng.Lo) && r <= rune(rng.Hi) {
				return '0' + (r-rune(rng.Lo))%10
			}
		}
		for _, rng := range unicode.Nd.R32 {
			lo, hi := rune(rng.Lo), rune(rng.Hi) //nolint:gosec // Unicode code points fit in a rune
			if r >= lo && r <= hi {
				return '0' + (r-lo)%10
			}
		}
		return r
	}, s)
}

// pyFloat is Python's float() on the [\d.]+ strings the extractors match.
func pyFloat(s string) (float64, bool) {
	v, err := strconv.ParseFloat(asciiDigits(s), 64)
	return v, err == nil
}

// pyInt is Python's int() on a run of digits.
func pyInt(s string) (int, bool) {
	v, err := strconv.Atoi(asciiDigits(s))
	return v, err == nil
}
