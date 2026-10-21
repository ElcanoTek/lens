// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package scrape fetches websites for classification. It has two backends:
//
//   - direct: a plain HTTP GET (no JavaScript) with browser-like headers,
//     retries with exponential backoff, a capped full-body read, and the
//     cross-host redirect policy;
//   - deep: headless Chrome in a rootless Podman container running
//     selenium/standalone-chrome, driven over W3C WebDriver (package
//     webdriver). Only one browser session is practical, so the session pool
//     is forced to one.
//
// A Client is safe for concurrent use by many goroutines: callers take a
// Session from the pool (AcquireSession), Scrape with it, and give it back
// (ReleaseSession). Every Result is plain data; ValidateResult turns one into
// the failure message the pipeline records, including the anti-bot / block
// page signatures item.IsAntibotError recognises.
package scrape

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/net/publicsuffix"

	"github.com/ElcanoTek/lens/internal/config"
	"github.com/ElcanoTek/lens/internal/logx"
	"github.com/ElcanoTek/lens/internal/scrape/webdriver"
)

// MaxContentChars is how much extracted text every backend keeps: the LLM
// prompt only consumes the first MaxContentChars characters.
const MaxContentChars = 4000

// MaxResponseBytes caps how much of a response body is buffered. Anything
// beyond it is waste (only MaxContentChars of text are kept), and without a
// cap a single huge response could exhaust memory.
const MaxResponseBytes = 5 * 1024 * 1024

// Scrape modes.
const (
	ModeDirect = "direct"
	ModeDeep   = "deep"
)

// CacheMiss is the only cache status a scrape reports (there is no cache);
// verbose reporting prints it.
const CacheMiss = "miss"

var log = logx.New("scraper_client")

// Options configures a Client. DefaultOptions has the Python release's
// defaults; OptionsFromConfig builds them from config.json.
type Options struct {
	RequestTimeout time.Duration
	MaxRetries     int
	// RetryDelay is the base of the exponential backoff between attempts.
	RetryDelay time.Duration
	UserAgent  string
	// Mode is ModeDirect or ModeDeep.
	Mode string
	// AllowCrossHostRedirects turns off the redirect policy (by default a
	// redirect to an unrelated host fails the scrape).
	AllowCrossHostRedirects bool

	PodmanBinary         string
	ChromeImage          string
	ChromeContainerName  string
	ChromePort           int
	ChromeVNCPort        int // 0 disables the VNC port
	ChromeExtraArgs      []string
	ChromeStartupTimeout time.Duration
	ChromeWaitAfterLoad  time.Duration
	// KeepContainer keeps one persistent container under the configured
	// name and port instead of a private one per run.
	KeepContainer    bool
	ChromePullPolicy string

	// Test hooks; nil means the real thing.

	// HTTPClient performs direct-mode requests. Its CheckRedirect is kept.
	HTTPClient *http.Client
	// Runner runs podman.
	Runner Runner
	// Sleep waits between retries and polls; it must return early with the
	// context's error when ctx is done.
	Sleep func(ctx context.Context, d time.Duration) error
	// ProcessAlive reports whether a pid still runs (orphan detection).
	ProcessAlive func(pid int) bool
	// FreePort returns an unused loopback TCP port.
	FreePort func() (int, error)
	// ChromeEndpoint replaces http://127.0.0.1:<port> as the WebDriver base.
	ChromeEndpoint string
}

// DefaultOptions are ScraperClient's constructor defaults.
func DefaultOptions() Options {
	return Options{
		RequestTimeout:       30 * time.Second,
		MaxRetries:           3,
		RetryDelay:           2 * time.Second,
		UserAgent:            "Mozilla/5.0",
		Mode:                 ModeDirect,
		PodmanBinary:         "podman",
		ChromeImage:          "docker.io/selenium/standalone-chrome:latest",
		ChromeContainerName:  "elcano-lens-chromedriver",
		ChromePort:           4444,
		ChromeVNCPort:        7900,
		ChromeStartupTimeout: 45 * time.Second,
		ChromeWaitAfterLoad:  time.Second,
		ChromePullPolicy:     "always",
	}
}

