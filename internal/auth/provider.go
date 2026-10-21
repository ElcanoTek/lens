// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Provider modes.
const (
	ModeElcano  = "elcano"
	ModeCentral = "central"
)

// TransactionCookieName is the signed login-transaction cookie (the Python
// release's Starlette session cookie name).
const TransactionCookieName = "lens_ui"

// Identity is a signed-in browser user, whichever mode verified it. Tenant is
// set in elcano mode, Subject in central mode.
type Identity struct {
	Subject string
	Email   string
	Tenant  string
}

// Provider is the selected browser-authentication mode.
type Provider interface {
	// Mode is "elcano" or "central".
	Mode() string
	// Identity is the verified user for this request, or nil.
	Identity(r *http.Request) *Identity
	// UnauthenticatedResponse redirects a signed-out browser to sign-in.
	UnauthenticatedResponse(w http.ResponseWriter, r *http.Request)
}

// ElcanoProvider is the unchanged shared magic-link cookie path.
type ElcanoProvider struct {
	Verifier *CookieVerifier
}

// Mode implements Provider.
func (p *ElcanoProvider) Mode() string { return ModeElcano }

// Identity implements Provider.
func (p *ElcanoProvider) Identity(r *http.Request) *Identity { return p.Verifier.CurrentIdentity(r) }

// UnauthenticatedResponse implements Provider.
func (p *ElcanoProvider) UnauthenticatedResponse(w http.ResponseWriter, r *http.Request) {
	p.Verifier.LoginRedirect(w, r)
}

// LogoutURL is the auth service's logout page.
func (p *ElcanoProvider) LogoutURL() string { return p.Verifier.LoginURL + "/logout" }

// loginTransaction is what rides in the signed lens_ui cookie between
// /auth/login and /auth/callback.
type loginTransaction struct {
	State     string `json:"state"`
	Nonce     string `json:"nonce"`
	Verifier  string `json:"verifier"`
	Next      string `json:"next"`
	CreatedAt int64  `json:"created_at"`
}

// CentralProvider is opt-in central Auth: authorization-code handoff and
// Lens-only sessions in the local access database.
type CentralProvider struct {
	Store        *Store
	Client       *Client
	KeyResolver  *KeyResolver
	CookieSecure bool
	// CookieName is __Host-lens_session when Secure (the prefix pins it to
	// this host, path / and Secure), else lens_session.
	CookieName string
	// Transactions is the signed lens_ui login-transaction cookie.
	Transactions *SignedCookie
	now          func() time.Time
}

// NewCentralProvider assembles a central provider. Static keys plus Auth's
// published JWKS are trusted, so a signing-key rotation on Auth needs no env
// edit here.
func NewCentralProvider(store *Store, client *Client, staticKeys []string, sessionSecret []byte, cookieSecure bool) *CentralProvider {
	name := "lens_session"
	if cookieSecure {
		name = "__Host-lens_session"
	}
	return &CentralProvider{
		Store:        store,
		Client:       client,
		KeyResolver:  NewKeyResolver(client.IssuerURL, staticKeys, HTTPJWKSFetcher(nil), nil),
		CookieSecure: cookieSecure,
		CookieName:   name,
		Transactions: NewSignedCookie(TransactionCookieName, sessionSecret, LoginTransactionSeconds*time.Second, cookieSecure),
		now:          time.Now,
	}
}

// Mode implements Provider.
func (p *CentralProvider) Mode() string { return ModeCentral }

func (p *CentralProvider) sessionToken(r *http.Request) string {
	cookie, err := r.Cookie(p.CookieName)
	if err != nil {
		return ""
	}
	return cookie.Value
}

// Identity implements Provider. A database failure is logged and treated as
// signed out (fail closed).
func (p *CentralProvider) Identity(r *http.Request) *Identity {
	token := p.sessionToken(r)
	if token == "" {
		return nil
	}
	identity, err := p.Store.GetIdentity(r.Context(), token)
	if err != nil {
		logger.Error("central session lookup failed", "error", err)
		return nil
	}
	if identity == nil {
		return nil
	}
	return &Identity{Subject: identity.Subject, Email: identity.Email}
}

// UnauthenticatedResponse implements Provider: /auth/login?next=<this page>.
func (p *CentralProvider) UnauthenticatedResponse(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	if len(target) > 2048 {
		target = "/"
	}
	http.Redirect(w, r, "/auth/login?next="+pyQuote(target, ""), http.StatusSeeOther)
}

func randomURLSafe(n int) string {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		panic("crypto/rand failed: " + err.Error()) // never happens on supported platforms
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// BeginLogin starts a login transaction (state, nonce, PKCE verifier in the
// signed lens_ui cookie) and redirects to Auth's /authorize.
func (p *CentralProvider) BeginLogin(w http.ResponseWriter, r *http.Request, nextPath string) {
	state := randomURLSafe(32)
	nonce := randomURLSafe(32)
	verifier := randomURLSafe(64)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	if err := p.Transactions.Set(w, loginTransaction{
		State: state, Nonce: nonce, Verifier: verifier, Next: SafeLocalPath(nextPath), CreatedAt: p.now().Unix(),
	}); err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, p.Client.AuthorizationURL(state, challenge, nonce), http.StatusSeeOther) //nolint:gosec // G710: the configured Auth origin, not request input
}

