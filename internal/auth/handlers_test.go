// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package auth

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// authServer is a fake central Auth whose /token mints id_tokens for the
// nonce of the login that produced each code.
type authServer struct {
	t      *testing.T
	server *httptest.Server

	mu          sync.Mutex
	key         signer
	nonces      map[string]string
	forcedNonce map[string]string // raw JSON for the nonce claim
	forcedKid   map[string]string
	status      int
}

func newAuthServer(t *testing.T, key signer) *authServer {
	a := &authServer{t: t, key: key, nonces: map[string]string{}, forcedNonce: map[string]string{}, forcedKid: map[string]string{}, status: http.StatusOK}
	a.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		switch r.URL.Path {
		case "/jwks.json":
			http.Error(w, "Auth's JWKS is not reachable in this test", http.StatusServiceUnavailable)
		case "/token":
			_ = r.ParseForm()
			if a.status != http.StatusOK {
				w.WriteHeader(a.status)
				return
			}
			code := r.PostForm.Get("code")
			now := time.Now().Unix()
			kid := a.key.kid()
			if forced, ok := a.forcedKid[code]; ok {
				kid = forced
			}
			nonce := string(mustJSON(t, a.nonces[code]))
			if forced, ok := a.forcedNonce[code]; ok {
				nonce = forced
			}
			header := enc(t, map[string]any{"typ": "JWT", "alg": "EdDSA", "kid": kid})
			claims := fmt.Sprintf(`{"iss":%q,"sub":"account-123","aud":"lens","email":"alice@example.com","iat":%d,"exp":%d,"nonce":%s}`,
				a.server.URL, now, now+300, nonce)
			token := a.key.signSegments(header, base64.RawURLEncoding.EncodeToString([]byte(claims)))
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(mustJSON(t, map[string]string{"id_token": token}))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(a.server.Close)
	return a
}

type lensApp struct {
	t        *testing.T
	server   *httptest.Server
	client   *http.Client
	provider Provider
	auth     *authServer
	key      signer
	accounts []string
}

func newLensApp(t *testing.T, mode string) *lensApp {
	t.Helper()
	key := newSigner(t)
	auth := newAuthServer(t, key)
	env := centralEnv(t, map[string]string{
		"LENS_AUTH_MODE": mode, "LENS_AUTH_COOKIE_SECURE": "0", "AUTH_ALLOW_INSECURE_HTTP": "1",
		"AUTH_ISSUER_URL": auth.server.URL, "LENS_PUBLIC_URL": "http://lens.example.com",
		"AUTH_CLIENT_ID": "lens", "AUTH_SIGNING_PUBKEY": key.pubB64(),
	})
	provider, err := NewProviderFromEnv(context.Background(), envMap(env))
	if err != nil {
		t.Fatal(err)
	}
	if central, ok := provider.(*CentralProvider); ok {
		t.Cleanup(func() { _ = central.Store.Close() })
		if err := central.Store.GrantAccess(context.Background(), "alice@example.com"); err != nil {
			t.Fatal(err)
		}
	}
	app := &lensApp{t: t, provider: provider, auth: auth, key: key}
	handlers := &Handlers{
		Provider: func(context.Context) (Provider, error) { return provider, nil },
		RenderSignedOut: func(w http.ResponseWriter, _ *http.Request, accountURL string) {
			app.accounts = append(app.accounts, accountURL)
			_, _ = io.WriteString(w, "You are signed out")
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", handlers.Login)
	mux.HandleFunc("GET /auth/login", handlers.Login)
	mux.HandleFunc("GET /auth/callback", handlers.Callback)
	mux.HandleFunc("/auth/backchannel-logout", handlers.BackchannelLogout)
	mux.HandleFunc("POST /logout", handlers.Logout)
	mux.HandleFunc("GET /signed-out", handlers.SignedOut)
	app.server = httptest.NewServer(mux)
	t.Cleanup(app.server.Close)
	jar, _ := cookiejar.New(nil)
	app.client = &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return app
}

func (a *lensApp) do(req *http.Request) (*http.Response, string) {
	a.t.Helper()
	resp, err := a.client.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func (a *lensApp) get(path string) (*http.Response, string) {
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, a.server.URL+path, nil)
	return a.do(req)
}

func (a *lensApp) postForm(path string, form url.Values) (*http.Response, string) {
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, a.server.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return a.do(req)
}

func (a *lensApp) sessionCookie() string {
	u, _ := url.Parse(a.server.URL)
	for _, c := range a.client.Jar.Cookies(u) {
		if c.Name == "lens_session" {
			return c.Value
		}
	}
	return ""
}

func (a *lensApp) clearCookies() {
	jar, _ := cookiejar.New(nil)
	a.client.Jar = jar
}

// signIn runs /auth/login then /auth/callback for code.
func (a *lensApp) signIn(code string) (*http.Response, string, *http.Response) {
	start, _ := a.get("/auth/login")
	if start.StatusCode != http.StatusSeeOther {
		a.t.Fatalf("login status %d", start.StatusCode)
	}
	location, _ := url.Parse(start.Header.Get("Location"))
	query := location.Query()
	a.auth.mu.Lock()
	a.auth.nonces[code] = query.Get("nonce")
	a.auth.mu.Unlock()
	resp, body := a.get("/auth/callback?" + url.Values{"code": {code}, "state": {query.Get("state")}}.Encode())
	return resp, body, start
}

func (a *lensApp) central() *CentralProvider { return a.provider.(*CentralProvider) }

func (a *lensApp) identity(token string) *CentralIdentity {
	id, err := a.central().Store.GetIdentity(context.Background(), token)
	if err != nil {
		a.t.Fatal(err)
	}
	return id
}

func TestCentralLoginCreatesLensOnlySessionAndBackchannelRevokes(t *testing.T) {
	app := newLensApp(t, "central")
	callback, _, start := app.signIn("one-time-code")

	transaction := strings.ToLower(start.Header.Get("Set-Cookie"))
	for _, want := range []string{"lens_ui=", "httponly", "samesite=lax", "max-age=600", "path=/"} {
		if !strings.Contains(transaction, want) {
			t.Errorf("transaction cookie %q lacks %q", transaction, want)
		}
	}
	if strings.Contains(transaction, "secure") {
		t.Error("Secure set with LENS_AUTH_COOKIE_SECURE=0")
	}
	if start.Header.Get("Cache-Control") != "no-store" {
		t.Error("login redirect is cacheable")
	}
	location, _ := url.Parse(start.Header.Get("Location"))
	q := location.Query()
	if location.Path != "/authorize" || q.Get("client_id") != "lens" || q.Get("code_challenge_method") != "S256" ||
		q.Get("redirect_uri") != "http://lens.example.com/auth/callback" || q.Get("scope") != "openid email" {
		t.Errorf("authorize URL %s", location)
	}

	if callback.StatusCode != http.StatusSeeOther || callback.Header.Get("Location") != "/" ||
		callback.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("callback %d %v", callback.StatusCode, callback.Header)
	}
	session := app.sessionCookie()
	if session == "" || app.identity(session) == nil {
		t.Fatal("no session")
	}

	logout, _ := app.postForm("/auth/backchannel-logout", url.Values{"logout_token": {app.key.mint(t,
		map[string]any{"typ": "logout+jwt", "alg": "EdDSA", "kid": app.key.kid()},
		map[string]any{"iss": app.auth.server.URL, "sub": "account-123", "aud": "lens", "iat": 1000, "exp": 2_000_000_000,
			"jti": "event-123", "events": map[string]any{BackchannelLogoutEvent: map[string]any{}}}, nil, nil)}})
	if logout.StatusCode != http.StatusNoContent {
		t.Fatalf("backchannel %d", logout.StatusCode)
	}
	if app.identity(session) != nil {
		t.Fatal("session survived back-channel logout")
	}

	oversized, _ := app.postForm("/auth/backchannel-logout", url.Values{"logout_token": {strings.Repeat("x", 20_001)}})
	if oversized.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized %d", oversized.StatusCode)
	}

	// A consumed or superseded code is a retry for the user, not an outage.
	app.auth.mu.Lock()
	app.auth.status = http.StatusBadRequest
	app.auth.mu.Unlock()
	rejected, body, _ := app.signIn("one-time-code")
	if rejected.StatusCode != http.StatusBadRequest || !strings.Contains(body, "Try again") {
		t.Fatalf("rejected code %d %s", rejected.StatusCode, body)
	}
}

func TestCallbackRequiresALiveMatchingTransaction(t *testing.T) {
	app := newLensApp(t, "central")
	if resp, body := app.get("/auth/callback?code=c&state=s"); resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "Try again") {
		t.Fatalf("no transaction %d %s", resp.StatusCode, body)
	}
	if resp, body := app.get("/auth/callback?error=access_denied"); resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "not completed") {
		t.Fatalf("error param %d %s", resp.StatusCode, body)
	}
	start, _ := app.get("/auth/login?next=%2Fjobs%3Fx%3D1")
	location, _ := url.Parse(start.Header.Get("Location"))
	if resp, _ := app.get("/auth/callback?code=c&state=wrong"); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("wrong state %d", resp.StatusCode)
	}
	// The transaction was consumed by the failed attempt.
	state := location.Query().Get("state")
	if resp, _ := app.get("/auth/callback?code=c&state=" + url.QueryEscape(state)); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("reused transaction %d", resp.StatusCode)
	}

	// ?next= survives the round trip.
	start, _ = app.get("/auth/login?next=%2Fjobs%3Fx%3D1")
	location, _ = url.Parse(start.Header.Get("Location"))
	app.auth.mu.Lock()
	app.auth.nonces["code-next"] = location.Query().Get("nonce")
	app.auth.mu.Unlock()
	resp, _ := app.get("/auth/callback?code=code-next&state=" + url.QueryEscape(location.Query().Get("state")))
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/jobs?x=1" {
		t.Fatalf("next %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	// Signed in: /auth/login goes home.
	if resp, _ := app.get("/auth/login"); resp.Header.Get("Location") != "/" {
		t.Fatalf("signed-in login %q", resp.Header.Get("Location"))
	}
}

