// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package scrape

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ElcanoTek/lens/internal/config"
	"github.com/ElcanoTek/lens/internal/item"
)

// recordSleep collects requested delays instead of sleeping.
type recordSleep struct {
	mu     sync.Mutex
	delays []time.Duration
}

func (r *recordSleep) sleep(ctx context.Context, d time.Duration) error {
	r.mu.Lock()
	r.delays = append(r.delays, d)
	r.mu.Unlock()
	return ctx.Err()
}

func directClient(t *testing.T, mutate func(*Options)) *Client {
	t.Helper()
	o := DefaultOptions()
	o.RetryDelay = 0
	o.Sleep = (&recordSleep{}).sleep
	if mutate != nil {
		mutate(&o)
	}
	c := New(o)
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

func TestSessionPoolAcquireAndRelease(t *testing.T) {
	ctx := context.Background()
	c := directClient(t, nil)
	if err := c.CreateSessionPool(ctx, 2); err != nil {
		t.Fatal(err)
	}
	first, err := c.AcquireSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.AcquireSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !first.InUse() || !second.InUse() || first == second {
		t.Fatal("sessions not exclusively checked out")
	}
	if first.ID != "local-0" || second.ID != "local-1" {
		t.Errorf("ids %s %s", first.ID, second.ID)
	}
	if st := c.SessionStats(); st.Total != 2 || st.InUse != 2 || st.Available != 0 || st.Utilization != 100 {
		t.Errorf("stats %+v", st)
	}
	c.ReleaseSession(first)
	third, err := c.AcquireSession(ctx)
	if err != nil || third != first {
		t.Fatalf("got %v %v", third, err)
	}
	c.ReleaseSession(second)
	c.ReleaseSession(third)
	if st := c.SessionStats(); st.InUse != 0 || st.Available != 2 {
		t.Errorf("stats %+v", st)
	}
}

func TestAcquireWaitsAndHonoursCancellation(t *testing.T) {
	c := directClient(t, nil)
	if err := c.CreateSessionPool(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	held, _ := c.AcquireSession(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.AcquireSession(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	done := make(chan *Session)
	go func() {
		s, _ := c.AcquireSession(context.Background())
		done <- s
	}()
	time.Sleep(20 * time.Millisecond)
	c.ReleaseSession(held)
	select {
	case s := <-done:
		if s != held {
			t.Error("waiter got another session")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter never woke")
	}
}

func TestAcquireReleasesPermitOnFailure(t *testing.T) {
	c := directClient(t, nil)
	if err := c.CreateSessionPool(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	// Force the "no free session" path while the semaphore still has a
	// permit. The permit must be returned, or the next acquire deadlocks.
	c.mu.Lock()
	c.sessions = nil
	c.mu.Unlock()
	if _, err := c.AcquireSession(context.Background()); err == nil || !strings.Contains(err.Error(), "No available sessions") {
		t.Fatalf("got %v", err)
	}
	c.mu.Lock()
	c.sessions = []*Session{{ID: "local-0"}}
	c.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	s, err := c.AcquireSession(ctx)
	if err != nil || !s.InUse() {
		t.Fatalf("got %v %v", s, err)
	}
}

func TestPoolErrors(t *testing.T) {
	c := directClient(t, nil)
	if _, err := c.AcquireSession(context.Background()); err == nil || !strings.Contains(err.Error(), "not been initialised") {
		t.Errorf("acquire before pool: %v", err)
	}
	if err := c.CreateSessionPool(context.Background(), 0); err == nil {
		t.Error("empty pool accepted")
	}
}

func TestCloseWakesWaiters(t *testing.T) {
	c := New(DefaultOptions())
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = c.CreateSessionPool(context.Background(), 1)
	_, _ = c.AcquireSession(context.Background())
	errs := make(chan error)
	go func() {
		_, err := c.AcquireSession(context.Background())
		errs <- err
	}()
	time.Sleep(20 * time.Millisecond)
	_ = c.Close(context.Background())
	select {
	case err := <-errs:
		if err == nil {
			t.Error("waiter got a session from a closed pool")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter never woke")
	}
}

type chunkReader struct{ chunks [][]byte }

func (r *chunkReader) Read(p []byte) (int, error) {
	if len(r.chunks) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.chunks[0])
	r.chunks[0] = r.chunks[0][n:]
	if len(r.chunks[0]) == 0 {
		r.chunks = r.chunks[1:]
	}
	return n, nil
}

type endless struct{}

func (endless) Read(p []byte) (int, error) {
	n := min(len(p), 1000)
	for i := range n {
		p[i] = 'x'
	}
	return n, nil
}

func TestReadBodyDrainsTheFullStream(t *testing.T) {
	// A reader that hands out one chunk per Read, like aiohttp's
	// StreamReader: the scraper must keep reading to EOF or real pages
	// parse as near-empty.
	r := &chunkReader{chunks: [][]byte{[]byte(strings.Repeat("a", 1000)), []byte(strings.Repeat("b", 1000)), []byte(strings.Repeat("c", 1000))}}
	body, err := readBody(r, MaxResponseBytes)
	if err != nil || string(body) != strings.Repeat("a", 1000)+strings.Repeat("b", 1000)+strings.Repeat("c", 1000) {
		t.Fatalf("got %d bytes, %v", len(body), err)
	}
}

func TestReadBodyRespectsSizeCap(t *testing.T) {
	body, err := readBody(endless{}, 1500)
	if err != nil || len(body) != 1500 {
		t.Fatalf("got %d bytes, %v", len(body), err)
	}
}

func TestDirectScrapeReadsTheWholeLargePage(t *testing.T) {
	// Well past one 128 KiB read: the text sits after a huge <head>.
	head := "<head><title> Big page </title><meta name=description content=' about it '>" +
		"<script>" + strings.Repeat("x", 300*1024) + "</script></head>"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Sec-Fetch-Mode") != "navigate" || r.Header.Get("User-Agent") != "test-agent/1.0" {
			http.Error(w, "headers", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, "<html>"+head+"<body><p>Hello body</p></body></html>")
	}))
	defer srv.Close()
	c := directClient(t, func(o *Options) { o.UserAgent = "test-agent/1.0" })
	_ = c.CreateSessionPool(context.Background(), 1)
	s, _ := c.AcquireSession(context.Background())
	defer c.ReleaseSession(s)
	r := c.Scrape(context.Background(), s, srv.URL)
	if !r.Success || r.Title != "Big page" || r.MetaDescription != "about it" || r.Content != "Big page Hello body" {
		t.Fatalf("result %+v", r)
	}
	if r.StatusCode != 200 || r.Mode != ModeDirect || r.ContentLength != len("Big page Hello body") || r.CacheStatus != CacheMiss {
		t.Errorf("result %+v", r)
	}
	if r.FinalURL != srv.URL {
		t.Errorf("final url %q", r.FinalURL)
	}
}

func TestBlockedStatusIsReturnedAndValidatesAsAntibot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "<html><body>nope</body></html>")
	}))
	defer srv.Close()
	c := directClient(t, nil)
	r := c.Scrape(context.Background(), nil, srv.URL)
	if !r.Success || r.StatusCode != 403 {
		t.Fatalf("result %+v", r)
	}
	msg := ValidateResult(r, 10)
	if msg != "Received HTTP status 403" || !item.IsAntibotError("Scrape validation failed: "+msg) {
		t.Errorf("validation %q", msg)
	}
}