func seconds(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// OptionsFromConfig builds the options orchestration used for a pass in the
// given mode. rejectRedirects is the pass's redirect policy (the deep retry
// pass follows cross-host redirects when retry_follow_redirects is on).
func OptionsFromConfig(cfg *config.Config, mode string, rejectRedirects bool) Options {
	o := DefaultOptions()
	o.RequestTimeout = seconds(cfg.RequestTimeout)
	o.MaxRetries = cfg.MaxRetries
	o.RetryDelay = seconds(cfg.RetryDelay)
	o.UserAgent = cfg.UserAgent
	o.Mode = mode
	o.AllowCrossHostRedirects = !rejectRedirects
	o.PodmanBinary = cfg.DeepScrapePodmanBinary
	o.ChromeImage = cfg.DeepScrapeImage
	o.ChromeContainerName = cfg.DeepScrapeContainerName
	o.ChromePort = cfg.DeepScrapePort
	o.ChromeVNCPort = cfg.DeepScrapeVNCPort
	o.ChromeExtraArgs = append([]string(nil), cfg.DeepScrapeExtraArgs...)
	o.ChromeStartupTimeout = seconds(cfg.DeepScrapeStartup)
	o.ChromeWaitAfterLoad = seconds(cfg.DeepScrapeWaitAfterLoad)
	o.KeepContainer = cfg.DeepScrapeKeepContainer
	if cfg.DeepScrapePullPolicy != "" {
		o.ChromePullPolicy = cfg.DeepScrapePullPolicy
	}
	return o
}

// Result is one scrape. Failed scrapes carry Error and the timing fields.
type Result struct {
	Success bool
	// URL is the requested URL, with https:// added when it had no scheme.
	URL string
	// FinalURL is where the content came from: after redirects, and the
	// www. host when the bare domain refused connections.
	FinalURL        string
	Title           string
	MetaDescription string
	Content         string
	// ContentLength is len(Content) in characters (code points).
	ContentLength  int
	StatusCode     int
	ProcessingTime time.Duration
	// Mode is the backend that ran ("direct" or "deep").
	Mode string
	// ScrapedAt is the local time in Python's isoformat (see ISOTime), as the
	// output CSV's Scraped_At column has always carried it.
	ScrapedAt string
	Error     string
	// RetriesUsed counts direct-mode attempts after the first (and a
	// deep-mode browser recovery).
	RetriesUsed int
	CacheStatus string
}

// Session is one slot of the pool. Deep sessions own a browser session.
type Session struct {
	ID string

	mu       sync.Mutex
	inUse    bool
	lastUsed time.Time
	driver   *webdriver.Session
}

// InUse reports whether the session is checked out.
func (s *Session) InUse() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inUse
}

func (s *Session) getDriver() *webdriver.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.driver
}

func (s *Session) setDriver(d *webdriver.Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.driver = d
}

// SessionStats is the pool's occupancy.
type SessionStats struct {
	Total, InUse, Available int
	// Utilization is InUse/Total as a percentage.
	Utilization float64
}

// Client is the scraper. Create it with New, Start it, build the pool with
// CreateSessionPool, and Close it when done.
type Client struct {
	opts       Options
	mode       string
	httpClient *http.Client
	runner     Runner
	sleep      func(context.Context, time.Duration) error

	mu       sync.Mutex
	sessions []*Session
	sem      chan struct{}
	closed   chan struct{}

	// Deep-mode container state, guarded by cmu. Container operations are
	// rare and slow; they hold cmu for their whole run so a recovery and a
	// Close cannot interleave.
	cmu              sync.Mutex
	containerName    string
	chromePort       int
	vncPort          int
	endpoint         string
	containerStarted bool
}