func TestExpiredTransactionIsRefused(t *testing.T) {
	app := newLensApp(t, "central")
	central := app.central()
	clock := &fakeClock{now: time.Now().Unix()}
	central.now = clock.time
	central.Transactions.now = clock.time
	start, _ := app.get("/auth/login")
	location, _ := url.Parse(start.Header.Get("Location"))
	clock.add(LoginTransactionSeconds)
	resp, _ := app.get("/auth/callback?code=c&state=" + url.QueryEscape(location.Query().Get("state")))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expired transaction %d", resp.StatusCode)
	}
}

func TestAccountWithoutAccessIs403(t *testing.T) {
	app := newLensApp(t, "central")
	if _, err := app.central().Store.RevokeAccess(context.Background(), "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	resp, body, _ := app.signIn("code-1")
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "does not have Lens access") || app.sessionCookie() != "" {
		t.Fatalf("denied %d %s", resp.StatusCode, body)
	}
}

func TestCallbackSignsInOnlyWithAnIDTokenSignedByAuth(t *testing.T) {
	app := newLensApp(t, "central")
	signedIn, _, _ := app.signIn("code-1")
	if signedIn.StatusCode != http.StatusSeeOther {
		t.Fatalf("sign in %d", signedIn.StatusCode)
	}
	if id := app.identity(app.sessionCookie()); id == nil || id.Email != "alice@example.com" {
		t.Fatalf("identity %+v", id)
	}

	app.clearCookies()
	app.auth.mu.Lock()
	app.auth.key = newSigner(t)
	app.auth.mu.Unlock()
	forged, body, _ := app.signIn("code-2")
	if forged.StatusCode != http.StatusBadGateway || app.sessionCookie() != "" || !strings.Contains(body, "unavailable") {
		t.Fatalf("forged %d %s", forged.StatusCode, body)
	}

	// Malformed values Auth would never sign are a failed sign-in (502),
	// never a server error, and leave no session.
	app.auth.mu.Lock()
	app.auth.key = app.key
	app.auth.forcedNonce["code-3"] = `"\ud800"`
	app.auth.forcedKid["code-4"] = "é"
	app.auth.mu.Unlock()
	for _, code := range []string{"code-3", "code-4"} {
		refused, _, _ := app.signIn(code)
		if refused.StatusCode != http.StatusBadGateway || app.sessionCookie() != "" {
			t.Fatalf("%s: %d", code, refused.StatusCode)
		}
	}

	// The back-channel answers a non-ASCII kid with 400, not 500.
	badKid := app.key.mint(t, map[string]any{"typ": "logout+jwt", "alg": "EdDSA", "kid": "é"},
		map[string]any{"iss": app.auth.server.URL, "sub": "account-123", "aud": "lens", "iat": 1000, "exp": 2_000_000_000,
			"jti": "event-123", "events": map[string]any{BackchannelLogoutEvent: map[string]any{}}}, nil, nil)
	if resp, _ := app.postForm("/auth/backchannel-logout", url.Values{"logout_token": {badKid}}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad kid %d", resp.StatusCode)
	}
}