func TestRetriesWithBackoffThenSucceeds(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n < 3 {
			conn, _, _ := w.(http.Hijacker).Hijack()
			_ = conn.Close() // connection dropped mid-request
			return
		}
		_, _ = io.WriteString(w, "<p>ok</p>")
	}))
	defer srv.Close()
	sleeper := &recordSleep{}
	c := directClient(t, func(o *Options) {
		o.RetryDelay = 2 * time.Second
		o.Sleep = sleeper.sleep
	})
	r := c.Scrape(context.Background(), nil, srv.URL)
	if !r.Success || r.Content != "ok" || r.RetriesUsed != 2 {
		t.Fatalf("result %+v", r)
	}
	if want := []time.Duration{2 * time.Second, 4 * time.Second}; fmt.Sprint(sleeper.delays) != fmt.Sprint(want) {
		t.Errorf("delays %v", sleeper.delays)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func okResponse(r *http.Request, body string) *http.Response {
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/html"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    r,
	}
}

func TestFailFastFailureIsRecorded(t *testing.T) {
	c := directClient(t, func(o *Options) {
		o.MaxRetries = 1
		o.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("boom")
		})}
	})
	r := c.Scrape(context.Background(), nil, "example.invalid")
	if r.Success || !strings.Contains(r.Error, "boom") || r.URL != "https://example.invalid" {
		t.Fatalf("result %+v", r)
	}
	if !strings.HasPrefix(r.Error, "Failed to fetch https://example.invalid: ") {
		t.Errorf("error %q", r.Error)
	}
}

