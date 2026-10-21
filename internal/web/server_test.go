// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	lens "github.com/ElcanoTek/lens"
	"github.com/ElcanoTek/lens/internal/catalog"
	"github.com/ElcanoTek/lens/internal/config"
	"github.com/ElcanoTek/lens/internal/jobs"
)

// ---- lens submit spool (tests/test_lens_submit.py, service side) ----------

const sportsSpec = `{"categories": [{"name": "Sports", "type": "boolean", "question": "Is it about sports?"}]}`

func (h *harness) spoolEntry(name string, body []byte, request map[string]any) string {
	h.t.Helper()
	const filename = "sites.csv"
	entry := filepath.Join(h.dirs.Spool, name)
	if err := os.MkdirAll(entry, 0o700); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(entry, filename), body, 0o600); err != nil {
		h.t.Fatal(err)
	}
	req := map[string]any{"input_file": filename, "mode": "auto"}
	for k, v := range request {
		req[k] = v
	}
	raw, _ := json.Marshal(req)
	if err := os.WriteFile(filepath.Join(entry, "request.json"), raw, 0o600); err != nil {
		h.t.Fatal(err)
	}
	return entry
}

func spoolResult(t *testing.T, entry string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(entry, "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSpoolRequestBecomesADashboardJob(t *testing.T) {
	h := newHarness(t, false)
	var cats any
	_ = json.Unmarshal([]byte(sportsSpec), &cats)
	entry := h.spoolEntry("submit-abc", []byte("Domain\nexample.com\n"), map[string]any{
		"name": "deal sites", "custom_categories": cats, "decision_model": "~typesafe/jev-latest", "research_fallback": false,
	})
	h.srv.ProcessSpoolOnce()
	result := spoolResult(t, entry)
	job, ok := h.srv.Manager().Get(result["job_id"].(string))
	if !ok || job.Status != jobs.StatusQueued || h.srv.Manager().QueuePosition(job.ID) != 1 {
		t.Fatalf("job = %+v", job)
	}
	if job.Name != "deal sites" || job.DecisionModel != "~typesafe/jev-latest" || job.ResearchFallback || !job.DeepRetry {
		t.Fatalf("job = %+v", job)
	}
	var got any
	_ = json.Unmarshal(job.CustomCategories, &got)
	if !jsonEqual(got, cats) {
		t.Fatalf("categories %s", job.CustomCategories)
	}
	if b, _ := os.ReadFile(filepath.Join(h.dirs.Inputs, "sites.csv")); string(b) != "Domain\nexample.com\n" {
		t.Fatal("input not stored")
	}
	if exists(filepath.Join(entry, "sites.csv")) {
		t.Fatal("spooled input not removed")
	}
	raw, _ := os.ReadFile(filepath.Join(entry, "result.json"))
	if string(raw) != `{"job_id": "`+job.ID+`", "input_file": "sites.csv"}` {
		t.Fatalf("result.json = %s", raw)
	}
	// Persisted like any other job, so it survives a restart.
	var saved struct {
		Jobs []struct{ ID string } `json:"jobs"`
	}
	state, _ := os.ReadFile(h.dirs.StatePath())
	_ = json.Unmarshal(state, &saved)
	if len(saved.Jobs) != 1 || saved.Jobs[0].ID != job.ID {
		t.Fatalf("state %s", state)
	}
	// Each request is ingested once.
	h.srv.ProcessSpoolOnce()
	if len(h.jobs()) != 1 {
		t.Fatal("ingested twice")
	}
}

func TestSpoolNameClashKeepsBothFiles(t *testing.T) {
	h := newHarness(t, false)
	h.writeInput("sites.csv", "Domain\nother.com\n")
	entry := h.spoolEntry("submit-abc", []byte("Domain\nexample.com\n"), nil)
	h.srv.ProcessSpoolOnce()
	if got := spoolResult(t, entry)["input_file"]; got != "sites (2).csv" {
		t.Fatal(got)
	}
	if b, _ := os.ReadFile(filepath.Join(h.dirs.Inputs, "sites.csv")); string(b) != "Domain\nother.com\n" {
		t.Fatal("existing file overwritten")
	}
}

func TestSpoolInvalidRequestsAreRejectedWithoutAJob(t *testing.T) {
	for i, tc := range []struct {
		patch   map[string]any
		body    []byte
		message string
	}{
		{map[string]any{"custom_categories": map[string]any{"categories": []any{map[string]any{"name": "1bad", "type": "boolean", "question": "q"}}}}, nil, ""},
		{map[string]any{"mode": "nope"}, nil, "Invalid mode"},
		{map[string]any{"llm_model": "bad model; rm -rf"}, nil, "Invalid model id"},
		{map[string]any{"input_file": "missing.csv"}, nil, "missing"},
		{map[string]any{"input_file": "../escape.csv"}, nil, ""},
		{nil, []byte{}, "empty"},
		{map[string]any{"input_file": "binary.csv"}, nil, "missing"},
	} {
		h := newHarness(t, false)
		body := tc.body
		if body == nil {
			body = []byte("Domain\nexample.com\n")
		}
		entry := h.spoolEntry("submit-x", body, tc.patch)
		h.srv.ProcessSpoolOnce()
		result := spoolResult(t, entry)
		msg, ok := result["error"].(string)
		if !ok || !strings.Contains(msg, tc.message) || len(h.jobs()) != 0 {
			t.Errorf("case %d: result %v, jobs %d", i, result, len(h.jobs()))
		}
	}
}

func TestSpoolIncompleteSymlinkedAndSweptEntries(t *testing.T) {
	h := newHarness(t, false)
	partial := filepath.Join(h.dirs.Spool, "submit-partial")
	_ = os.MkdirAll(partial, 0o700)
	_ = os.WriteFile(filepath.Join(partial, "sites.csv"), []byte("Domain\nexample.com\n"), 0o600)
	// A symlinked entry is never followed.
	elsewhere := t.TempDir()
	_ = os.WriteFile(filepath.Join(elsewhere, "sites.csv"), []byte("Domain\nexample.com\n"), 0o600)
	_ = os.WriteFile(filepath.Join(elsewhere, "request.json"), []byte(`{"input_file": "sites.csv"}`), 0o600)
	if err := os.Symlink(elsewhere, filepath.Join(h.dirs.Spool, "submit-link")); err != nil {
		t.Fatal(err)
	}
	h.srv.ProcessSpoolOnce()
	if exists(filepath.Join(partial, "result.json")) || exists(filepath.Join(elsewhere, "result.json")) || len(h.jobs()) != 0 {
		t.Fatal("incomplete or symlinked entry ingested")
	}
	entry := h.spoolEntry("submit-done", []byte("Domain\nexample.com\n"), nil)
	h.srv.ProcessSpoolOnce()
	old := time.Now().Add(-SpoolResultTTL - 5*time.Second)
	_ = os.Chtimes(filepath.Join(entry, "result.json"), old, old)
	h.srv.ProcessSpoolOnce()
	if exists(entry) {
		t.Fatal("answered entry not swept after the TTL")
	}
}

func TestSpoolWatcherRuns(t *testing.T) {
	h := newHarness(t, true)
	entry := h.spoolEntry("submit-live", []byte("Domain\nexample.com\n"), nil)
	for range 250 {
		if exists(filepath.Join(entry, "result.json")) {
			return
		}
		waitShort()
	}
	t.Fatal("watcher never answered")
}

// ---- headers, static assets, fonts (test_input_robustness.py) -------------

func TestPagesCarryNonceCSPAndOtherResponsesAClosedOne(t *testing.T) {
	h := newHarness(t, false)
	page, body := h.get("/")
	csp := page.Header.Get("Content-Security-Policy")
	m := regexp.MustCompile(`script-src 'self' 'nonce-([A-Za-z0-9_-]+)'`).FindStringSubmatch(csp)
	if m == nil {
		t.Fatal(csp)
	}
	for _, want := range []string{"default-src 'self'", "style-src 'self';", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("csp lacks %q", want)
		}
	}
	if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "form-action") {
		t.Error(csp)
	}
	if strings.Contains(body, ` style="`) || strings.Contains(body, "<script>") || !strings.Contains(body, `<script nonce="`+m[1]+`">`) ||
		regexp.MustCompile(` on[a-z]+="`).MatchString(body) {
		t.Error("inline script/style policy violated")
	}
	if page.Header.Get("X-Content-Type-Options") != "nosniff" || page.Header.Get("X-Frame-Options") != "DENY" ||
		page.Header.Get("Referrer-Policy") != "strict-origin-when-cross-origin" || page.Header.Get("Cache-Control") != "no-store" ||
		page.Header.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Errorf("headers %v", page.Header)
	}
	if second, _ := h.get("/"); second.Header.Get("Content-Security-Policy") == csp {
		t.Error("nonce reused")
	}
	health, _ := h.get("/health")
	if health.Header.Get("Content-Security-Policy") != CSPNonPage || health.Header.Get("Cache-Control") != "no-store" {
		t.Error(health.Header)
	}
	missing, body := h.get("/nope")
	if missing.StatusCode != 404 || body != `{"detail":"Not Found"}` || missing.Header.Get("X-Frame-Options") != "DENY" {
		t.Error(missing.StatusCode, body)
	}
	wrong, _ := h.send(http.MethodGet, "/jobs", nil, "")
	if wrong.StatusCode != 405 || wrong.Header.Get("Allow") != "POST" {
		t.Error(wrong.StatusCode, wrong.Header)
	}
}