func TestBackchannelRequestShape(t *testing.T) {
	app := newLensApp(t, "central")
	access := app.key.mint(t, map[string]any{"typ": "access+jwt", "alg": "EdDSA", "kid": app.key.kid()},
		map[string]any{"iss": app.auth.server.URL, "sub": "account-9", "aud": "lens", "email": "carol@example.com",
			"iat": time.Now().Unix(), "exp": time.Now().Unix() + 300, "jti": "access-1",
			"events": map[string]any{ApplicationAccessEvent: map[string]any{"action": "grant", "version": 1}}}, nil, nil)
	if resp, _ := app.postForm("/auth/backchannel-logout", url.Values{"access_token": {access}}); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("provisioning %d", resp.StatusCode)
	}
	if ok, _ := app.central().Store.IsAllowed(context.Background(), "carol@example.com"); !ok {
		t.Fatal("provisioning not applied")
	}
	for name, form := range map[string]url.Values{
		"neither":    {},
		"both":       {"access_token": {access}, "logout_token": {"x"}},
		"bad logout": {"logout_token": {"x.y.z"}},
		"bad access": {"access_token": {"x.y.z"}},
	} {
		if resp, _ := app.postForm("/auth/backchannel-logout", form); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %d", name, resp.StatusCode)
		}
	}

	// Content-Length rules, checked on a recorder so the header is exactly
	// what the test says.
	handlers := &Handlers{Provider: func(context.Context) (Provider, error) { return app.provider, nil }}
	for _, tc := range []struct {
		length string
		want   int
	}{{"", http.StatusLengthRequired}, {"12a", http.StatusBadRequest}, {"20001", http.StatusRequestEntityTooLarge}, {"99999999999999999999", http.StatusRequestEntityTooLarge}} {
		req := httptest.NewRequest(http.MethodPost, "/auth/backchannel-logout", strings.NewReader("logout_token=x"))
		req.Header.Del("Content-Length")
		if tc.length != "" {
			req.Header.Set("Content-Length", tc.length)
		}
		rec := httptest.NewRecorder()
		handlers.BackchannelLogout(rec, req)
		if rec.Code != tc.want {
			t.Errorf("Content-Length %q: %d, want %d", tc.length, rec.Code, tc.want)
		}
	}
}

