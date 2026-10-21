// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package webdriver is a minimal W3C WebDriver client for the deep-scrape
// Chrome container.
//
// The deep pass needs five WebDriver commands: open a session, navigate,
// read the current URL, read the page source, and close the session (plus
// setting the page-load timeout). That is a handful of JSON-over-HTTP calls,
// so this package makes them with net/http instead of depending on Selenium
// bindings and their transitive tree. The Chrome image is still
// selenium/standalone-chrome.
//
// Errors follow the W3C error codes: every failure is an *Error whose Code
// is the W3C code ("timeout", "session not created", "invalid session id",
// or "unknown error" for everything else, including the endpoint being
// unreachable). The message starts with the code, so the scraper's restart
// signatures ("invalid session id", "chrome not reachable" …) still match.
//
// Requests always go straight to the endpoint: proxy variables in the
// environment are ignored, because the container is on loopback and a proxy
// would only see (or break) the browser traffic.
package webdriver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// DefaultCommandTimeout is Selenium's default HTTP timeout for a command.
const DefaultCommandTimeout = 120 * time.Second

// navigationMargin: navigation blocks until the page-load timeout fires;
// leave room past it so the browser's own timeout error arrives instead of a
// socket timeout.
const navigationMargin = 30 * time.Second

// maxErrorText bounds endpoint error text quoted into logs.
const maxErrorText = 500

// maxReplyBytes caps a reply; page sources are the big ones.
const maxReplyBytes = 64 << 20

// W3C error codes the scraper distinguishes.
const (
	CodeTimeout           = "timeout"
	CodeSessionNotCreated = "session not created"
	CodeInvalidSessionID  = "invalid session id"
	CodeUnknown           = "unknown error"
)

// Error is a failed WebDriver command.
type Error struct {
	// Code is the W3C error code.
	Code string
	// Message is the full text, "<code>: <detail>".
	Message string
}

func (e *Error) Error() string { return e.Message }

func newError(code, detail string) *Error {
	return &Error{Code: code, Message: code + ": " + detail}
}

// HasCode reports whether err is a WebDriver *Error with the given code.
func HasCode(err error, code string) bool {
	var wd *Error
	return errors.As(err, &wd) && wd.Code == code
}

// ChromeOptions are the parts of a Chrome session the scraper sets.
type ChromeOptions struct {
	Args                   []string
	ExcludeSwitches        []string
	UseAutomationExtension bool
	PageLoadStrategy       string
	// PageLoadTimeout, when positive, goes into the capabilities' timeouts.
	PageLoadTimeout time.Duration
}

// ChromeCapabilities returns the alwaysMatch capabilities for a Chrome
// session.
func ChromeCapabilities(o ChromeOptions) map[string]any {
	args := append([]string{}, o.Args...)
	exclude := append([]string{}, o.ExcludeSwitches...)
	caps := map[string]any{
		"browserName":      "chrome",
		"pageLoadStrategy": o.PageLoadStrategy,
		"goog:chromeOptions": map[string]any{
			"args":                   args,
			"excludeSwitches":        exclude,
			"useAutomationExtension": o.UseAutomationExtension,
		},
	}
	if o.PageLoadTimeout > 0 {
		caps["timeouts"] = map[string]any{"pageLoad": o.PageLoadTimeout.Milliseconds()}
	}
	return caps
}

// Option configures a Session.
type Option func(*Session)

// WithCommandTimeout bounds every command (navigation gets more; see Navigate).
func WithCommandTimeout(d time.Duration) Option {
	return func(s *Session) { s.commandTimeout = d }
}

// WithHTTPClient replaces the HTTP client (tests).
func WithHTTPClient(c *http.Client) Option {
	return func(s *Session) { s.http = c }
}

// directClient never uses a proxy.
var directClient = &http.Client{Transport: &http.Transport{
	Proxy:               nil,
	MaxIdleConnsPerHost: 2,
	IdleConnTimeout:     90 * time.Second,
}}

// Session is one browser session on a WebDriver endpoint. A Session is used
// by one goroutine at a time (the scraper's session pool guarantees that);
// Quit may be called concurrently and more than once.
type Session struct {
	base           string
	http           *http.Client
	commandTimeout time.Duration

	mu              sync.Mutex
	id              string
	pageLoadTimeout time.Duration
}

// NewSession starts a browser session on endpoint (e.g.
// "http://127.0.0.1:4444/wd/hub") with the given alwaysMatch capabilities.
func NewSession(ctx context.Context, endpoint string, caps map[string]any, opts ...Option) (*Session, error) {
	s := &Session{base: strings.TrimRight(endpoint, "/"), http: directClient, commandTimeout: DefaultCommandTimeout}
	for _, o := range opts {
		o(s)
	}
	if t, ok := caps["timeouts"].(map[string]any); ok {
		if ms, ok := toMillis(t["pageLoad"]); ok {
			s.pageLoadTimeout = time.Duration(ms) * time.Millisecond
		}
	}
	value, payload, err := s.request(ctx, http.MethodPost, "/session", map[string]any{
		"capabilities": map[string]any{"alwaysMatch": caps, "firstMatch": []any{map[string]any{}}},
	}, 0)
	if err != nil {
		return nil, err
	}
	var id any
	if m, ok := value.(map[string]any); ok {
		id = m["sessionId"]
	}
	if isEmpty(id) {
		id = payload["sessionId"] // pre-W3C servers
	}
	if isEmpty(id) {
		return nil, newError(CodeSessionNotCreated, "the endpoint returned no session id")
	}
	s.id = stringify(id)
	return s, nil
}