// CompleteLogin consumes the login transaction (the lens_ui cookie is cleared
// whatever the outcome), redeems the code and issues a Lens session. It
// returns the session token and the local path to land on. Errors are
// *CentralAuthError of kind KindTransaction, KindAccessDenied,
// KindCodeRejected or KindGeneric, or a storage error.
func (p *CentralProvider) CompleteLogin(ctx context.Context, w http.ResponseWriter, r *http.Request, code, state string) (string, string, error) {
	var tx loginTransaction
	found := p.Transactions.Get(r, &tx)
	if p.Transactions.Present(r) {
		p.Transactions.Clear(w)
	}
	if !found {
		return "", "", newError(KindTransaction, "Missing login transaction", nil)
	}
	if tx.CreatedAt+LoginTransactionSeconds <= p.now().Unix() || tx.State == "" ||
		subtle.ConstantTimeCompare([]byte(tx.State), []byte(state)) != 1 ||
		tx.Nonce == "" || tx.Verifier == "" {
		return "", "", newError(KindTransaction, "Invalid login transaction", nil)
	}
	principal, err := p.Client.Exchange(ctx, code, tx.Verifier, tx.Nonce, p.KeyResolver)
	if err != nil {
		return "", "", err
	}
	token, err := p.Store.CreateSession(ctx, principal.Subject, principal.Email)
	if err != nil {
		return "", "", err
	}
	next := tx.Next
	if next == "" {
		next = "/"
	}
	return token, SafeLocalPath(next), nil
}

