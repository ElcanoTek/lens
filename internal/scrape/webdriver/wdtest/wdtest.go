// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package wdtest is a fake W3C WebDriver endpoint for tests. It serves the
// handful of commands the deep pass uses under /wd/hub, records every
// request, and can be told to answer a command with a W3C error, a non-JSON
// gateway page, or anything else.
package wdtest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Page is the source every session serves.
const Page = "<html><head><title>Fake page</title></head><body><p>Rendered body</p></body></html>"

// Request is one recorded command.
type Request struct {
	Method, Path string
	// Body is the decoded JSON body (nil when there was none).
	Body any
}

type reply struct {
	status int
	body   any // []byte is sent raw; anything else as JSON
}

// Server is the fake endpoint.
type Server struct {
	// Endpoint is the base URL, without /wd/hub.
	Endpoint string

	mu       sync.Mutex
	requests []Request
	sessions map[string]bool
	url      string
	page     string
	counter  int
	fail     map[string]reply
	redirect string
}

// New starts a fake endpoint that is closed when the test ends.
func New(t testing.TB) *Server {
	s := &Server{sessions: map[string]bool{}, url: "about:blank", page: Page, fail: map[string]reply{}}
	ts := httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(ts.Close)
	s.Endpoint = ts.URL
	return s
}

// Fail makes (method, suffix) answer status with body; suffix is the path
// after /session/<id> ("" with POST is the new-session command).
func (s *Server) Fail(method, suffix string, status int, body any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail[method+" "+suffix] = reply{status, body}
}

// ClearFailures removes every Fail.
func (s *Server) ClearFailures() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = map[string]reply{}
}

// SetPage changes the page source served from now on.
func (s *Server) SetPage(page string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.page = page
}

// RedirectTo makes every later navigation end up on url, as a server-side
// redirect would.
func (s *Server) RedirectTo(url string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.redirect = url
}

// ForgetSessions drops every session, as if Chrome crashed.
func (s *Server) ForgetSessions() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions = map[string]bool{}
}

// SessionCount is the number of open sessions.
func (s *Server) SessionCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

// Requests returns a copy of every recorded request.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &body)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, Request{Method: r.Method, Path: r.URL.Path, Body: body})

	if r.URL.Path == "/wd/hub/status" {
		send(w, http.StatusOK, map[string]any{"value": map[string]any{"ready": true}})
		return
	}
	const prefix = "/wd/hub/session"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		send(w, http.StatusNotFound, map[string]any{"value": map[string]any{"error": "unknown command", "message": r.URL.Path}})
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	if r.Method == http.MethodPost && rest == "" {
		if f, ok := s.fail["POST "]; ok {
			send(w, f.status, f.body)
			return
		}
		s.counter++
		id := fmt.Sprintf("s%d", s.counter)
		s.sessions[id] = true
		var caps any
		if m, ok := body.(map[string]any); ok {
			if c, ok := m["capabilities"].(map[string]any); ok {
				caps = c["alwaysMatch"]
			}
		}
		send(w, http.StatusOK, map[string]any{"value": map[string]any{"sessionId": id, "capabilities": caps}})
		return
	}
	id, suffix, _ := strings.Cut(strings.TrimPrefix(rest, "/"), "/")
	if suffix != "" {
		suffix = "/" + suffix
	}
	if f, ok := s.fail[r.Method+" "+suffix]; ok {
		send(w, f.status, f.body)
		return
	}
	if !s.sessions[id] {
		send(w, http.StatusNotFound, map[string]any{"value": map[string]any{"error": "invalid session id", "message": "no such session"}})
		return
	}
	m, _ := body.(map[string]any)
	switch {
	case r.Method == http.MethodDelete && suffix == "":
		delete(s.sessions, id)
		send(w, http.StatusOK, map[string]any{"value": nil})
	case r.Method == http.MethodPost && suffix == "/timeouts":
		send(w, http.StatusOK, map[string]any{"value": nil})
	case r.Method == http.MethodPost && suffix == "/url":
		if u, ok := m["url"].(string); ok {
			s.url = u
		}
		if s.redirect != "" {
			s.url = s.redirect
		}
		send(w, http.StatusOK, map[string]any{"value": nil})
	case r.Method == http.MethodGet && suffix == "/url":
		send(w, http.StatusOK, map[string]any{"value": s.url})
	case r.Method == http.MethodGet && suffix == "/source":
		send(w, http.StatusOK, map[string]any{"value": s.page})
	default:
		send(w, http.StatusNotFound, map[string]any{"value": map[string]any{"error": "unknown command", "message": "?"}})
	}
}

func send(w http.ResponseWriter, status int, body any) {
	raw, ok := body.([]byte)
	if !ok {
		raw, _ = json.Marshal(body)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}