func TestDNSFailureIsNotRetriedButFallsBackToWWW(t *testing.T) {
	var hosts []string
	c := directClient(t, func(o *Options) {
		o.HTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			hosts = append(hosts, r.URL.Host)
			if r.URL.Host == "example.com" {
				return nil, &net.DNSError{Err: "no such host", Name: "example.com", IsNotFound: true}
			}
			return okResponse(r, "<title>WWW</title>"), nil
		})}
	})
	r := c.Scrape(context.Background(), nil, "example.com")
	if !r.Success || r.Title != "WWW" || r.FinalURL != "https://www.example.com" {
		t.Fatalf("result %+v", r)
	}
	if fmt.Sprint(hosts) != "[example.com www.example.com]" {
		t.Errorf("hosts %v", hosts)
	}
}

func TestTimeoutsDoNotFallBackToWWW(t *testing.T) {
	var hosts []string
	c := directClient(t, func(o *Options) {
		o.MaxRetries = 2
		o.HTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			hosts = append(hosts, r.URL.Host)
			return nil, context.DeadlineExceeded
		})}
	})
	r := c.Scrape(context.Background(), nil, "example.com")
	if r.Success || fmt.Sprint(hosts) != "[example.com example.com]" || r.RetriesUsed != 1 {
		t.Fatalf("result %+v hosts %v", r, hosts)
	}
}

func TestCrossHostRedirectPolicy(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "<title>Destination</title><p>content</p>")
	}))
	defer target.Close()
	// 127.0.0.1 -> localhost is a move to an unrelated host.
	other := strings.Replace(target.URL, "127.0.0.1", "localhost", 1)
	origin := httptest.NewServer(http.RedirectHandler(other+"/landing", http.StatusFound))
	defer origin.Close()

	rejecting := directClient(t, nil)
	r := rejecting.Scrape(context.Background(), nil, origin.URL)
	if r.Success || r.Error != "URL redirected to different host: localhost" {
		t.Fatalf("result %+v", r)
	}
	following := directClient(t, func(o *Options) { o.AllowCrossHostRedirects = true })
	r = following.Scrape(context.Background(), nil, origin.URL)
	if !r.Success || r.Title != "Destination" || !strings.HasPrefix(r.FinalURL, other) {
		t.Fatalf("result %+v", r)
	}
}

func TestSameSiteRedirectIsAllowed(t *testing.T) {
	c := New(DefaultOptions())
	for _, tc := range [][2]string{
		{"https://example.com", "https://www.example.com/"},
		{"https://myfantasyleague.com", "https://home.myfantasyleague.com/"},
		{"https://m.example.com", "https://example.com/"},
		{"https://bbc.co.uk", "https://www.news.bbc.co.uk/"},
		{"https://Example.com:8443", "https://example.com/"},
	} {
		if err := c.ensureSameHost(tc[0], tc[1]); err != nil {
			t.Errorf("%s -> %s: %v", tc[0], tc[1], err)
		}
	}
}

func TestUnrelatedDomainRedirectIsRejected(t *testing.T) {
	c := New(DefaultOptions())
	for _, tc := range [][2]string{
		{"https://example.com", "https://parked-lander.com/x"},
		{"https://paper.io", "https://voodoo.io/"},
		{"https://foo.co.uk", "https://bar.co.uk/"},
		{"https://example.com", "https://notexample.com/"},
		{"https://shop.example.com", "https://evil.com/"},
	} {
		err := c.ensureSameHost(tc[0], tc[1])
		if err == nil || !strings.Contains(err.Error(), "redirected to different host") {
			t.Errorf("%s -> %s: %v", tc[0], tc[1], err)
		}
	}
	allow := DefaultOptions()
	allow.AllowCrossHostRedirects = true
	if err := New(allow).ensureSameHost("https://example.com", "https://parked-lander.com/x"); err != nil {
		t.Errorf("policy disabled: %v", err)
	}
}