func TestCentralLogoutSignsOutEverywhereViaAuth(t *testing.T) {
	app := newLensApp(t, "central")
	token, err := app.central().Store.CreateSession(context.Background(), "account-123", "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(app.server.URL)
	app.client.Jar.SetCookies(u, []*http.Cookie{{Name: "lens_session", Value: token, Path: "/"}})

	if resp, _ := app.postForm("/logout", url.Values{"csrf_token": {"wrong"}}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("bad csrf %d", resp.StatusCode)
	}
	if app.identity(token) == nil {
		t.Fatal("a forged logout ended the session")
	}

	resp, _ := app.postForm("/logout", url.Values{"csrf_token": {CSRFTokenForSession(token)}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != app.auth.server.URL+"/logout?client_id=lens" {
		t.Fatalf("logout %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if app.identity(token) != nil {
		t.Fatal("session survived logout")
	}
	if app.sessionCookie() != "" {
		t.Fatal("cookie not cleared")
	}

	signedOut, body := app.get("/signed-out")
	if signedOut.StatusCode != http.StatusOK || !strings.Contains(body, "You are signed out") || signedOut.Header.Get("Location") != "" {
		t.Fatalf("signed out %d %s", signedOut.StatusCode, body)
	}
	if len(app.accounts) != 1 || app.accounts[0] != app.auth.server.URL+"/account" {
		t.Fatalf("account URL %v", app.accounts)
	}
}

func TestLegacyLogoutStillUsesTheElcanoAuthLogout(t *testing.T) {
	app := newLensApp(t, "elcano")
	resp, _ := app.postForm("/logout", nil)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "https://auth.elcanotek.com/logout" {
		t.Fatalf("legacy logout %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp, _ := app.get("/auth/callback?code=c&state=s"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("callback in elcano mode %d", resp.StatusCode)
	}
	if resp, _ := app.postForm("/auth/backchannel-logout", url.Values{"logout_token": {"x"}}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("backchannel in elcano mode %d", resp.StatusCode)
	}
	login, _ := app.get("/login")
	if login.StatusCode != http.StatusSeeOther || !strings.HasPrefix(login.Header.Get("Location"), "https://auth.elcanotek.com/?return_to=http%3A%2F%2F127.0.0.1") {
		t.Fatalf("legacy login %q", login.Header.Get("Location"))
	}
	app.get("/signed-out")
	if len(app.accounts) != 1 || app.accounts[0] != "https://auth.elcanotek.com" {
		t.Fatalf("account URL %v", app.accounts)
	}
}

func TestUnauthenticatedResponseCarriesTheBoundedTarget(t *testing.T) {
	app := newLensApp(t, "central")
	for path, want := range map[string]string{
		"/jobs?x=1":                     "/auth/login?next=%2Fjobs%3Fx%3D1",
		"/" + strings.Repeat("a", 2100): "/auth/login?next=%2F",
	} {
		rec := httptest.NewRecorder()
		app.provider.UnauthenticatedResponse(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != want {
			t.Errorf("%.20s: %d %q", path, rec.Code, rec.Header().Get("Location"))
		}
	}
}

func TestSignedCookieRejectsTamperingAndAge(t *testing.T) {
	cookie := NewSignedCookie("lens_ui", []byte("test-session-secret-with-at-least-32-bytes"), time.Minute, true)
	clock := &fakeClock{now: 1000}
	cookie.now = clock.time
	rec := httptest.NewRecorder()
	if err := cookie.Set(rec, map[string]string{"a": "b"}); err != nil {
		t.Fatal(err)
	}
	set := rec.Result().Cookies()[0]
	if !set.Secure || !set.HttpOnly || set.SameSite != http.SameSiteLaxMode || set.MaxAge != 60 {
		t.Fatalf("attributes %+v", set)
	}
	read := func(value string) bool {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.AddCookie(&http.Cookie{Name: "lens_ui", Value: value})
		var out map[string]string
		return cookie.Get(req, &out) && out["a"] == "b"
	}
	if !read(set.Value) {
		t.Fatal("round trip")
	}
	if read(set.Value[:len(set.Value)-2] + "AA") {
		t.Fatal("tampered MAC accepted")
	}
	other := NewSignedCookie("lens_ui", []byte("another-session-secret-with-32-bytes!!"), time.Minute, true)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: "lens_ui", Value: set.Value})
	var out map[string]string
	if other.Get(req, &out) {
		t.Fatal("another secret verified the cookie")
	}
	clock.add(61)
	if read(set.Value) {
		t.Fatal("expired cookie accepted")
	}
}

func TestShlexSplit(t *testing.T) {
	cases := map[string][]string{
		`"/var/lib/lens/access.db"`:   {"/var/lib/lens/access.db"},
		`'/a b'`:                      {"/a b"},
		`/plain # comment`:            {"/plain"},
		`a#b`:                         {"a"},
		`"x\"y\z"`:                    {`x"y\z`},
		`a\ b`:                        {"a b"},
		`""`:                          {""},
		``:                            nil,
		`a b`:                         {"a", "b"},
		`"/opt/lens/data/access.db" `: {"/opt/lens/data/access.db"},
	}
	for in, want := range cases {
		got, err := shlexSplit(in)
		if err != nil || strings.Join(got, "\x00") != strings.Join(want, "\x00") || len(got) != len(want) {
			t.Errorf("shlexSplit(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{`"unterminated`, `'x`, `trailing\`} {
		if _, err := shlexSplit(bad); err == nil {
			t.Errorf("shlexSplit(%q) accepted", bad)
		}
	}
}
