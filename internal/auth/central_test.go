// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package auth

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu  sync.Mutex
	now int64
}

func (c *fakeClock) set(v int64) {
	c.mu.Lock()
	c.now = v
	c.mu.Unlock()
}

func (c *fakeClock) add(d int64) {
	c.mu.Lock()
	c.now += d
	c.mu.Unlock()
}

func (c *fakeClock) time() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Unix(c.now, 0)
}

func openTestStore(t *testing.T, idle, absolute int64) (*Store, *fakeClock) {
	t.Helper()
	store, err := OpenStore(context.Background(), filepath.Join(t.TempDir(), "access.db"), idle, absolute)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	clock := &fakeClock{now: 1000}
	store.SetClock(clock.time)
	return store, clock
}

func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func isCentral(err error) bool { return errors.Is(err, ErrCentralAuth) }

func TestLocalAllowlistSessionsAndBackchannelRevocation(t *testing.T) {
	ctx := context.Background()
	store, clock := openTestStore(t, DefaultIdleSeconds, DefaultAbsoluteSeconds)
	if ok, _ := store.IsAllowed(ctx, "alice@example.com"); ok {
		t.Fatal("allowed before grant")
	}
	if err := store.GrantAccess(ctx, "Alice@Example.com"); err != nil {
		t.Fatal(err)
	}
	first, err := store.CreateSession(ctx, "account-123", "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.CreateSession(ctx, "account-456", "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	event := LogoutEvent{EventID: "event-123", Issuer: "https://auth.example.com", Subject: "account-123", IssuedAt: 1001}
	clock.set(1002)
	if ok, err := store.ConsumeLogoutEvent(ctx, event); !ok || err != nil {
		t.Fatalf("first consume %v %v", ok, err)
	}
	clock.set(1003)
	if ok, _ := store.ConsumeLogoutEvent(ctx, event); ok {
		t.Fatal("replay consumed")
	}
	clock.set(1004)
	if id, _ := store.GetIdentity(ctx, first); id != nil {
		t.Fatal("logged-out subject still signed in")
	}
	if id, _ := store.GetIdentity(ctx, other); id == nil || id.Subject != "account-456" {
		t.Fatalf("other subject: %+v", id)
	}
	if id, _ := store.GetIdentity(ctx, "not-ascii-é"); id != nil {
		t.Fatal("non-ASCII token")
	}
}

func TestStoreRefusesUnlistedEmailsAndBadTimeouts(t *testing.T) {
	ctx := context.Background()
	store, _ := openTestStore(t, 60, 120)
	if _, err := store.CreateSession(ctx, "account-1", "bob@example.com"); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("unlisted: %v", err)
	}
	_ = store.GrantAccess(ctx, "bob@example.com")
	if _, err := store.CreateSession(ctx, "", "bob@example.com"); !isCentral(err) {
		t.Fatalf("empty subject: %v", err)
	}
	if _, err := store.CreateSession(ctx, "s", "not-an-email"); !errors.Is(err, ErrInvalidEmail) {
		t.Fatalf("bad email: %v", err)
	}
	for _, pair := range [][2]int64{{0, 10}, {10, 0}, {20, 10}} {
		if _, err := OpenStore(ctx, filepath.Join(t.TempDir(), "a.db"), pair[0], pair[1]); err == nil {
			t.Errorf("timeouts %v accepted", pair)
		}
	}
}