// SetSessionCookie sets the Lens session cookie (a browser-session cookie:
// no Max-Age; the server enforces the timeouts).
func (p *CentralProvider) SetSessionCookie(w http.ResponseWriter, token string) {
	//nolint:gosec // G124: Secure follows LENS_AUTH_COOKIE_SECURE, which central mode only lets go with AUTH_ALLOW_INSECURE_HTTP
	http.SetCookie(w, &http.Cookie{
		Name: p.CookieName, Value: token, Path: "/",
		Secure: p.CookieSecure, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
}

// ClearSessionCookie expires the Lens session cookie.
func (p *CentralProvider) ClearSessionCookie(w http.ResponseWriter) {
	//nolint:gosec // G124: see SetSessionCookie
	http.SetCookie(w, &http.Cookie{
		Name: p.CookieName, Value: "", Path: "/", MaxAge: -1, Expires: time.Unix(0, 0),
		Secure: p.CookieSecure, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
}

// CSRFToken is the form token for this request's session ("" when signed
// out).
func (p *CentralProvider) CSRFToken(r *http.Request) string {
	token := p.sessionToken(r)
	if token == "" {
		return ""
	}
	return CSRFTokenForSession(token)
}

// ValidCSRF checks a submitted form token against the session in constant
// time.
func (p *CentralProvider) ValidCSRF(r *http.Request, submitted string) bool {
	token := p.sessionToken(r)
	if token == "" || submitted == "" {
		return false
	}
	expected := CSRFTokenForSession(token)
	return expected != "" && subtle.ConstantTimeCompare([]byte(expected), []byte(submitted)) == 1
}

// LogoutURL is Auth's RP-initiated logout for this client.
func (p *CentralProvider) LogoutURL() string {
	return p.Client.IssuerURL + "/logout?client_id=" + pyQuote(p.Client.ClientID, "")
}

// SafeLocalPath returns raw when it is a same-origin absolute path, else "/".
// The path rides in the signed login-transaction cookie; an unbounded value
// would push that cookie past the browser's 4 KB limit and silently break
// sign-in. Control characters have no place in a redirect target.
func SafeLocalPath(raw string) string {
	if len([]rune(raw)) > 2048 {
		return "/"
	}
	for _, c := range raw {
		if c < 32 || c == 127 {
			return "/"
		}
	}
	if !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") || strings.Contains(raw, "\\") {
		return "/"
	}
	// urlsplit of a string starting with a single "/" never yields a scheme
	// or netloc, but keep the Python's belt-and-braces check.
	if parsed, err := url.Parse(raw); err == nil && (parsed.Scheme != "" || parsed.Host != "") {
		return "/"
	}
	return raw
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// CookieSecure is LENS_AUTH_COOKIE_SECURE (default on): whether Lens cookies
// carry Secure. Turning it off is refused in central mode unless
// AUTH_ALLOW_INSECURE_HTTP is also set, so production stays Secure by
// construction; the flag exists so the login flow can be exercised over
// plain HTTP in development.
func CookieSecure(getenv func(string) (string, bool)) bool {
	v, ok := getenv("LENS_AUTH_COOKIE_SECURE")
	if !ok {
		v = "1"
	}
	return truthy(v)
}

// NewCentralProviderFromEnv validates every central-mode setting and opens
// the access database.
func NewCentralProviderFromEnv(ctx context.Context, getenv func(string) (string, bool)) (*CentralProvider, error) {
	secure := CookieSecure(getenv)
	insecure, _ := getenv("AUTH_ALLOW_INSECURE_HTTP")
	if !secure && !truthy(insecure) {
		return nil, errors.New("Central auth requires a Secure Lens cookie") //nolint:staticcheck // message kept verbatim from the Python release
	}
	secret, _ := getenv("LENS_SESSION_SECRET")
	if len(secret) < 32 {
		return nil, errors.New("LENS_SESSION_SECRET must contain at least 32 bytes")
	}
	client, err := NewClientFromEnv(getenv)
	if err != nil {
		return nil, err
	}
	keys, err := RequireStaticSigningKeys(getenv)
	if err != nil {
		return nil, err
	}
	store, err := OpenStoreFromEnv(ctx, getenv)
	if err != nil {
		return nil, err
	}
	return NewCentralProvider(store, client, keys, []byte(secret), secure), nil
}

// NewProviderFromEnv builds the provider LENS_AUTH_MODE selects ("elcano",
// the default, or "central"). getenv is os.LookupEnv in production.
func NewProviderFromEnv(ctx context.Context, getenv func(string) (string, bool)) (Provider, error) {
	mode, ok := getenv("LENS_AUTH_MODE")
	if !ok {
		mode = ModeElcano
	}
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case ModeElcano:
		return &ElcanoProvider{Verifier: NewCookieVerifierFromEnv(getenv)}, nil
	case ModeCentral:
		return NewCentralProviderFromEnv(ctx, getenv)
	default:
		return nil, errors.New("LENS_AUTH_MODE must be either 'elcano' or 'central'")
	}
}

// fingerprintVars are every setting a provider is built from.
var fingerprintVars = []string{
	"LENS_AUTH_MODE", "LENS_ACCESS_DB", "LENS_AUTH_COOKIE_SECURE", "AUTH_ALLOW_INSECURE_HTTP",
	"AUTH_SIGNING_PUBKEY", "AUTH_SIGNING_PREVIOUS_PUBKEYS", "AUTH_ISSUER_URL", "LENS_PUBLIC_URL",
	"AUTH_CLIENT_ID", "AUTH_CLIENT_SECRET", "LENS_SESSION_SECRET", "LENS_SESSION_IDLE_SECONDS",
	"LENS_SESSION_ABSOLUTE_SECONDS", "AUTH_COOKIE_NAME", "AUTH_LOGIN_URL",
}

// ProviderCache returns one provider per distinct auth environment: built on
// first use, rebuilt when any setting it depends on changes (so a test or an
// in-process env change is picked up), and never cached on failure, so a
// broken configuration keeps failing loudly. Safe for concurrent use.
//
// A long-running server whose environment never changes can equally build
// once at startup with NewProviderFromEnv; the cache costs one env read per
// setting per call.
type ProviderCache struct {
	getenv func(string) (string, bool)

	mu          sync.Mutex
	fingerprint string
	provider    Provider
}

// NewProviderCache builds a cache over getenv (os.LookupEnv in production).
func NewProviderCache(getenv func(string) (string, bool)) *ProviderCache {
	return &ProviderCache{getenv: getenv}
}

func (c *ProviderCache) currentFingerprint() string {
	var b strings.Builder
	for _, name := range fingerprintVars {
		v, ok := c.getenv(name)
		if ok {
			b.WriteByte('1')
		} else {
			b.WriteByte('0')
		}
		b.WriteString(v)
		b.WriteByte(0)
	}
	return b.String()
}

// Get returns the provider for the current environment.
func (c *ProviderCache) Get(ctx context.Context) (Provider, error) {
	fingerprint := c.currentFingerprint()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.provider != nil && c.fingerprint == fingerprint {
		return c.provider, nil
	}
	provider, err := NewProviderFromEnv(ctx, c.getenv)
	if err != nil {
		return nil, err
	}
	// The replaced provider is not closed: requests that already hold it may
	// still be using its database. Env changes are rare (tests, never steady
	// state), so this leaks at most one handle per change.
	c.fingerprint, c.provider = fingerprint, provider
	return provider, nil
}

// Clear forgets the cached provider.
func (c *ProviderCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.provider, c.fingerprint = nil, ""
}

// AccountURL is where the signed-out page sends someone who wants to manage
// (or end) their central session: Auth's account page in central mode, the
// auth service in elcano mode.
func AccountURL(p Provider) string {
	switch prov := p.(type) {
	case *CentralProvider:
		return prov.Client.IssuerURL + "/account"
	case *ElcanoProvider:
		return prov.Verifier.LoginURL
	}
	return ""
}