func TestStaticAssetsAreVersionedCachedAndResolve(t *testing.T) {
	h := newHarness(t, false)
	_, body := h.get("/")
	refs := regexp.MustCompile(`(?:href|src)="(/static/[^"]+)`).FindAllStringSubmatch(body, -1)
	if len(refs) == 0 {
		t.Fatal("no static references")
	}
	version := h.srv.StaticVersion()
	if !regexp.MustCompile(`^[0-9a-f]{12}$`).MatchString(version) {
		t.Fatal(version)
	}
	seen := map[string]bool{}
	for _, ref := range refs {
		u := ref[1]
		if !strings.Contains(u, "?v="+version) {
			t.Errorf("%s is unversioned", u)
		}
		if seen[u] {
			continue
		}
		seen[u] = true
		// No dead <link>/<script>/<img>: fetch every reference.
		resp, _ := h.get(strings.Split(u, "#")[0])
		if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Cache-Control"), "immutable") {
			t.Errorf("%s: %d %s", u, resp.StatusCode, resp.Header.Get("Cache-Control"))
		}
	}
	for _, tc := range []struct{ path, cache string }{
		{"/static/main.js", "no-cache"},
		{"/static/main.js?v=oldhash123", "no-cache"},
		{"/static/main.js?v=" + version, "public, max-age=31536000, immutable"},
	} {
		resp, _ := h.get(tc.path)
		if resp.StatusCode != 200 || resp.Header.Get("Cache-Control") != tc.cache || resp.Header.Get("Content-Type") != "text/javascript; charset=utf-8" {
			t.Errorf("%s: %d %v", tc.path, resp.StatusCode, resp.Header)
		}
	}
	// Revalidation is a cheap 304.
	resp, _ := h.get("/static/styles.css")
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, h.ts.URL+"/static/styles.css", nil)
	req.Header.Set("If-None-Match", resp.Header.Get("ETag"))
	again, _ := roundTrip(t, h.client, req)
	if again.StatusCode != http.StatusNotModified {
		t.Errorf("revalidation: %d", again.StatusCode)
	}
	for _, p := range []string{"/static/", "/static/nope.js", "/static/design-system", "/static/..%2fgo.mod"} {
		if resp, _ := h.get(p); resp.StatusCode != 404 {
			t.Errorf("%s: %d", p, resp.StatusCode)
		}
	}
	// The version is content-derived: one byte changes it.
	sub, _ := fs.Sub(lens.Static, "static")
	if StaticVersion(sub) != version {
		t.Error("version not derived from the embedded tree")
	}
}