// New returns a client for o. It does no I/O; Start does.
func New(o Options) *Client {
	d := DefaultOptions()
	if o.RequestTimeout <= 0 {
		o.RequestTimeout = d.RequestTimeout
	}
	if o.MaxRetries < 1 {
		o.MaxRetries = 1
	}
	if o.UserAgent == "" {
		o.UserAgent = d.UserAgent
	}
	if o.PodmanBinary == "" {
		o.PodmanBinary = d.PodmanBinary
	}
	if o.ChromeImage == "" {
		o.ChromeImage = d.ChromeImage
	}
	if o.ChromeContainerName == "" {
		o.ChromeContainerName = d.ChromeContainerName
	}
	if o.ChromePort == 0 {
		o.ChromePort = d.ChromePort
	}
	if o.ChromeStartupTimeout <= 0 {
		o.ChromeStartupTimeout = d.ChromeStartupTimeout
	}
	if o.ChromePullPolicy == "" {
		o.ChromePullPolicy = d.ChromePullPolicy
	}
	c := &Client{
		opts:          o,
		mode:          strings.ToLower(o.Mode),
		runner:        o.Runner,
		sleep:         o.Sleep,
		containerName: o.ChromeContainerName,
		chromePort:    o.ChromePort,
		vncPort:       o.ChromeVNCPort,
		closed:        make(chan struct{}),
	}
	if c.mode == "" {
		c.mode = ModeDirect
	}
	if c.runner == nil {
		c.runner = ExecRunner{}
	}
	if c.sleep == nil {
		c.sleep = Sleep
	}
	c.endpoint = c.defaultEndpoint()
	return c
}

func (c *Client) defaultEndpoint() string {
	if c.opts.ChromeEndpoint != "" {
		return strings.TrimRight(c.opts.ChromeEndpoint, "/")
	}
	return fmt.Sprintf("http://127.0.0.1:%d", c.chromePort)
}

// Sleep waits for d or until ctx is done.
func Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// errTooManyRedirects matches aiohttp's limit of ten redirects.
var errTooManyRedirects = errors.New("too many redirects")

// newHTTPClient is the direct mode's shared client: browser headers are set
// per request, cookies persist for the run (one aiohttp session did the
// same, and cookie-check redirects need it), and environment proxies are
// ignored, as aiohttp ignored them.
func newHTTPClient() *http.Client {
	jar, _ := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{
		Transport: transport,
		Jar:       jar,
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errTooManyRedirects
			}
			return nil
		},
	}
}

// Mode returns the backend name results carry.
func (c *Client) Mode() string {
	if c.mode == ModeDeep {
		return ModeDeep
	}
	return ModeDirect
}

// Start prepares the backend: the HTTP client for direct mode, the Chrome
// container for deep mode. It fails for an unknown mode.
func (c *Client) Start(ctx context.Context) error {
	switch c.mode {
	case ModeDirect:
		c.httpClient = c.opts.HTTPClient
		if c.httpClient == nil {
			c.httpClient = newHTTPClient()
		}
		return nil
	case ModeDeep:
		if !c.opts.KeepContainer {
			if err := c.claimPrivateContainer(ctx); err != nil {
				return err
			}
		}
		return c.ensureChromeContainer(ctx)
	default:
		return fmt.Errorf("Unsupported scrape mode: %s", c.mode) //nolint:staticcheck // message kept from the Python release
	}
}