func isEmpty(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case bool:
		return !x
	case float64:
		return x == 0
	}
	return false
}

func toMillis(v any) (int64, bool) {
	switch x := v.(type) {
	case int64:
		return x, true
	case int:
		return int64(x), true
	case float64:
		return int64(x), true
	}
	return 0, false
}

// stringify is Python's str() for the JSON values a reply can carry.
func stringify(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return "None"
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// ID returns the session id ("" once quit).
func (s *Session) ID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.id
}

func (s *Session) sessionPath(suffix string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.id == "" {
		return "", newError(CodeInvalidSessionID, "the session was already closed")
	}
	return "/session/" + url.PathEscape(s.id) + suffix, nil
}

// SetPageLoadTimeout sets how long navigation may take.
func (s *Session) SetPageLoadTimeout(ctx context.Context, d time.Duration) error {
	path, err := s.sessionPath("/timeouts")
	if err != nil {
		return err
	}
	if _, _, err := s.request(ctx, http.MethodPost, path, map[string]any{"pageLoad": d.Milliseconds()}, 0); err != nil {
		return err
	}
	s.mu.Lock()
	s.pageLoadTimeout = d
	s.mu.Unlock()
	return nil
}

// Navigate loads url and returns when the page-load strategy is satisfied.
func (s *Session) Navigate(ctx context.Context, target string) error {
	path, err := s.sessionPath("/url")
	if err != nil {
		return err
	}
	wait := s.commandTimeout
	s.mu.Lock()
	if s.pageLoadTimeout > 0 && s.pageLoadTimeout+navigationMargin > wait {
		wait = s.pageLoadTimeout + navigationMargin
	}
	s.mu.Unlock()
	_, _, err = s.request(ctx, http.MethodPost, path, map[string]any{"url": target}, wait)
	return err
}

// CurrentURL returns the URL the browser ended up on.
func (s *Session) CurrentURL(ctx context.Context) (string, error) {
	return s.getString(ctx, "/url")
}

// PageSource returns the rendered DOM's markup.
func (s *Session) PageSource(ctx context.Context) (string, error) {
	return s.getString(ctx, "/source")
}

func (s *Session) getString(ctx context.Context, suffix string) (string, error) {
	path, err := s.sessionPath(suffix)
	if err != nil {
		return "", err
	}
	value, _, err := s.request(ctx, http.MethodGet, path, nil, 0)
	if err != nil {
		return "", err
	}
	return stringify(value), nil
}

// Quit ends the session. Later calls do nothing.
func (s *Session) Quit(ctx context.Context) error {
	s.mu.Lock()
	id := s.id
	s.id = ""
	s.mu.Unlock()
	if id == "" {
		return nil
	}
	_, _, err := s.request(ctx, http.MethodDelete, "/session/"+url.PathEscape(id), nil, 0)
	return err
}

func (s *Session) request(ctx context.Context, method, path string, body any, timeout time.Duration) (any, map[string]any, error) {
	if timeout <= 0 {
		timeout = s.commandTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, nil, newError(CodeUnknown, err.Error())
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.base+path, reader)
	if err != nil {
		return nil, nil, newError(CodeUnknown, "WebDriver endpoint unreachable: "+err.Error())
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	resp, err := s.http.Do(req)
	if err != nil {
		// Refused, reset, timed out, truncated reply, malformed URL.
		return nil, nil, newError(CodeUnknown, "WebDriver endpoint unreachable: "+err.Error())
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxReplyBytes))
	if err != nil {
		return nil, nil, newError(CodeUnknown, "WebDriver endpoint unreachable: "+err.Error())
	}
	return decodeReply(resp.StatusCode, raw)
}

// decodeReply returns (value, payload) from a reply, or the WebDriver error
// it carries.
func decodeReply(status int, body []byte) (any, map[string]any, error) {
	var payload map[string]any
	parsed := len(body) == 0
	if len(body) == 0 {
		payload = map[string]any{}
	} else if err := json.Unmarshal(body, &payload); err == nil {
		parsed = payload != nil
	}
	value := payload["value"]
	if v, ok := value.(map[string]any); ok && !isEmpty(v["error"]) {
		code := stringify(v["error"])
		message := ""
		if !isEmpty(v["message"]) {
			message = stringify(v["message"])
		}
		return nil, nil, newError(code, truncate(message))
	}
	if status < 200 || status >= 300 {
		return nil, nil, newError(CodeUnknown, fmt.Sprintf("HTTP %d: %s", status, truncate(strings.ToValidUTF8(string(body), "�"))))
	}
	if !parsed {
		return nil, nil, newError(CodeUnknown, "response is not a JSON object")
	}
	return value, payload, nil
}

func truncate(s string) string {
	r := []rune(s)
	if len(r) > maxErrorText {
		return string(r[:maxErrorText])
	}
	return s
}