func TestBundledFontsAreTheTwoBrandFaces(t *testing.T) {
	static, _ := fs.Sub(lens.Static, "static")
	var sheets, scripts []string
	_ = fs.WalkDir(static, ".", func(p string, _ fs.DirEntry, _ error) error {
		switch {
		case strings.HasSuffix(p, ".css"):
			sheets = append(sheets, p)
		case strings.HasSuffix(p, ".js"):
			scripts = append(scripts, p)
		}
		return nil
	})
	if len(sheets) == 0 {
		t.Fatal("expected stylesheets under static/")
	}
	faceRe := regexp.MustCompile(`(?s)@font-face\s*\{(.*?)\}`)
	familyRe := regexp.MustCompile(`font-family:\s*"([^"]+)"`)
	urlRe := regexp.MustCompile(`url\(\s*"([^"]+)"`)
	families := map[string]bool{}
	for _, sheet := range sheets {
		text, _ := fs.ReadFile(static, sheet)
		for _, block := range faceRe.FindAllStringSubmatch(string(text), -1) {
			fam := familyRe.FindStringSubmatch(block[1])
			if fam == nil {
				t.Fatalf("@font-face without a quoted family in %s", sheet)
			}
			families[fam[1]] = true
			for _, u := range urlRe.FindAllStringSubmatch(block[1], -1) {
				target := path.Clean(path.Join(path.Dir(sheet), u[1]))
				if st, err := fs.Stat(static, target); err != nil || st.IsDir() {
					t.Errorf("%s: src url() misses %s", sheet, u[1])
				}
			}
		}
	}
	if len(families) != 2 || !families["Nebula Sans"] || !families["Hack"] {
		t.Fatalf("families = %v", families)
	}
	for _, licence := range []string{"design-system/fonts/nebula-sans/OFL.txt", "design-system/fonts/hack/LICENSE.md"} {
		if _, err := fs.Stat(static, licence); err != nil {
			t.Errorf("licence %s missing", licence)
		}
	}
	// Self-hosted only: no CDN or Google Fonts request anywhere in the UI.
	scanned := map[string][]byte{}
	for _, p := range append(sheets, scripts...) {
		scanned[p], _ = fs.ReadFile(static, p)
	}
	tmpls, _ := fs.Glob(lens.Templates, "templates/*.html")
	for _, p := range tmpls {
		scanned[p], _ = fs.ReadFile(lens.Templates, p)
	}
	for p, text := range scanned {
		for _, host := range []string{"fonts.googleapis.com", "fonts.gstatic.com", "cdn.jsdelivr.net", "unpkg.com"} {
			if bytes.Contains(text, []byte(host)) {
				t.Errorf("%s loads from %s", p, host)
			}
		}
	}
}