// CreateSessionPool initialises a pool of n reusable sessions; deep mode
// supports only one browser session and reduces n to 1.
func (c *Client) CreateSessionPool(ctx context.Context, n int) error {
	requested := n
	if n <= 0 {
		return errors.New("Number of sessions must be at least 1") //nolint:staticcheck // message kept from the Python release
	}
	if c.mode == ModeDeep && n > 1 {
		log.Warn(fmt.Sprintf("Deep scrape mode supports only a single concurrent browser session. "+
			"Reducing requested sessions from %d to 1.", n))
		n = 1
	}
	log.Info(fmt.Sprintf("Creating %d local scraping sessions (requested %d)...", n, requested))

	sessions := make([]*Session, 0, n)
	for i := 0; i < n; i++ {
		if c.mode == ModeDeep {
			driver, err := c.createWebdriver(ctx)
			if err != nil {
				for _, s := range sessions {
					c.safeQuit(ctx, s.getDriver())
				}
				return err
			}
			sessions = append(sessions, &Session{ID: fmt.Sprintf("chrome-%d", i), driver: driver})
		} else {
			sessions = append(sessions, &Session{ID: fmt.Sprintf("local-%d", i)})
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sessions = sessions
	c.sem = make(chan struct{}, n)
	return nil
}

// AcquireSession takes a free session, waiting while all are in use.
func (c *Client) AcquireSession(ctx context.Context) (*Session, error) {
	c.mu.Lock()
	sem, closed := c.sem, c.closed
	c.mu.Unlock()
	if sem == nil {
		return nil, errors.New("Session pool has not been initialised")
	}
	select {
	case sem <- struct{}{}:
	case <-closed:
		return nil, errors.New("session pool was closed")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.sessions {
		s.mu.Lock()
		if !s.inUse {
			s.inUse = true
			s.lastUsed = time.Now()
			s.mu.Unlock()
			return s, nil
		}
		s.mu.Unlock()
	}
	// Never hold the permit on failure: a leaked permit would shrink the
	// pool permanently and eventually deadlock every worker.
	<-sem
	return nil, errors.New("No available sessions in pool") //nolint:staticcheck // message kept from the Python release
}

// ReleaseSession returns a session to the pool.
func (c *Client) ReleaseSession(s *Session) {
	if s == nil {
		return
	}
	s.mu.Lock()
	wasInUse := s.inUse
	s.inUse = false
	s.mu.Unlock()
	c.mu.Lock()
	sem := c.sem
	c.mu.Unlock()
	if sem != nil && wasInUse {
		select {
		case <-sem:
		default:
		}
	}
}

// SessionStats reports the pool's occupancy.
func (c *Client) SessionStats() SessionStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := SessionStats{Total: len(c.sessions)}
	for _, s := range c.sessions {
		if s.InUse() {
			st.InUse++
		}
	}
	st.Available = st.Total - st.InUse
	if st.Total > 0 {
		st.Utilization = float64(st.InUse) / float64(st.Total) * 100
	}
	return st
}

// Close quits every browser session, empties the pool (waiters get an
// error) and stops a private Chrome container. It runs to completion even
// when ctx is already cancelled — shutdown is exactly when it must run.
func (c *Client) Close(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	log.Info("Cleaning up scraper sessions...")
	c.mu.Lock()
	sessions := c.sessions
	c.sessions = nil
	c.sem = nil
	select {
	case <-c.closed:
	default:
		close(c.closed)
	}
	c.mu.Unlock()
	if c.mode == ModeDeep {
		for _, s := range sessions {
			c.safeQuit(ctx, s.getDriver())
			s.setDriver(nil)
		}
	}
	if c.httpClient != nil {
		c.httpClient.CloseIdleConnections()
	}
	if c.mode == ModeDeep && !c.opts.KeepContainer {
		c.cmu.Lock()
		started := c.containerStarted
		c.cmu.Unlock()
		if started {
			return c.stopChromeContainer(ctx)
		}
	}
	return nil
}

// Scrape fetches target (https:// is assumed without a scheme) with the
// client's backend. It never returns nil; a failure is a Result with
// Success false and Error set.
func (c *Client) Scrape(ctx context.Context, s *Session, target string) *Result {
	start := time.Now()
	if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
		target = "https://" + target
	}
	scrapedAt := ISOTime(time.Now())
	var page *fetched
	var err error
	if c.mode == ModeDeep {
		page, err = c.scrapeWithChrome(ctx, s, target, c.opts.RequestTimeout, true)
	} else {
		page, err = c.scrapeDirect(ctx, target, c.opts.RequestTimeout)
	}
	elapsed := time.Since(start)
	if err != nil {
		log.Error(fmt.Sprintf("Failed to scrape %s: %s", target, err))
		retries := 0
		var fe *FetchError
		if errors.As(err, &fe) {
			retries = fe.Attempts - 1
		}
		return &Result{
			URL:            target,
			Error:          err.Error(),
			ProcessingTime: elapsed,
			Mode:           c.Mode(),
			ScrapedAt:      ISOTime(time.Now()),
			RetriesUsed:    retries,
			CacheStatus:    CacheMiss,
		}
	}
	return &Result{
		Success:         true,
		URL:             target,
		FinalURL:        page.finalURL,
		Title:           page.Title,
		MetaDescription: page.MetaDescription,
		Content:         page.Content,
		ContentLength:   utf8.RuneCountInString(page.Content),
		StatusCode:      page.status,
		ProcessingTime:  elapsed,
		Mode:            c.Mode(),
		ScrapedAt:       scrapedAt,
		RetriesUsed:     page.retries,
		CacheStatus:     CacheMiss,
	}
}

// fetched is a parsed page plus how it was fetched.
type fetched struct {
	Page
	status   int
	finalURL string
	retries  int
}

// RedirectError is a redirect to an unrelated host while the policy rejects
// them.
type RedirectError struct{ Host string }

func (e *RedirectError) Error() string { return "URL redirected to different host: " + e.Host }

// FetchError is a direct-mode fetch that failed every attempt.
type FetchError struct {
	URL string
	// Attempts is how many requests were made.
	Attempts int
	Err      error
}

func (e *FetchError) Error() string { return fmt.Sprintf("Failed to fetch %s: %v", e.URL, e.Err) }

func (e *FetchError) Unwrap() error { return e.Err }

// bodyError marks a failure while reading a response body (as opposed to
// connecting): it is retried, but never sends the scrape to the www. host.
type bodyError struct{ err error }

func (e *bodyError) Error() string { return e.err.Error() }

func (e *bodyError) Unwrap() error { return e.err }

func (c *Client) scrapeDirect(ctx context.Context, target string, timeout time.Duration) (*fetched, error) {
	if c.httpClient == nil {
		return nil, errors.New("Web session is not initialised") //nolint:staticcheck // message kept from the Python release
	}
	page, err := c.fetchWithRetries(ctx, target, timeout)
	if err != nil {
		// Bare domains sometimes have no A record (or no listener) while the
		// www. host works. Only worth trying when the connection itself
		// failed — an HTTP-level error would just repeat.
		fallback := wwwFallbackURL(target)
		if fallback == "" || !isConnectionFailure(err) || ctx.Err() != nil {
			return nil, err
		}
		log.Info(fmt.Sprintf("Connection to %s failed; retrying via %s", target, fallback))
		page, err = c.fetchWithRetries(ctx, fallback, timeout)
		if err != nil {
			return nil, err
		}
		target = fallback
	}
	parsed := ParseHTML(target, page.body)
	return &fetched{Page: parsed, status: page.status, finalURL: page.finalURL, retries: page.retries}, nil
}

type rawPage struct {
	body     string
	status   int
	finalURL string
	retries  int
}

func shorten(err error) string {
	s := err.Error()
	if r := []rune(s); len(r) > 100 {
		return string(r[:100])
	}
	return s
}

func (c *Client) fetchWithRetries(ctx context.Context, target string, timeout time.Duration) (*rawPage, error) {
	var last error
	attempts := 0
	for attempt := 1; attempt <= c.opts.MaxRetries; attempt++ {
		attempts = attempt
		page, err := c.fetchOnce(ctx, target, timeout)
		if err == nil {
			page.retries = attempt - 1
			return page, nil
		}
		var redirect *RedirectError
		if errors.As(err, &redirect) {
			return nil, err // a policy decision: retrying cannot change it
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		last = err
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && !dnsErr.IsTimeout {
			// DNS resolution won't heal within a retry cycle; fail fast so
			// dead domains don't hold a session slot through backoff sleeps.
			log.Warn(fmt.Sprintf("DNS resolution failed for %s: %s", target, shorten(err)))
			break
		}
		if attempt < c.opts.MaxRetries {
			delay := time.Duration(float64(c.opts.RetryDelay) * math.Pow(2, float64(attempt-1)))
			log.Warn(fmt.Sprintf("Attempt %d/%d to fetch %s failed: %s. Retrying in %.1fs...",
				attempt, c.opts.MaxRetries, target, shorten(err), delay.Seconds()))
			if err := c.sleep(ctx, delay); err != nil {
				return nil, err
			}
		} else {
			log.Error(fmt.Sprintf("All %d attempts to fetch %s failed: %s", c.opts.MaxRetries, target, shorten(err)))
		}
	}
	return nil, &FetchError{URL: target, Attempts: attempts, Err: last}
}

// browserHeaders match what a real browser sends on a top-level navigation.
// Sites with basic bot filtering reject requests that carry a User-Agent
// with none of the companion headers a browser always includes.
func browserHeaders(userAgent string) map[string]string {
	return map[string]string{
		"User-Agent":                userAgent,
		"Accept":                    "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8",
		"Accept-Language":           "en-US,en;q=0.9",
		"Upgrade-Insecure-Requests": "1",
		"Sec-Fetch-Dest":            "document",
		"Sec-Fetch-Mode":            "navigate",
		"Sec-Fetch-Site":            "none",
		"Sec-Fetch-User":            "?1",
	}
}

func (c *Client) fetchOnce(ctx context.Context, target string, timeout time.Duration) (*rawPage, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range browserHeaders(c.opts.UserAgent) {
		req.Header.Set(k, v)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	finalURL := resp.Request.URL.String()
	if err := c.ensureSameHost(target, finalURL); err != nil {
		return nil, err
	}
	raw, err := readBody(resp.Body, MaxResponseBytes)
	if err != nil {
		return nil, &bodyError{err}
	}
	return &rawPage{
		body:     DecodeBody(raw, resp.Header.Get("Content-Type")),
		status:   resp.StatusCode,
		finalURL: finalURL,
	}, nil
}

// readBody reads the whole body, capped at limit bytes. aiohttp's
// StreamReader.read(n) returned one buffered chunk, not the body, and a
// single call silently truncated modern pages to their <head>; the loop to
// EOF (here io.ReadAll) is what keeps real pages from failing validation as
// "content too short".
func readBody(r io.Reader, limit int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, limit))
}

// isConnectionFailure reports whether a fetch failed to connect at all
// (refused, reset, no DNS answer, TLS failure), which is what makes the www.
// host worth a try. A timeout is not: falling back to www. would just hang
// again.
func isConnectionFailure(err error) bool {
	var fe *FetchError
	if !errors.As(err, &fe) || fe.Err == nil {
		return false
	}
	cause := fe.Err
	var be *bodyError
	if errors.As(cause, &be) || errors.Is(cause, errTooManyRedirects) {
		return false
	}
	if errors.Is(cause, context.DeadlineExceeded) {
		return false
	}
	var netErr net.Error
	if errors.As(cause, &netErr) && netErr.Timeout() {
		return false
	}
	var dnsErr *net.DNSError
	var opErr *net.OpError
	var certErr *tls.CertificateVerificationError
	var hostErr x509.HostnameError
	var authErr x509.UnknownAuthorityError
	switch {
	case errors.As(cause, &dnsErr), errors.As(cause, &opErr), errors.As(cause, &certErr),
		errors.As(cause, &hostErr), errors.As(cause, &authErr):
		return true
	case errors.Is(cause, syscall.ECONNREFUSED), errors.Is(cause, syscall.ECONNRESET),
		errors.Is(cause, io.EOF), errors.Is(cause, io.ErrUnexpectedEOF):
		return true // includes "server disconnected"
	}
	return false
}

// wwwFallbackURL rewrites a plain second-level host to its www. host;
// anything with a subdomain, port or credentials is left alone ("").
func wwwFallbackURL(target string) string {
	u, err := url.Parse(target)
	if err != nil || u.Host == "" || u.User != nil {
		return ""
	}
	host := u.Host
	if strings.HasPrefix(host, "www.") || strings.Contains(host, ":") || strings.Count(host, ".") != 1 {
		return ""
	}
	u.Host = "www." + host
	return u.String()
}

// hostOf is the URL's host, lowercase, without port or a www. prefix.
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	// Hostname, not a cut at the first ':': a bracketed IPv6 host carries
	// colons, and [2001:db8::1] vs [2001:4860::1] must not compare equal.
	host := strings.ToLower(u.Hostname())
	return strings.TrimPrefix(host, "www.")
}