func TestLogoutTokenValidation(t *testing.T) {
	key := newSigner(t)
	keys := []string{key.pubB64()}
	raw := key.logoutToken(t, nil)
	event, err := VerifyLogoutToken(raw, "https://auth.example.com", "lens", keys, i64(1010))
	if err != nil || event.EventID != "event-123" || event.Subject != "account-123" || event.IssuedAt != 1000 {
		t.Fatalf("valid: %+v %v", event, err)
	}
	if _, err := VerifyLogoutToken(raw, "https://auth.example.com/", "lens", keys, i64(1010)); err != nil {
		t.Fatalf("issuer trailing slash is tolerated for logout tokens: %v", err)
	}
	if _, err := VerifyLogoutToken(raw, "https://auth.example.com", "explorer", keys, i64(1010)); !isCentral(err) {
		t.Fatal("wrong audience accepted")
	}
	for name, overrides := range map[string]map[string]any{
		"numeric issuer": {"iss": 123},
		"no exp":         {"exp": nil},
		"expired":        {"exp": 1000},
		"string exp":     {"exp": "soon"},
		"bool exp":       {"exp": true},
		"float exp":      {"exp": 1300.5},
		"nonce present":  {"nonce": "x"},
		"no events":      {"events": nil},
		"wrong event":    {"events": map[string]any{"other": map[string]any{}}},
		"empty sub":      {"sub": ""},
		"long jti":       {"jti": strings.Repeat("j", 256)},
		"future iat":     {"iat": 5000},
		"zero iat":       {"iat": 0},
	} {
		if _, err := VerifyLogoutToken(key.logoutToken(t, overrides), "https://auth.example.com", "lens", keys, i64(1200)); !isCentral(err) {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestAccessTokenRejectsWrongAudienceAndInvalidVersion(t *testing.T) {
	key := newSigner(t)
	keys := []string{key.pubB64()}
	raw := key.accessToken(t, "grant", 4, nil)
	event, err := VerifyAccessToken(raw, "https://auth.example.com", "lens", keys, i64(1010))
	want := AccessProvisionEvent{EventID: "access-4", Subject: "account-123", Email: "alice@example.com",
		Issuer: "https://auth.example.com", IssuedAt: 1000, Version: 4, Allowed: true}
	if err != nil || event != want {
		t.Fatalf("event %+v %v", event, err)
	}
	revoke, err := VerifyAccessToken(key.accessToken(t, "revoke", 5, nil), "https://auth.example.com", "lens", keys, i64(1010))
	if err != nil || revoke.Allowed {
		t.Fatalf("revoke %+v %v", revoke, err)
	}
	if _, err := VerifyAccessToken(raw, "https://auth.example.com", "explorer", keys, i64(1010)); !isCentral(err) {
		t.Fatal("wrong audience accepted")
	}
	for name, token := range map[string]string{
		"bool version":  key.accessToken(t, "grant", true, nil),
		"zero version":  key.accessToken(t, "grant", 0, nil),
		"float version": key.accessToken(t, "grant", 2.5, nil),
		"bad action":    key.accessToken(t, "delete", 1, nil),
		"bad email":     key.accessToken(t, "grant", 1, map[string]any{"email": "nope"}),
		"no email":      key.accessToken(t, "grant", 1, map[string]any{"email": nil}),
		"nonce":         key.accessToken(t, "grant", 1, map[string]any{"nonce": "n"}),
	} {
		if _, err := VerifyAccessToken(token, "https://auth.example.com", "lens", keys, i64(1010)); !isCentral(err) {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestAccessProvisioningIsOrderedAndRevocationEndsSessions(t *testing.T) {
	ctx := context.Background()
	store, clock := openTestStore(t, DefaultIdleSeconds, DefaultAbsoluteSeconds)
	grant := AccessProvisionEvent{EventID: "grant-2", Subject: "account-123", Email: "alice@example.com",
		Issuer: "https://auth.example.com", IssuedAt: 1000, Version: 2, Allowed: true}
	staleRevoke := grant
	staleRevoke.EventID, staleRevoke.IssuedAt, staleRevoke.Version, staleRevoke.Allowed = "revoke-1", 1001, 1, false
	revoke := grant
	revoke.EventID, revoke.IssuedAt, revoke.Version, revoke.Allowed = "revoke-3", 1002, 3, false

	clock.set(1010)
	if ok, err := store.ApplyAccessProvisioning(ctx, grant); !ok || err != nil {
		t.Fatalf("grant %v %v", ok, err)
	}
	clock.set(1011)
	issued, err := store.CreateSession(ctx, grant.Subject, grant.Email)
	if err != nil {
		t.Fatal(err)
	}
	clock.set(1012)
	if ok, _ := store.ApplyAccessProvisioning(ctx, staleRevoke); ok {
		t.Fatal("stale revoke applied")
	}
	if ok, _ := store.IsAllowed(ctx, grant.Email); !ok {
		t.Fatal("stale revoke took effect")
	}
	clock.set(1013)
	if ok, _ := store.ApplyAccessProvisioning(ctx, revoke); !ok {
		t.Fatal("revoke not applied")
	}
	if ok, _ := store.IsAllowed(ctx, grant.Email); ok {
		t.Fatal("still allowed")
	}
	clock.set(1014)
	if id, _ := store.GetIdentity(ctx, issued); id != nil {
		t.Fatal("session survived revoke")
	}
	clock.set(1015)
	if ok, _ := store.ApplyAccessProvisioning(ctx, revoke); ok {
		t.Fatal("replayed revoke applied")
	}
}

func TestAccessProvisioningEmailChangeDisablesTheOldAddress(t *testing.T) {
	ctx := context.Background()
	store, _ := openTestStore(t, DefaultIdleSeconds, DefaultAbsoluteSeconds)
	first := AccessProvisionEvent{EventID: "e1", Subject: "acct", Email: "old@example.com", Issuer: "https://a", IssuedAt: 1, Version: 1, Allowed: true}
	if _, err := store.ApplyAccessProvisioning(ctx, first); err != nil {
		t.Fatal(err)
	}
	session, _ := store.CreateSession(ctx, "acct", "old@example.com")
	moved := first
	moved.EventID, moved.Email, moved.Version = "e2", "new@example.com", 2
	if ok, _ := store.ApplyAccessProvisioning(ctx, moved); !ok {
		t.Fatal("move not applied")
	}
	if ok, _ := store.IsAllowed(ctx, "old@example.com"); ok {
		t.Fatal("old address still allowed")
	}
	if ok, _ := store.IsAllowed(ctx, "new@example.com"); !ok {
		t.Fatal("new address not allowed")
	}
	if id, _ := store.GetIdentity(ctx, session); id != nil {
		t.Fatal("old-address session survived")
	}
}

func TestAccessAdminCLIGrantsListsAndRevokes(t *testing.T) {
	ctx := context.Background()
	env := envMap(map[string]string{"LENS_ACCESS_DB": filepath.Join(t.TempDir(), "access.db")})
	var out, errOut bytes.Buffer
	run := func(args ...string) int {
		out.Reset()
		errOut.Reset()
		return accessMain(ctx, args, &out, &errOut, env)
	}
	if code := run("grant", "Alice@Example.com"); code != 0 || out.String() != "granted alice@example.com\n" {
		t.Fatalf("grant: %d %q %q", code, out.String(), errOut.String())
	}
	if code := run("list"); code != 0 || !strings.Contains(out.String(), "alice@example.com") {
		t.Fatalf("list: %d %q", code, out.String())
	}
	if code := run("revoke", "alice@example.com"); code != 0 || !strings.Contains(out.String(), "revoked alice@example.com") {
		t.Fatalf("revoke: %d %q", code, out.String())
	}
	store, err := OpenStoreFromEnv(ctx, env)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := store.IsAllowed(ctx, "alice@example.com"); ok {
		t.Fatal("still allowed")
	}
	_ = store.Close()
	// Revoking an email that is not allowed changes nothing and must say so.
	if code := run("revoke", "alice@example.com"); code != 1 || !strings.Contains(errOut.String(), "not currently allowed") ||
		strings.Contains(out.String(), "revoked") {
		t.Fatalf("second revoke: %d %q %q", code, out.String(), errOut.String())
	}
	// A malformed email is an operator error, not a crash.
	if code := run("grant", "not-an-email"); code != 1 || !strings.Contains(errOut.String(), "valid email") {
		t.Fatalf("bad email: %d %q", code, errOut.String())
	}
	for _, args := range [][]string{{}, {"grant"}, {"bogus"}, {"list", "x"}} {
		if code := run(args...); code != 2 {
			t.Errorf("%v: exit %d", args, code)
		}
	}
}

// fakeIssuer stands in for Auth's /token and /jwks.json.
type fakeIssuer struct {
	t      *testing.T
	server *httptest.Server

	mu          sync.Mutex
	tokenStatus int
	tokenBody   func(form url.Values) []byte
	jwks        func() ([]byte, int)
	jwksCalls   int
	lastRequest *http.Request
	lastForm    url.Values
}

func newFakeIssuer(t *testing.T) *fakeIssuer {
	f := &fakeIssuer{t: t, tokenStatus: http.StatusOK}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.URL.Path {
		case "/token":
			body, _ := io.ReadAll(r.Body)
			form, _ := url.ParseQuery(string(body))
			f.lastRequest, f.lastForm = r, form
			w.WriteHeader(f.tokenStatus)
			if f.tokenBody != nil {
				_, _ = w.Write(f.tokenBody(form))
			}
		case "/jwks.json":
			f.jwksCalls++
			if f.jwks == nil {
				http.Error(w, "no JWKS here", http.StatusServiceUnavailable)
				return
			}
			body, status := f.jwks()
			w.WriteHeader(status)
			_, _ = w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeIssuer) answer(body any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokenBody = func(url.Values) []byte { return mustJSON(f.t, body) }
}

func (f *fakeIssuer) client(t *testing.T) *Client {
	t.Helper()
	client, err := NewClient(f.server.URL, "https://lens.example.com", "lens", "a-random-client-secret-with-32-bytes", true)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func noJWKS(context.Context, string) ([]byte, error) { return nil, errors.New("no JWKS in this test") }

func staticResolver(keys ...string) *KeyResolver {
	return NewKeyResolver("https://auth.example.com", keys, noJWKS, func() time.Time { return time.Unix(1000, 0) })
}

func jwksFor(t *testing.T, publicKeysB64 ...string) []byte {
	t.Helper()
	keys := []map[string]string{}
	for _, encoded := range publicKeysB64 {
		raw, _ := base64.StdEncoding.DecodeString(encoded)
		keys = append(keys, map[string]string{
			"kty": "OKP", "crv": "Ed25519", "kid": kidForRaw(raw), "x": base64.RawURLEncoding.EncodeToString(raw),
		})
	}
	return mustJSON(t, map[string]any{"keys": keys})
}

func liveIDToken(t *testing.T, s signer, issuer string, overrides map[string]any) string {
	now := time.Now().Unix()
	claims := map[string]any{"iss": issuer, "iat": now, "exp": now + 300}
	for k, v := range overrides {
		claims[k] = v
	}
	return s.idToken(t, nil, claims)
}

func TestCodeExchangeUsesBasicPKCEAndValidatesIdentity(t *testing.T) {
	issuer := newFakeIssuer(t)
	key := newSigner(t)
	issuer.answer(map[string]any{
		"sub": "account-123", "email": "Alice@Example.com", "nonce": "expected-nonce",
		"id_token": liveIDToken(t, key, issuer.server.URL, nil),
	})
	principal, err := issuer.client(t).Exchange(context.Background(), "single-use-code", "pkce-verifier", "expected-nonce", staticResolver(key.pubB64()))
	if err != nil || principal.Email != "alice@example.com" {
		t.Fatalf("exchange: %+v %v", principal, err)
	}
	req, form := issuer.lastRequest, issuer.lastForm
	if req.Method != http.MethodPost || req.URL.Path != "/token" || !strings.HasPrefix(req.Header.Get("Authorization"), "Basic ") {
		t.Fatalf("request %s %s %q", req.Method, req.URL, req.Header.Get("Authorization"))
	}
	user, pass, _ := req.BasicAuth()
	if user != "lens" || pass != "a-random-client-secret-with-32-bytes" {
		t.Fatal("basic credentials")
	}
	want := url.Values{
		"grant_type": {"authorization_code"}, "code": {"single-use-code"},
		"redirect_uri": {"https://lens.example.com/auth/callback"}, "client_id": {"lens"}, "code_verifier": {"pkce-verifier"},
	}
	if form.Encode() != want.Encode() {
		t.Fatalf("form %v", form)
	}
	if req.Header.Get("Cache-Control") != "no-store" || req.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
		t.Fatal("headers")
	}
}

func TestHTTP400FromTokenEndpointIsARejectedCode(t *testing.T) {
	issuer := newFakeIssuer(t)
	issuer.tokenStatus = http.StatusBadRequest
	_, err := issuer.client(t).Exchange(context.Background(), "c", "v", "n", staticResolver())
	if !errors.Is(err, ErrCodeExchangeRejected) {
		t.Fatalf("400: %v", err)
	}
	for _, status := range []int{http.StatusInternalServerError, http.StatusFound, http.StatusUnauthorized} {
		issuer.tokenStatus = status
		_, err := issuer.client(t).Exchange(context.Background(), "c", "v", "n", staticResolver())
		if !isCentral(err) || errors.Is(err, ErrCodeExchangeRejected) {
			t.Errorf("%d: %v", status, err)
		}
	}
}

func TestCodeExchangeRefusesOversizedResponsesAndIsNotRedirected(t *testing.T) {
	issuer := newFakeIssuer(t)
	issuer.tokenBody = func(url.Values) []byte { return bytes.Repeat([]byte(" "), MaxTokenResponseBytes+1) }
	if _, err := issuer.client(t).Exchange(context.Background(), "c", "v", "n", staticResolver()); !isCentral(err) {
		t.Fatalf("oversized: %v", err)
	}

	elsewhere := newFakeIssuer(t)
	redirector := httptest.NewServer(http.RedirectHandler(elsewhere.server.URL+"/token", http.StatusTemporaryRedirect))
	defer redirector.Close()
	client, _ := NewClient(redirector.URL, "https://lens.example.com", "lens", "a-random-client-secret-with-32-bytes", true)
	if _, err := client.Exchange(context.Background(), "c", "v", "n", staticResolver()); !isCentral(err) {
		t.Fatalf("redirect: %v", err)
	}
	if elsewhere.lastRequest != nil {
		t.Fatal("the redirect was followed with the client secret")
	}
}

func TestLoginPrunesRevokedAndAbsolutelyExpiredSessions(t *testing.T) {
	ctx := context.Background()
	store, clock := openTestStore(t, 60, 120)
	_ = store.GrantAccess(ctx, "alice@example.com")
	revoked, _ := store.CreateSession(ctx, "account-1", "alice@example.com")
	clock.set(1001)
	if ok, _ := store.RevokeSession(ctx, revoked); !ok {
		t.Fatal("revoke")
	}
	clock.set(1002)
	_, _ = store.CreateSession(ctx, "account-1", "alice@example.com") // absolute expiry 1122
	clock.set(1100)
	live, _ := store.CreateSession(ctx, "account-1", "alice@example.com")
	clock.set(1130)
	latest, _ := store.CreateSession(ctx, "account-1", "alice@example.com")

	rows, err := rawDB(t, store.Path()).Query("SELECT token_hash FROM sessions")
	if err != nil {
		t.Fatal(err)
	}
	remaining := map[string]bool{}
	for rows.Next() {
		var hash string
		_ = rows.Scan(&hash)
		remaining[hash] = true
	}
	_ = rows.Close()
	liveHash, _ := tokenHash(live)
	latestHash, _ := tokenHash(latest)
	if len(remaining) != 2 || !remaining[liveHash] || !remaining[latestHash] {
		t.Fatalf("remaining %v", remaining)
	}
	clock.set(1131)
	for _, token := range []string{live, latest} {
		if id, _ := store.GetIdentity(ctx, token); id == nil {
			t.Fatal("live session lost")
		}
	}
}

func TestReplayTableIsPrunedAfterRetention(t *testing.T) {
	ctx := context.Background()
	store, clock := openTestStore(t, DefaultIdleSeconds, DefaultAbsoluteSeconds)
	week := int64(7 * 24 * 60 * 60)
	if ok, _ := store.ConsumeLogoutEvent(ctx, LogoutEvent{"old", "a", "https://auth.example.com", 1000}); !ok {
		t.Fatal("old")
	}
	clock.set(1001 + week)
	if ok, _ := store.ConsumeLogoutEvent(ctx, LogoutEvent{"new", "b", "https://auth.example.com", 1000 + week}); !ok {
		t.Fatal("new")
	}
	var ids []string
	rows, _ := rawDB(t, store.Path()).Query("SELECT event_id FROM revocation_events")
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		ids = append(ids, id)
	}
	_ = rows.Close()
	if strings.Join(ids, ",") != "new" {
		t.Fatalf("ids %v", ids)
	}
}

func TestSafeLocalPathBoundsLengthAndControlCharacters(t *testing.T) {
	cases := map[string]string{
		"/jobs?x=1":                     "/jobs?x=1",
		"/" + strings.Repeat("a", 2048): "/",
		"/jobs\r\nSet-Cookie: x":        "/",
		"//evil.example":                "/",
		"https://evil.example/":         "/",
		"jobs":                          "/",
		"/\\evil":                       "/",
		"/a\x7fb":                       "/",
		"/":                             "/",
	}
	for in, want := range cases {
		if got := SafeLocalPath(in); got != want {
			t.Errorf("SafeLocalPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func centralEnv(t *testing.T, overrides map[string]string) map[string]string {
	env := map[string]string{
		"LENS_AUTH_MODE":      "central",
		"LENS_SESSION_SECRET": "test-session-secret-with-at-least-32-bytes",
		"LENS_ACCESS_DB":      filepath.Join(t.TempDir(), "access.db"),
		"AUTH_ISSUER_URL":     "https://auth.example.com",
		"LENS_PUBLIC_URL":     "https://lens.example.com",
		"AUTH_CLIENT_SECRET":  "a-test-secret-that-is-at-least-32-bytes",
		"AUTH_SIGNING_PUBKEY": newSigner(t).pubB64(),
	}
	for k, v := range overrides {
		if v == "\x00unset" {
			delete(env, k)
		} else {
			env[k] = v
		}
	}
	return env
}

func TestCentralModeConfigurationIsValidatedAtConstruction(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name      string
		overrides map[string]string
		want      string
	}{
		{"no pubkey", map[string]string{"AUTH_SIGNING_PUBKEY": ""}, "AUTH_SIGNING_PUBKEY is required"},
		{"bad pubkey", map[string]string{"AUTH_SIGNING_PUBKEY": "not base64!"}, "AUTH_SIGNING_PUBKEY is not valid base64"},
		{"short pubkey", map[string]string{"AUTH_SIGNING_PUBKEY": "c2hvcnQ="}, "32-byte"},
		{"bad previous", map[string]string{"AUTH_SIGNING_PREVIOUS_PUBKEYS": "c2hvcnQ="}, "32-byte"},
		{"short secret", map[string]string{"LENS_SESSION_SECRET": "short"}, "LENS_SESSION_SECRET"},
		{"insecure cookie", map[string]string{"LENS_AUTH_COOKIE_SECURE": "0"}, "Secure Lens cookie"},
		{"http issuer", map[string]string{"AUTH_ISSUER_URL": "http://auth.example.com"}, "AUTH_ISSUER_URL must be an HTTPS origin"},
		{"issuer path", map[string]string{"AUTH_ISSUER_URL": "https://auth.example.com/x"}, "AUTH_ISSUER_URL"},
		{"public query", map[string]string{"LENS_PUBLIC_URL": "https://lens.example.com/?a=b"}, "LENS_PUBLIC_URL"},
		{"user info", map[string]string{"LENS_PUBLIC_URL": "https://u:p@lens.example.com"}, "LENS_PUBLIC_URL"},
		{"client id colon", map[string]string{"AUTH_CLIENT_ID": "a:b"}, "AUTH_CLIENT_ID"},
		{"short client secret", map[string]string{"AUTH_CLIENT_SECRET": "x"}, "AUTH_CLIENT_SECRET"},
		{"bad timeouts", map[string]string{"LENS_SESSION_IDLE_SECONDS": "100", "LENS_SESSION_ABSOLUTE_SECONDS": "50"}, "Invalid session timeouts"},
		{"bad mode", map[string]string{"LENS_AUTH_MODE": "magic"}, "LENS_AUTH_MODE must be"},
	}
	for _, tc := range cases {
		_, err := NewProviderFromEnv(ctx, envMap(centralEnv(t, tc.overrides)))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
	}

	env := centralEnv(t, map[string]string{"LENS_AUTH_COOKIE_SECURE": "0", "AUTH_ALLOW_INSECURE_HTTP": "1", "AUTH_ISSUER_URL": "http://auth.example.com/"})
	provider, err := NewProviderFromEnv(ctx, envMap(env))
	if err != nil {
		t.Fatal(err)
	}
	central := provider.(*CentralProvider)
	t.Cleanup(func() { _ = central.Store.Close() })
	if central.CookieName != "lens_session" || central.Client.IssuerURL != "http://auth.example.com" || central.Client.ClientID != "lens" {
		t.Fatalf("provider %+v", central)
	}
	secure, err := NewProviderFromEnv(ctx, envMap(centralEnv(t, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = secure.(*CentralProvider).Store.Close() })
	if secure.(*CentralProvider).CookieName != "__Host-lens_session" || !secure.(*CentralProvider).Transactions.Secure {
		t.Fatal("secure cookie names")
	}
}

func TestLegacyModeIsTheDefault(t *testing.T) {
	provider, err := NewProviderFromEnv(context.Background(), envMap(map[string]string{}))
	if err != nil || provider.Mode() != ModeElcano {
		t.Fatalf("%v %v", provider, err)
	}
	if !CookieSecure(envMap(map[string]string{})) || CookieSecure(envMap(map[string]string{"LENS_AUTH_COOKIE_SECURE": "0"})) {
		t.Fatal("cookie secure default")
	}
}

func TestProviderCacheRebuildsOnlyWhenTheEnvironmentChanges(t *testing.T) {
	ctx := context.Background()
	env := centralEnv(t, nil)
	cache := NewProviderCache(func(name string) (string, bool) { v, ok := env[name]; return v, ok })
	first, err := cache.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := cache.Get(ctx)
	if first != again {
		t.Fatal("rebuilt without a change")
	}
	env["LENS_AUTH_MODE"] = "elcano"
	switched, _ := cache.Get(ctx)
	if switched.Mode() != ModeElcano {
		t.Fatal("env change not picked up")
	}
	env["LENS_AUTH_MODE"] = "nope"
	if _, err := cache.Get(ctx); err == nil {
		t.Fatal("a broken environment must keep failing")
	}
	_ = first.(*CentralProvider).Store.Close()
}

func TestSessionTouchIsRateLimitedToOneWritePerMinute(t *testing.T) {
	ctx := context.Background()
	store, clock := openTestStore(t, DefaultIdleSeconds, DefaultAbsoluteSeconds)
	_ = store.GrantAccess(ctx, "alice@example.com")
	issued, _ := store.CreateSession(ctx, "account-123", "alice@example.com")
	hash, _ := tokenHash(issued)
	db := rawDB(t, store.Path())
	stamps := func() [2]int64 {
		var out [2]int64
		if err := db.QueryRow("SELECT last_seen_at, idle_expires_at FROM sessions WHERE token_hash = ?", hash).Scan(&out[0], &out[1]); err != nil {
			t.Fatal(err)
		}
		return out
	}
	idle := store.IdleSeconds()
	if stamps() != [2]int64{1000, 1000 + idle} {
		t.Fatalf("initial %v", stamps())
	}
	for _, at := range []int64{1030, 1059} {
		clock.set(at)
		if id, _ := store.GetIdentity(ctx, issued); id == nil {
			t.Fatal("lost")
		}
	}
	if stamps() != [2]int64{1000, 1000 + idle} {
		t.Fatalf("touched early %v", stamps())
	}
	clock.set(1060)
	if id, _ := store.GetIdentity(ctx, issued); id == nil {
		t.Fatal("lost")
	}
	if stamps() != [2]int64{1060, 1060 + idle} {
		t.Fatalf("not touched %v", stamps())
	}
	clock.set(1060 + idle)
	if id, _ := store.GetIdentity(ctx, issued); id != nil {
		t.Fatal("idle session survived")
	}
}

type jwksStub struct {
	mu    sync.Mutex
	keys  []string
	fail  bool
	calls int
	t     *testing.T
}

func (j *jwksStub) fetch(_ context.Context, url string) ([]byte, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.calls++
	if url != "https://auth.example.com/jwks.json" {
		j.t.Errorf("fetched %s", url)
	}
	if j.fail {
		return nil, errors.New("auth unreachable")
	}
	return jwksFor(j.t, j.keys...), nil
}

func TestKeyResolverMergesStaticAndPublishedKeysAndSurvivesFetchFailure(t *testing.T) {
	ctx := context.Background()
	oldKey, newKey := newSigner(t), newSigner(t)
	clock := &fakeClock{now: 1000}
	stub := &jwksStub{t: t, keys: []string{newKey.pubB64()}}
	resolver := NewKeyResolver("https://auth.example.com/", []string{oldKey.pubB64()}, stub.fetch, clock.time)
	want := oldKey.pubB64() + "," + newKey.pubB64()
	if got := strings.Join(resolver.PublicKeys(ctx), ","); got != want || stub.calls != 1 {
		t.Fatalf("first %s calls=%d", got, stub.calls)
	}
	if got := strings.Join(resolver.PublicKeys(ctx), ","); got != want || stub.calls != 1 {
		t.Fatalf("cached %s calls=%d", got, stub.calls)
	}
	clock.add(11 * 60)
	stub.fail = true
	if got := strings.Join(resolver.PublicKeys(ctx), ","); got != want || stub.calls != 2 {
		t.Fatalf("after failure %s calls=%d", got, stub.calls)
	}

	rotated := newSigner(t)
	raw := rotated.logoutToken(t, nil)
	stub.fail, stub.keys = false, []string{newKey.pubB64(), rotated.pubB64()}
	clock.add(61)
	if !strings.Contains(strings.Join(resolver.KeysForToken(ctx, raw), ","), rotated.pubB64()) {
		t.Fatal("rotated key not fetched")
	}
	fetchedAfter := stub.calls
	resolver.KeysForToken(ctx, newSigner(t).logoutToken(t, nil))
	if stub.calls != fetchedAfter {
		t.Fatal("an unknown kid refreshed again within a minute")
	}
}

func TestLogoutTokenVerifiesAgainstAKeyOnlyPublishedInJWKS(t *testing.T) {
	key := newSigner(t)
	raw := key.logoutToken(t, nil)
	stub := &jwksStub{t: t, keys: []string{key.pubB64()}}
	resolver := NewKeyResolver("https://auth.example.com", nil, stub.fetch, func() time.Time { return time.Unix(1000, 0) })
	event, err := VerifyLogoutToken(raw, "https://auth.example.com", "lens", resolver.KeysForToken(context.Background(), raw), i64(1010))
	if err != nil || event.Subject != "account-123" {
		t.Fatalf("%+v %v", event, err)
	}
}

func TestIDTokenIdentityComesFromTheSignedToken(t *testing.T) {
	key := newSigner(t)
	principal, err := VerifyIDToken(key.idToken(t, nil, nil), "https://auth.example.com/", "lens", "expected-nonce", []string{key.pubB64()}, i64(1010))
	if err != nil || principal.Subject != "account-123" || principal.Email != "alice@example.com" {
		t.Fatalf("%+v %v", principal, err)
	}
}

func TestIDTokenRefusesAnythingButThisLoginsAuthAssertion(t *testing.T) {
	key := newSigner(t)
	cases := []struct {
		header, claims map[string]any
	}{
		{map[string]any{"typ": "logout+jwt"}, nil},
		{map[string]any{"typ": nil}, nil},
		{map[string]any{"alg": "none"}, nil},
		{map[string]any{"alg": "HS256"}, nil},
		{map[string]any{"kid": "some-other-key"}, nil},
		{nil, map[string]any{"nonce": "another-login"}},
		{nil, map[string]any{"nonce": nil}},
		{nil, map[string]any{"aud": "explorer"}},
		{nil, map[string]any{"aud": []string{"lens"}}},
		{nil, map[string]any{"iss": "https://evil.example.com"}},
		{nil, map[string]any{"iss": "https://auth.example.com/"}},
		{nil, map[string]any{"iss": nil}},
		{nil, map[string]any{"exp": 900}},
		{nil, map[string]any{"exp": nil}},
		{nil, map[string]any{"exp": true}},
		{nil, map[string]any{"iat": nil}},
		{nil, map[string]any{"iat": 5000}},
		{nil, map[string]any{"sub": ""}},
		{nil, map[string]any{"sub": nil}},
		{nil, map[string]any{"email": "not-an-email"}},
		{nil, map[string]any{"email": nil}},
	}
	for _, tc := range cases {
		raw := key.idToken(t, tc.header, tc.claims)
		if _, err := VerifyIDToken(raw, "https://auth.example.com", "lens", "expected-nonce", []string{key.pubB64()}, i64(1010)); !isCentral(err) {
			t.Errorf("header %v claims %v: accepted", tc.header, tc.claims)
		}
	}
}

func TestIDTokenRefusesABadSignatureAndAnUnknownKey(t *testing.T) {
	key := newSigner(t)
	raw := key.idToken(t, nil, nil)
	parts := strings.Split(raw, ".")
	claimsJSON, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]any
	_ = json.Unmarshal(claimsJSON, &claims)
	claims["sub"] = "x"
	tampered := parts[0] + "." + enc(t, claims) + "." + parts[2]
	stranger := newSigner(t).pubB64()
	for _, tc := range []struct {
		token string
		keys  []string
	}{
		{tampered, []string{key.pubB64()}},
		{parts[0] + "." + parts[1] + "." + parts[2][:len(parts[2])-4] + "AAAA", []string{key.pubB64()}},
		{raw, []string{stranger}},
		{raw, nil},
		{parts[0] + "." + parts[1], []string{key.pubB64()}},
		{strings.Repeat("a", 16_385), []string{key.pubB64()}},
		{raw + "=", []string{key.pubB64()}},
	} {
		if _, err := VerifyIDToken(tc.token, "https://auth.example.com", "lens", "expected-nonce", tc.keys, i64(1010)); !isCentral(err) {
			t.Errorf("accepted %.40s", tc.token)
		}
	}
}

func TestCodeExchangeTrustsOnlyTheSignedClaims(t *testing.T) {
	issuer := newFakeIssuer(t)
	key := newSigner(t)
	unsigned := map[string]any{"sub": "account-999", "email": "mallory@example.com", "nonce": "expected-nonce",
		"iss": issuer.server.URL, "aud": "lens", "exp": time.Now().Unix() + 300}
	withToken := map[string]any{"id_token": liveIDToken(t, key, issuer.server.URL, nil)}
	for k, v := range unsigned {
		withToken[k] = v
	}
	issuer.answer(withToken)
	principal, err := issuer.client(t).Exchange(context.Background(), "c", "v", "expected-nonce", staticResolver(key.pubB64()))
	if err != nil || principal.Subject != "account-123" || principal.Email != "alice@example.com" {
		t.Fatalf("%+v %v", principal, err)
	}
	nullToken := map[string]any{"id_token": nil}
	numberToken := map[string]any{"id_token": 7}
	for _, body := range []any{unsigned, nullToken, numberToken, []string{"x"}} {
		issuer.answer(body)
		if _, err := issuer.client(t).Exchange(context.Background(), "c", "v", "expected-nonce", staticResolver(key.pubB64())); !isCentral(err) {
			t.Errorf("body %v accepted", body)
		}
	}
}

func TestCodeExchangeRefusesAnIDTokenSignedByAnotherKey(t *testing.T) {
	issuer := newFakeIssuer(t)
	configured := newSigner(t)
	issuer.answer(map[string]any{"id_token": liveIDToken(t, newSigner(t), issuer.server.URL, nil)})
	if _, err := issuer.client(t).Exchange(context.Background(), "c", "v", "expected-nonce", staticResolver(configured.pubB64())); !isCentral(err) {
		t.Fatalf("forged: %v", err)
	}
}

func TestCodeExchangeAcceptsAKeyOnlyPublishedInJWKS(t *testing.T) {
	issuer := newFakeIssuer(t)
	static, rotated := newSigner(t), newSigner(t)
	issuer.answer(map[string]any{"id_token": liveIDToken(t, rotated, issuer.server.URL, nil)})
	issuer.jwks = func() ([]byte, int) { return jwksFor(t, static.pubB64(), rotated.pubB64()), http.StatusOK }
	client := issuer.client(t)
	resolver := NewKeyResolver(client.IssuerURL, []string{static.pubB64()}, nil, nil)
	principal, err := client.Exchange(context.Background(), "c", "v", "expected-nonce", resolver)
	if err != nil || principal.Subject != "account-123" {
		t.Fatalf("%+v %v", principal, err)
	}
	if issuer.jwksCalls != 1 {
		t.Fatalf("jwks calls %d", issuer.jwksCalls)
	}
}

func TestHTTPJWKSFetcherCapsTheDocument(t *testing.T) {
	issuer := newFakeIssuer(t)
	issuer.jwks = func() ([]byte, int) { return bytes.Repeat([]byte("x"), MaxJWKSBytes+1), http.StatusOK }
	if _, err := HTTPJWKSFetcher(nil)(context.Background(), issuer.server.URL+"/jwks.json"); err == nil {
		t.Fatal("oversized JWKS accepted")
	}
	issuer.jwks = func() ([]byte, int) { return []byte(`{"keys":[]}`), http.StatusInternalServerError }
	if _, err := HTTPJWKSFetcher(nil)(context.Background(), issuer.server.URL+"/jwks.json"); err == nil {
		t.Fatal("HTTP 500 accepted")
	}
}

func resign(t *testing.T, raw string, s signer, header map[string]any) string {
	parts := strings.Split(raw, ".")
	headerJSON, _ := base64.RawURLEncoding.DecodeString(parts[0])
	var h map[string]any
	_ = json.Unmarshal(headerJSON, &h)
	for k, v := range header {
		h[k] = v
	}
	return s.signSegments(enc(t, h), parts[1])
}

func TestAMalformedKidIsAnInvalidTokenNotACrash(t *testing.T) {
	key := newSigner(t)
	keys := []string{key.pubB64()}
	for _, kid := range []any{"é", "kid with spaces", "", 7, nil} {
		header := map[string]any{"kid": kid}
		if _, err := VerifyIDToken(resign(t, key.idToken(t, nil, nil), key, header), "https://auth.example.com", "lens", "expected-nonce", keys, i64(1010)); !isCentral(err) {
			t.Errorf("id token kid %v accepted", kid)
		}
		if _, err := VerifyLogoutToken(resign(t, key.logoutToken(t, nil), key, header), "https://auth.example.com", "lens", keys, i64(1200)); !isCentral(err) {
			t.Errorf("logout kid %v accepted", kid)
		}
		if _, err := VerifyAccessToken(resign(t, key.accessToken(t, "grant", 1, nil), key, header), "https://auth.example.com", "lens", keys, i64(1200)); !isCentral(err) {
			t.Errorf("access kid %v accepted", kid)
		}
	}
}

// Python refused text it could not encode (a lone surrogate from a JSON
// escape); Go would otherwise substitute U+FFFD and carry on.
func TestIDTokenWithUnencodableTextIsRefusedNotACrash(t *testing.T) {
	key := newSigner(t)
	for field, value := range map[string]string{"nonce": `"\ud800"`, "sub": `"account-\ud800"`, "email": `"al\udc00ice@example.com"`} {
		header := enc(t, map[string]any{"typ": "JWT", "alg": "EdDSA", "kid": key.kid()})
		claims := `{"iss":"https://auth.example.com","sub":"account-123","aud":"lens","email":"alice@example.com",` +
			`"iat":1000,"exp":1300,"nonce":"expected-nonce","` + field + `":` + value + `}`
		raw := key.signSegments(header, base64.RawURLEncoding.EncodeToString([]byte(claims)))
		if _, err := VerifyIDToken(raw, "https://auth.example.com", "lens", "expected-nonce", []string{key.pubB64()}, i64(1010)); !isCentral(err) {
			t.Errorf("%s: accepted", field)
		}
	}
	// A valid surrogate pair is fine.
	if hasLoneSurrogateEscape([]byte(`{"a":"😀","b":"\\ud800"}`)) {
		t.Fatal("valid pair / escaped backslash flagged")
	}
}

func TestAnEmptyIDTokenIsRefusedWithoutFetchingKeys(t *testing.T) {
	issuer := newFakeIssuer(t)
	stub := &jwksStub{t: t}
	resolver := NewKeyResolver("https://auth.example.com", nil, stub.fetch, func() time.Time { return time.Unix(1000, 0) })
	issuer.answer(map[string]any{"id_token": ""})
	if _, err := issuer.client(t).Exchange(context.Background(), "c", "v", "expected-nonce", resolver); !isCentral(err) {
		t.Fatal(err)
	}
	if stub.calls != 0 {
		t.Fatal("keys fetched")
	}
}

func rotationFixture(t *testing.T, fetch JWKSFetcher) (*KeyResolver, *fakeClock, *jwksStub, signer, signer) {
	static, remote := newSigner(t), newSigner(t)
	clock := &fakeClock{now: 1000}
	stub := &jwksStub{t: t, keys: []string{static.pubB64(), remote.pubB64()}}
	if fetch == nil {
		fetch = stub.fetch
	}
	return NewKeyResolver("https://auth.example.com", []string{static.pubB64()}, fetch, clock.time), clock, stub, static, remote
}

func TestARetiredJWKSKeyStopsVerifyingOnceTheCacheIsStale(t *testing.T) {
	ctx := context.Background()
	resolver, clock, stub, _, remote := rotationFixture(t, nil)
	token := remote.idToken(t, nil, nil)
	has := func() bool {
		return strings.Contains(strings.Join(resolver.KeysForToken(ctx, token), ","), remote.pubB64())
	}
	if !has() {
		t.Fatal("remote key missing")
	}
	clock.add(5 * 60)
	if !has() {
		t.Fatal("fresh cache is trusted")
	}
	stub.keys = stub.keys[:1] // Auth retires the remote key
	clock.add(6 * 60)         // past the 10-minute cache
	if has() {
		t.Fatal("retired key still trusted")
	}
	if _, err := VerifyIDToken(token, "https://auth.example.com", "lens", "expected-nonce", resolver.KeysForToken(ctx, token), i64(1010)); !isCentral(err) {
		t.Fatal("retired key verified")
	}
}

func TestACachedJWKSKeySurvivesAFailedRefresh(t *testing.T) {
	ctx := context.Background()
	resolver, clock, stub, _, remote := rotationFixture(t, nil)
	token := remote.idToken(t, nil, nil)
	if !strings.Contains(strings.Join(resolver.KeysForToken(ctx, token), ","), remote.pubB64()) {
		t.Fatal("missing")
	}
	stub.fail = true
	clock.add(11 * 60)
	if !strings.Contains(strings.Join(resolver.KeysForToken(ctx, token), ","), remote.pubB64()) {
		t.Fatal("cached key lost on failed refresh")
	}
	if stub.calls != 2 {
		t.Fatalf("a stale remote key prompts a refresh: calls=%d", stub.calls)
	}
}

func TestAStaticKeyNeverWaitsOnJWKS(t *testing.T) {
	ctx := context.Background()
	mustNotFetch := func(context.Context, string) ([]byte, error) {
		t.Error("a static-key token fetched /jwks.json")
		return nil, errors.New("no")
	}
	resolver, clock, _, static, _ := rotationFixture(t, mustNotFetch)
	token := static.idToken(t, nil, nil)
	if !strings.Contains(strings.Join(resolver.KeysForToken(ctx, token), ","), static.pubB64()) {
		t.Fatal("cold cache")
	}
	clock.add(60 * 60)
	if !strings.Contains(strings.Join(resolver.KeysForToken(ctx, token), ","), static.pubB64()) {
		t.Fatal("long-stale cache")
	}
}

func TestNormalizeEmail(t *testing.T) {
	cases := map[string]string{
		"Alice@Example.com":   "alice@example.com",
		"  bob@example.com 　": "bob@example.com",
		"Straße@example.de":   "strasse@example.de", // casefold, not lower
		"josé@example.com":   "josé@example.com",   // NFC composes
		"Ångström@x.se":       "ångström@x.se",
	}
	for in, want := range cases {
		if got, err := NormalizeEmail(in); err != nil || got != want {
			t.Errorf("NormalizeEmail(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "no-at", "a@b@c", "@example.com", "alice@", "a@.example.com", "a@example.com.",
		strings.Repeat("a", 250) + "@b.com", "\xff@example.com"} {
		if _, err := NormalizeEmail(bad); !errors.Is(err, ErrInvalidEmail) {
			t.Errorf("NormalizeEmail(%q) = %v", bad, err)
		}
	}
	for _, bad := range []string{"al​ice@example.com", "a\x01@example.com", "a@exa͸mple.com"} {
		if _, err := NormalizeEmail(bad); !errors.Is(err, ErrEmailControlChars) {
			t.Errorf("NormalizeEmail(%q) = %v", bad, err)
		}
	}
}

func TestCSRFTokenIsDerivedFromTheSession(t *testing.T) {
	// HMAC-SHA256(key=session token, msg="lens-central-csrf-v1"), hex: the
	// value Python computed for "token-1".
	if got := CSRFTokenForSession("token-1"); got != "0eb8054f1bb8711b58f0335bdec0d81ac7a6e554c4fd0a47210aa14de4b45544" {
		t.Fatalf("csrf %q", got)
	}
	if CSRFTokenForSession("") != "" || CSRFTokenForSession("é") != "" {
		t.Fatal("csrf for unusable tokens")
	}
}