func TestNumericReadoutsRequestTabularFigures(t *testing.T) {
	css, _ := fs.ReadFile(lens.Static, "static/styles.css")
	if bytes.Contains(css, []byte("proportional-nums")) {
		t.Fatal("proportional figures re-enabled")
	}
	stripped := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAll(css, nil)
	blocks := regexp.MustCompile(`([^{}]+)\{([^{}]*font-variant-numeric\s*:\s*tabular-nums[^{}]*)\}`).FindAllSubmatch(stripped, -1)
	if len(blocks) == 0 {
		t.Fatal("no rule asks for tabular-nums")
	}
	selectors := map[string]bool{}
	for _, b := range blocks {
		for _, part := range strings.Split(string(b[1]), ",") {
			if s := strings.TrimSpace(part); s != "" {
				selectors[s] = true
			}
		}
	}
	if !selectors["table"] {
		t.Fatal("the base-layer table rule is gone")
	}
	index, _ := fs.ReadFile(lens.Templates, "templates/index.html")
	mainJS, _ := fs.ReadFile(lens.Static, "static/main.js")
	markup := string(index) + "\n" + string(mainJS)
	for sel := range selectors {
		if sel == "table" {
			continue
		}
		for _, tok := range regexp.MustCompile(`[.#][A-Za-z0-9_-]+`).FindAllString(sel, -1) {
			if !strings.Contains(markup, tok[1:]) {
				t.Errorf("%s: %s matches nothing in the markup", sel, tok)
			}
		}
	}
}