// publicSecondLevel are second-level labels that make a two-label host a
// public suffix (co.uk, com.au, ...), so "a.co.uk" and "b.co.uk" are never
// treated as one site.
var publicSecondLevel = map[string]bool{
	"co": true, "com": true, "net": true, "org": true, "gov": true, "edu": true,
	"ac": true, "or": true, "ne": true, "go": true,
}

// sameSite reports whether one host is a subdomain of the other, e.g.
// example.com -> home.example.com or m.example.com -> example.com. A
// redirect between two unrelated domains (a parked domain bouncing to a
// lander) still fails.
func sameSite(a, b string) bool {
	parent, child := a, b
	if len(a) > len(b) {
		parent, child = b, a
	}
	if !strings.HasSuffix(child, "."+parent) {
		return false
	}
	labels := strings.Split(parent, ".")
	if len(labels) < 2 {
		return false
	}
	return len(labels) != 2 || !publicSecondLevel[labels[0]]
}

// ensureSameHost enforces the cross-host redirect policy every backend
// shares.
func (c *Client) ensureSameHost(original, final string) error {
	if c.opts.AllowCrossHostRedirects {
		return nil
	}
	from, to := hostOf(original), hostOf(final)
	if from != to && !sameSite(from, to) {
		return &RedirectError{Host: to}
	}
	return nil
}
