// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package web

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ElcanoTek/lens/internal/auth"
	"github.com/ElcanoTek/lens/internal/catalog"
	"github.com/ElcanoTek/lens/internal/jobs"
)

// fakeCatalog is a pre-seeded model catalog (the Python tests patched
// _model_catalog_cache the same way).
type fakeCatalog struct {
	mu  sync.Mutex
	cat catalog.Catalog
}

func (f *fakeCatalog) Get(context.Context) catalog.Catalog {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cat
}

func (f *fakeCatalog) Allowed(kind, id string) bool {
	if id == catalog.Recommended(kind) {
		return true
	}
	for _, o := range f.Get(context.Background()).Options(kind) {
		if o.ID == id {
			return true
		}
	}
	return false
}

// testAuth mints elcano_auth cookies the way the auth service does.
type testAuth struct {
	priv ed25519.PrivateKey
	pub  string
}

func newTestAuth(t *testing.T) *testAuth {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &testAuth{priv: priv, pub: base64.StdEncoding.EncodeToString(pub)}
}

func (a *testAuth) cookie(email string) string {
	now := time.Now().Unix()
	payload, _ := json.Marshal(map[string]any{"email": email, "tenant": "elcanotek.com", "iat": now, "exp": now + 3600})
	body := base64.RawURLEncoding.EncodeToString(payload)
	sig := ed25519.Sign(a.priv, []byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func (a *testAuth) provider() AuthProvider {
	return NewAuthProvider(&auth.ElcanoProvider{Verifier: auth.NewCookieVerifier(a.pub, "elcano_auth", "https://auth.elcanotek.com")})
}

type harness struct {
	t       *testing.T
	home    string
	dirs    jobs.Dirs
	srv     *Server
	ts      *httptest.Server
	client  *http.Client
	auth    *testAuth
	catalog *fakeCatalog
	cancel  context.CancelFunc
}

type harnessOpt func(*Options)

// fakeExecutable writes a stand-in for `lens run`.
func fakeExecutable(t *testing.T, dir, script string) string {
	t.Helper()
	exe := filepath.Join(dir, "fake-lens")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"+script), 0o700); err != nil { //nolint:gosec // test helper must be executable
		t.Fatal(err)
	}
	return exe
}

// newHarness starts a server on a temp home with a signed-in client. The
// worker is not started unless start is true (most route tests only look
// at the queue).
func newHarness(t *testing.T, start bool, opts ...harnessOpt) *harness {
	t.Helper()
	home := t.TempDir()
	a := newTestAuth(t)
	cat := &fakeCatalog{}
	manager := jobs.NewManager(home)
	manager.Executable = fakeExecutable(t, home, "exit 0")
	manager.BaseArgs = nil
	o := Options{Home: home, Auth: a.provider(), Catalog: cat, Manager: manager, SpoolInterval: 20 * time.Millisecond}
	for _, fn := range opts {
		fn(&o)
	}
	srv, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if start {
		srv.Start(ctx)
	}
	ts := httptest.NewServer(srv.Handler())
	jar, _ := cookiejar.New(nil)
	u, _ := url.Parse(ts.URL)
	jar.SetCookies(u, []*http.Cookie{{Name: "elcano_auth", Value: a.cookie("tester@elcanotek.com")}})
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	h := &harness{t: t, home: home, dirs: srv.Dirs(), srv: srv, ts: ts, client: client, auth: a, catalog: cat, cancel: cancel}
	t.Cleanup(func() {
		ts.Close()
		cancel()
		srv.Wait()
	})
	return h
}

func (h *harness) do(method, path string, body io.Reader, contentType string) *http.Response {
	h.t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, h.ts.URL+path, body)
	if err != nil {
		h.t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp
}

func (h *harness) get(path string) (*http.Response, string) {
	h.t.Helper()
	resp := h.do(http.MethodGet, path, nil, "")
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func (h *harness) postForm(path string, values url.Values) *http.Response {
	h.t.Helper()
	resp := h.do(http.MethodPost, path, strings.NewReader(values.Encode()), "application/x-www-form-urlencoded")
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp
}

func (h *harness) writeInput(name, content string) string {
	h.t.Helper()
	p := filepath.Join(h.dirs.Inputs, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		h.t.Fatal(err)
	}
	return p
}

func (h *harness) writeOutput(name, content string) string {
	h.t.Helper()
	p := filepath.Join(h.dirs.Outputs, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		h.t.Fatal(err)
	}
	return p
}

func (h *harness) jobs() []jobs.Job { return h.srv.Manager().List() }

// newest returns jobs oldest-first.
func (h *harness) oldestFirst() []jobs.Job {
	list := h.jobs()
	for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
		list[i], list[j] = list[j], list[i]
	}
	return list
}

func location(resp *http.Response) string { return resp.Header.Get("Location") }

func waitShort() { time.Sleep(20 * time.Millisecond) }

func mustTime(t *testing.T, path string) time.Time {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.ModTime()
}