// ---- sign-in, health, startup (test_central_auth_endpoints.py, web side) --

func TestHealthLogoutAndSignedOut(t *testing.T) {
	h := newHarness(t, false)
	resp, body := h.get("/health")
	var health map[string]string
	_ = json.Unmarshal([]byte(body), &health)
	if resp.StatusCode != 200 || health["status"] != "ok" || health["auth_mode"] != "elcano" || health["time"] == "" {
		t.Fatal(body)
	}
	head, _ := h.send(http.MethodHead, "/health", nil, "")
	if head.StatusCode != 200 {
		t.Fatal(head.StatusCode)
	}
	// The legacy logout still hands over to the Elcano auth logout.
	logout := h.postForm("/logout", nil)
	if logout.StatusCode != 303 || location(logout) != "https://auth.elcanotek.com/logout" {
		t.Fatal(logout.StatusCode, location(logout))
	}
	resp, body = h.get("/signed-out")
	if resp.StatusCode != 200 || !strings.Contains(body, "You are signed out") || !strings.Contains(body, `href="/auth/login?next=%2F"`) ||
		!strings.Contains(body, `href="https://auth.elcanotek.com"`) || resp.Header.Get("Location") != "" ||
		!strings.HasPrefix(body, "<!-- SPDX-License-Identifier: BUSL-1.1 -->\n") {
		t.Fatal(resp.StatusCode, body)
	}
	// Signed in already: /login goes home. Callback/back-channel are
	// central-only.
	if resp, _ := h.get("/auth/login"); resp.StatusCode != 303 || location(resp) != "/" {
		t.Fatal(resp.StatusCode, location(resp))
	}
	if resp, _ := h.get("/auth/callback?code=x&state=y"); resp.StatusCode != 404 {
		t.Fatal(resp.StatusCode)
	}
}

func TestBackchannelBodyLimits(t *testing.T) {
	h := newHarness(t, false)
	post := func(body io.Reader, length int64, chunked bool) int {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, h.ts.URL+"/auth/backchannel-logout", body)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if chunked {
			req.ContentLength = -1
		} else {
			req.ContentLength = length
		}
		resp, _ := roundTrip(t, h.client, req)
		return resp.StatusCode
	}
	big := "logout_token=" + strings.Repeat("x", 20_001)
	if code := post(strings.NewReader(big), int64(len(big)), false); code != 413 {
		t.Errorf("oversized: %d", code)
	}
	if code := post(io.NopCloser(strings.NewReader("logout_token=x")), 0, true); code != 411 {
		t.Errorf("chunked: %d", code)
	}
	if code := post(strings.NewReader("logout_token=x"), 14, false); code != 404 {
		t.Errorf("elcano mode: %d", code)
	}
}

func TestStartupFailsWhenCentralAuthIsMisconfigured(t *testing.T) {
	env := map[string]string{
		"LENS_AUTH_MODE": "central", "LENS_SESSION_SECRET": "test-session-secret-with-at-least-32-bytes",
		"LENS_ACCESS_DB": filepath.Join(t.TempDir(), "access.db"), "AUTH_ISSUER_URL": "https://auth.example.com",
		"LENS_PUBLIC_URL": "https://lens.example.com", "AUTH_CLIENT_ID": "lens",
		"AUTH_CLIENT_SECRET": "a-test-secret-that-is-at-least-32-bytes", "AUTH_SIGNING_PUBKEY": "",
	}
	getenv := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	_, err := New(Options{Home: t.TempDir(), Getenv: getenv, Catalog: &fakeCatalog{}})
	if err == nil || !strings.Contains(err.Error(), "AUTH_SIGNING_PUBKEY") {
		t.Fatalf("err = %v", err)
	}
	env["LENS_AUTH_MODE"] = "bogus"
	if _, err := New(Options{Home: t.TempDir(), Getenv: getenv, Catalog: &fakeCatalog{}}); err == nil {
		t.Fatal("bogus mode accepted")
	}
}

