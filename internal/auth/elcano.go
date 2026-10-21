// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package auth

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Legacy ("elcano") mode defaults.
const (
	DefaultAuthCookieName = "elcano_auth"
	DefaultAuthLoginURL   = "https://auth.elcanotek.com"
)

// ElcanoSession is a verified elcano_auth cookie.
type ElcanoSession struct {
	Email  string
	Tenant string
	Exp    int64
}

// CookieVerifier verifies the unified Elcano cookie (Pattern B).
//
// Lens owns no login in this mode. It verifies the elcano_auth cookie minted
// by the auth service (auth.elcanotek.com) using that service's Ed25519
// public key (AUTH_SIGNING_PUBKEY) and bounces unauthenticated browsers to
// the auth login page.
//
// Token format mirrors auth/internal/token/token.go and home/server.js
// exactly: base64url(payload_json) + "." + base64url(ed25519_sig), where the
// signature is over the base64url body string. The payload is
// {"email","tenant","iat","exp"}; email and exp are what count.
//
// The public key can only verify, never sign, so it is safe to distribute
// and a leak here cannot forge a session.
type CookieVerifier struct {
	key        ed25519.PublicKey // nil: no (valid) key configured, nothing verifies
	CookieName string
	LoginURL   string
	now        func() time.Time
}

// NewCookieVerifier parses pubkeyB64 (standard base64 of 32 raw bytes). A
// missing or malformed key is not an error: every cookie then fails, which
// looks like a logged-out user (the documented behavior).
func NewCookieVerifier(pubkeyB64, cookieName, loginURL string) *CookieVerifier {
	v := &CookieVerifier{CookieName: cookieName, LoginURL: strings.TrimRight(loginURL, "/"), now: time.Now}
	if src := strings.TrimSpace(pubkeyB64); src != "" {
		if raw, err := base64.StdEncoding.DecodeString(src); err == nil && len(raw) == ed25519.PublicKeySize {
			v.key = ed25519.PublicKey(raw)
		}
	}
	return v
}

// NewCookieVerifierFromEnv reads AUTH_SIGNING_PUBKEY, AUTH_COOKIE_NAME and
// AUTH_LOGIN_URL.
func NewCookieVerifierFromEnv(getenv func(string) (string, bool)) *CookieVerifier {
	get := func(name, fallback string) string {
		if v, ok := getenv(name); ok {
			return v
		}
		return fallback
	}
	return NewCookieVerifier(get("AUTH_SIGNING_PUBKEY", ""), get("AUTH_COOKIE_NAME", DefaultAuthCookieName),
		get("AUTH_LOGIN_URL", DefaultAuthLoginURL))
}

// HasKey reports whether a usable public key is configured.
func (v *CookieVerifier) HasKey() bool { return v.key != nil }

func b64urlLenient(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

// VerifySession returns the session for a valid elcano_auth value. Every
// failure mode (no key configured, malformed, bad signature, expired,
// missing email) returns false — callers treat them all as "logged out".
func (v *CookieVerifier) VerifySession(token string) (ElcanoSession, bool) {
	if v.key == nil || token == "" {
		return ElcanoSession{}, false
	}
	dot := strings.IndexByte(token, '.')
	if dot < 1 || dot == len(token)-1 {
		return ElcanoSession{}, false
	}
	body, sig := token[:dot], token[dot+1:]
	signature, err := b64urlLenient(sig)
	if err != nil || !ed25519.Verify(v.key, []byte(body), signature) {
		return ElcanoSession{}, false
	}
	payloadBytes, err := b64urlLenient(body)
	if err != nil {
		return ElcanoSession{}, false
	}
	payload, ok := decodeJSONObject(payloadBytes)
	if !ok {
		return ElcanoSession{}, false
	}
	email, ok := payload["email"].(string)
	if !ok || email == "" {
		return ElcanoSession{}, false
	}
	// exp may be an integer or a float (Python accepted both).
	expNumber, ok := payload["exp"].(json.Number)
	if !ok {
		return ElcanoSession{}, false
	}
	exp, err := strconv.ParseFloat(string(expNumber), 64)
	now := float64(v.now().UnixNano()) / 1e9
	if err != nil || math.IsInf(exp, 0) || math.IsNaN(exp) || exp <= now {
		return ElcanoSession{}, false
	}
	tenant, _ := payload["tenant"].(string)
	return ElcanoSession{Email: email, Tenant: tenant, Exp: int64(exp)}, true
}

// CurrentIdentity is the verified identity for this request, or nil.
func (v *CookieVerifier) CurrentIdentity(r *http.Request) *Identity {
	cookie, err := r.Cookie(v.CookieName)
	if err != nil {
		return nil
	}
	session, ok := v.VerifySession(cookie.Value)
	if !ok {
		return nil
	}
	return &Identity{Email: session.Email, Tenant: session.Tenant}
}

// LoginRedirect sends the browser to the auth service, which bounces it back
// via ?return_to= after a successful magic-link sign-in.
func (v *CookieVerifier) LoginRedirect(w http.ResponseWriter, r *http.Request) {
	target := v.LoginURL + "/?return_to=" + pyQuote(RequestURL(r), "")
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// RequestURL reconstructs the absolute URL the browser asked for, as
// Starlette's request.url did behind uvicorn --proxy-headers
// --forwarded-allow-ips=127.0.0.1: the scheme comes from X-Forwarded-Proto
// only when the immediate peer is loopback (the local reverse proxy).
func RequestURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	} else if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			proto := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]))
			if proto == "http" || proto == "https" {
				scheme = proto
			}
		}
	}
	return scheme + "://" + r.Host + r.URL.RequestURI()
}

// pyQuote is Python's urllib.parse.quote(s, safe=safe): percent-encode every
// byte except ASCII letters, digits, "_.-~" and the characters in safe.
func pyQuote(s, safe string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			strings.IndexByte("_.-~", c) >= 0 || (c < 0x80 && strings.IndexByte(safe, c) >= 0) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&15])
	}
	return b.String()
}