func TestWWWFallbackURL(t *testing.T) {
	cases := map[string]string{
		"https://example.com":          "https://www.example.com",
		"https://example.com/path?q=1": "https://www.example.com/path?q=1",
		"https://www.example.com":      "",
		"https://sub.example.com":      "",
		"https://example.com:8080":     "",
		"https://example.co.uk":        "",
		"https://user@example.com":     "",
	}
	for in, want := range cases {
		if got := wwwFallbackURL(in); got != want {
			t.Errorf("%s: got %q, want %q", in, got, want)
		}
	}
}

func TestBrowserHeadersLookLikeABrowser(t *testing.T) {
	h := browserHeaders("test-agent/1.0")
	if h["User-Agent"] != "test-agent/1.0" || !strings.Contains(h["Accept"], "text/html") ||
		!strings.HasPrefix(h["Accept-Language"], "en-US") || h["Sec-Fetch-Mode"] != "navigate" {
		t.Errorf("headers %v", h)
	}
}

func TestFirecrawlModeIsGone(t *testing.T) {
	o := DefaultOptions()
	o.Mode = "firecrawl"
	if err := New(o).Start(context.Background()); err == nil || !strings.Contains(err.Error(), "Unsupported scrape mode: firecrawl") {
		t.Errorf("got %v", err)
	}
}

func TestOptionsFromConfig(t *testing.T) {
	cfg := config.Default()
	cfg.DeepScrapeVNCPort = 0
	cfg.DeepScrapeExtraArgs = []string{"--shm-size=2g"}
	o := OptionsFromConfig(cfg, ModeDeep, false)
	if o.RequestTimeout != 30*time.Second || o.MaxRetries != 3 || o.RetryDelay != 2*time.Second ||
		o.UserAgent != config.DefaultUserAgent || o.Mode != ModeDeep || !o.AllowCrossHostRedirects ||
		o.ChromeVNCPort != 0 || o.ChromeExtraArgs[0] != "--shm-size=2g" || o.ChromeWaitAfterLoad != time.Second ||
		o.ChromeStartupTimeout != 45*time.Second || o.ChromePullPolicy != "always" {
		t.Errorf("options %+v", o)
	}
}

func TestCancelledScrapeStopsRetrying(t *testing.T) {
	calls := 0
	c := directClient(t, func(o *Options) {
		o.Sleep = Sleep
		o.RetryDelay = time.Hour
		o.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			return nil, errors.New("reset")
		})}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	r := c.Scrape(ctx, nil, "https://sub.example.com")
	if r.Success || time.Since(start) > 5*time.Second || calls != 1 {
		t.Fatalf("result %+v after %v, %d calls", r, time.Since(start), calls)
	}
}

func TestDecodeBody(t *testing.T) {
	cases := []struct {
		raw         []byte
		contentType string
		want        string
	}{
		{[]byte("caf\xc3\xa9"), "text/html; charset=utf-8", "café"},
		{[]byte("caf\xe9"), "text/html; charset=ISO-8859-1", "café"},
		{[]byte("\x93hi\x94\x81"), "text/html; charset=windows-1252", "“hi”"},
		{[]byte("ok\xff"), "text/html", "ok"},
		{[]byte("ok\xff"), "text/html; charset=klingon", "ok"},
		{[]byte("a\xe9b"), "text/plain; charset=us-ascii", "ab"},
	}
	for _, c := range cases {
		if got := DecodeBody(c.raw, c.contentType); got != c.want {
			t.Errorf("%q %s: got %q, want %q", c.raw, c.contentType, got, c.want)
		}
	}
}

func TestParseHTMLTruncatesTheContent(t *testing.T) {
	page := ParseHTML("https://x.test/", "<p>"+strings.Repeat("é", MaxContentChars+10)+"</p>")
	if want := strings.Repeat("é", MaxContentChars) + "..."; page.Content != want {
		t.Errorf("content has %d runes", len([]rune(page.Content)))
	}
}

func TestISOTime(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 6_000, time.Local)
	if got := ISOTime(ts); got != "2026-01-02T03:04:05.000006" {
		t.Errorf("got %s", got)
	}
	if got := ISOTime(ts.Truncate(time.Second)); got != "2026-01-02T03:04:05" {
		t.Errorf("got %s", got)
	}
}