func TestCentralModeEndToEndWiring(t *testing.T) {
	a := newTestAuth(t)
	env := map[string]string{
		"LENS_AUTH_MODE": "central", "LENS_SESSION_SECRET": "test-session-secret-with-at-least-32-bytes",
		"LENS_ACCESS_DB": filepath.Join(t.TempDir(), "access.db"), "LENS_AUTH_COOKIE_SECURE": "0",
		"AUTH_ALLOW_INSECURE_HTTP": "1", "AUTH_ISSUER_URL": "http://auth.example.com",
		"LENS_PUBLIC_URL": "http://lens.example.com", "AUTH_CLIENT_ID": "lens",
		"AUTH_CLIENT_SECRET": "a-test-secret-that-is-at-least-32-bytes", "AUTH_SIGNING_PUBKEY": a.pub,
	}
	getenv := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	srv, err := New(Options{Home: t.TempDir(), Getenv: getenv, Catalog: &fakeCatalog{}})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(p string) reply {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+p, nil)
		resp, _ := roundTrip(t, client, req)
		return resp
	}
	// Anonymous page visit → local login route with the path preserved.
	if resp := get("/?x=1"); resp.StatusCode != 303 || location(resp) != "/auth/login?next="+url.QueryEscape("/?x=1") {
		t.Fatal(resp.StatusCode, location(resp))
	}
	// /auth/login starts a transaction and redirects to Auth.
	resp := get("/auth/login?next=/")
	if resp.StatusCode != 303 || !strings.HasPrefix(location(resp), "http://auth.example.com/authorize?") {
		t.Fatal(resp.StatusCode, location(resp))
	}
	if !strings.Contains(strings.ToLower(resp.Header.Get("Set-Cookie")), "httponly") {
		t.Fatal(resp.Header)
	}
	if resp := get("/health"); resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	if srv.Auth().Mode() != "central" {
		t.Fatal(srv.Auth().Mode())
	}
	if resp := get("/auth/callback"); resp.StatusCode != 400 {
		t.Fatal(resp.StatusCode)
	}
}

func TestForwardedProtoTrustedOnlyFromLoopbackProxy(t *testing.T) {
	var seen string
	h := proxyHeaders(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = r.URL.Scheme + "://" + r.URL.Host + " " + ClientIP(r)
	}))
	for _, tc := range []struct{ remote, want string }{
		{"127.0.0.1:5000", "https://lens.example.com 203.0.113.9"},
		{"10.0.0.2:5000", "http://lens.example.com 10.0.0.2"},
	} {
		req := httptest.NewRequest(http.MethodGet, "http://lens.example.com/", nil)
		req.RemoteAddr = tc.remote
		req.Header.Set("X-Forwarded-Proto", "https")
		req.Header.Set("X-Forwarded-For", "203.0.113.9")
		h.ServeHTTP(httptest.NewRecorder(), req)
		if seen != tc.want {
			t.Errorf("%s: %s", tc.remote, seen)
		}
	}
}

// ---- API/MCP mounts --------------------------------------------------------

func TestAPIAndMCPAreMounted(t *testing.T) {
	t.Setenv("LENS_API_TOKEN_DB", filepath.Join(t.TempDir(), "tokens.db"))
	h := newHarness(t, false, func(o *Options) {
		o.Mounts = []MountFunc{APIMounts(catalog.NewSource(""), config.Default())}
	})
	for _, p := range []string{"/api/v1/whoami", "/api/v1/jobs"} {
		resp, body := h.get(p)
		// Bearer-token surfaces ignore the browser cookie.
		if resp.StatusCode != 401 || resp.Header.Get("X-Frame-Options") != "DENY" {
			t.Errorf("%s: %d %s", p, resp.StatusCode, body)
		}
	}
	resp, _ := h.send(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`), "application/json")
	if resp.StatusCode == 404 {
		t.Fatal("mcp not mounted")
	}
}

// ---- parallel jobs (tests/test_parallel_jobs.py) ---------------------------

func TestMaxParallelJobsIsClamped(t *testing.T) {
	for value, want := range map[string]int{"": 1, "1": 1, "3": 3, "0": 1, "-2": 1, "50": 8, "lots": 1} {
		t.Setenv("LENS_MAX_PARALLEL_JOBS", value)
		if got := jobs.MaxParallelFromEnv(); got != want {
			t.Errorf("%q: %d", value, got)
		}
	}
}

func TestSlotsFillAndAFreedSlotStartsTheNextJob(t *testing.T) {
	gate := t.TempDir()
	// Each job runs until its input's gate file appears.
	script := `for a in "$@"; do case "$prev" in --input-csv) in="$a";; esac; prev="$a"; done
g="` + gate + `/$(basename "$in")"; while [ ! -e "$g" ]; do sleep 0.02; done`
	h := newHarness(t, true, func(o *Options) {
		m := jobs.NewManager(o.Home)
		m.Executable = fakeExecutable(t, o.Home, script)
		m.BaseArgs = nil
		m.MaxParallel = 2
		o.Manager = m
	})
	m := h.srv.Manager()
	ids := make([]string, 0, 3)
	for _, name := range []string{"f0.csv", "f1.csv", "f2.csv"} {
		j, err := m.Create(jobs.NewJob{InputFile: name, Mode: "auto"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, j.ID)
		time.Sleep(5 * time.Millisecond)
	}
	release := func(name string) { _ = os.WriteFile(filepath.Join(gate, name), nil, 0o600) }
	t.Cleanup(func() {
		for _, n := range []string{"f0.csv", "f1.csv", "f2.csv", "late.csv"} {
			release(n)
		}
	})
	waitStatus(t, h, ids[0], jobs.StatusRunning)
	waitStatus(t, h, ids[1], jobs.StatusRunning)
	if j, _ := m.Get(ids[2]); j.Status != jobs.StatusQueued || m.QueuePosition(ids[2]) != 1 {
		t.Fatalf("third job %s", j.Status)
	}
	release("f0.csv")
	waitStatus(t, h, ids[0], jobs.StatusCompleted)
	waitStatus(t, h, ids[2], jobs.StatusRunning)
	if m.QueueLength() != 0 {
		t.Fatal("queue not drained")
	}
	release("f1.csv")
	release("f2.csv")
	waitStatus(t, h, ids[2], jobs.StatusCompleted)
	// A job queued later starts as soon as a slot is free.
	late, _ := m.Create(jobs.NewJob{InputFile: "late.csv", Mode: "auto"})
	waitStatus(t, h, late.ID, jobs.StatusRunning)
	release("late.csv")
	waitStatus(t, h, late.ID, jobs.StatusCompleted)
	if m.CurrentID() != "" {
		t.Fatal("current job not cleared")
	}
}

// ---- lens serve -------------------------------------------------------------

func TestServeStartsAndShutsDownOnSignal(t *testing.T) {
	home := t.TempDir()
	t.Setenv("LENS_HOME", home)
	t.Setenv("LENS_AUTH_MODE", "elcano")
	t.Setenv("LENS_API_TOKEN_DB", filepath.Join(home, "tokens.db"))
	done := make(chan int, 1)
	ready := make(chan string, 1)
	go func() {
		done <- serve([]string{"--addr", "127.0.0.1:0"}, io.Discard, func(addr string) { ready <- addr })
	}()
	var addr string
	select {
	case addr = <-ready:
	case code := <-done:
		t.Fatalf("serve exited %d", code)
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not start")
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+addr+"/health", nil)
	resp, _ := roundTrip(t, http.DefaultClient, req)
	if resp.StatusCode != 200 || !isRegular(filepath.Join(home, "managed-files", "outputs")) && !exists(filepath.Join(home, "managed-files", "spool")) {
		t.Fatal(resp.StatusCode)
	}
	_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d", code)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("no graceful shutdown")
	}
}
