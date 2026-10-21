// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package auth

import (
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// mintCookie mints an elcano_auth value exactly like auth-server does.
func mintCookie(t *testing.T, s signer, overrides map[string]any) string {
	t.Helper()
	now := time.Now().Unix()
	payload := map[string]any{"email": "alice@elcanotek.com", "tenant": "elcanotek.com", "iat": now, "exp": now + 3600}
	for k, v := range overrides {
		payload[k] = v
	}
	body := enc(t, payload)
	return body + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(s.priv, []byte(body)))
}

func TestElcanoCookieVerification(t *testing.T) {
	key := newSigner(t)
	verifier := NewCookieVerifier(key.pubB64(), DefaultAuthCookieName, DefaultAuthLoginURL)

	session, ok := verifier.VerifySession(mintCookie(t, key, nil))
	if !ok || session.Email != "alice@elcanotek.com" || session.Tenant != "elcanotek.com" {
		t.Fatalf("valid cookie: %+v %v", session, ok)
	}
	if _, ok := verifier.VerifySession(mintCookie(t, key, map[string]any{"exp": 1.5e12})); !ok {
		t.Fatal("a float exp is accepted, as the Python did")
	}

	token := mintCookie(t, key, nil)
	_, sig, _ := strings.Cut(token, ".")
	tampered := base64.RawURLEncoding.EncodeToString([]byte(`{"email":"mallory@evil.com","exp":9999999999}`)) + "." + sig
	foreign := mintCookie(t, newSigner(t), nil)
	arrayBody := base64.RawURLEncoding.EncodeToString([]byte(`["alice@elcanotek.com"]`))

	rejected := map[string]string{
		"expired":        mintCookie(t, key, map[string]any{"exp": time.Now().Unix() - 1}),
		"missing email":  mintCookie(t, key, map[string]any{"email": ""}),
		"non-string":     mintCookie(t, key, map[string]any{"email": 7}),
		"string exp":     mintCookie(t, key, map[string]any{"exp": "soon"}),
		"tampered":       tampered,
		"foreign key":    foreign,
		"empty":          "",
		"not-a-token":    "not-a-token",
		"only-one-part":  "only-one-part",
		"three parts":    "a.b.c",
		"leading dot":    ".abc",
		"trailing dot":   "abc.",
		"not an object":  arrayBody + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key.priv, []byte(arrayBody))),
		"garbage base64": "!!!." + sig,
	}
	for name, value := range rejected {
		if _, ok := verifier.VerifySession(value); ok {
			t.Errorf("%s: accepted", name)
		}
	}

	for _, pub := range []string{"", "not base64!", base64.StdEncoding.EncodeToString([]byte("short"))} {
		if _, ok := NewCookieVerifier(pub, DefaultAuthCookieName, DefaultAuthLoginURL).VerifySession(token); ok {
			t.Errorf("pubkey %q verified a cookie", pub)
		}
	}
}

func TestCurrentIdentityReadsTheConfiguredCookie(t *testing.T) {
	key := newSigner(t)
	verifier := NewCookieVerifierFromEnv(envMap(map[string]string{"AUTH_SIGNING_PUBKEY": key.pubB64()}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if verifier.CurrentIdentity(req) != nil {
		t.Fatal("no cookie must be signed out")
	}
	req.AddCookie(&http.Cookie{Name: "elcano_auth", Value: mintCookie(t, key, nil)})
	identity := verifier.CurrentIdentity(req)
	if identity == nil || identity.Email != "alice@elcanotek.com" {
		t.Fatalf("identity %+v", identity)
	}

	renamed := NewCookieVerifierFromEnv(envMap(map[string]string{
		"AUTH_SIGNING_PUBKEY": key.pubB64(), "AUTH_COOKIE_NAME": "other_cookie",
	}))
	if renamed.CurrentIdentity(req) != nil {
		t.Fatal("AUTH_COOKIE_NAME must select the cookie")
	}
}

func TestLoginRedirectPointsAtAuthWithReturnTo(t *testing.T) {
	verifier := NewCookieVerifier("", DefaultAuthCookieName, "https://auth.elcanotek.com/")
	req := httptest.NewRequest(http.MethodGet, "https://explorer.elcanotek.com/email?s3_key=x", nil)
	rec := httptest.NewRecorder()
	verifier.LoginRedirect(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d", rec.Code)
	}
	want := "https://auth.elcanotek.com/?return_to=https%3A%2F%2Fexplorer.elcanotek.com%2Femail%3Fs3_key%3Dx"
	if got := rec.Header().Get("Location"); got != want {
		t.Fatalf("location %q, want %q", got, want)
	}
}

func TestRequestURLTrustsForwardedProtoOnlyFromLoopback(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://lens.example.com/jobs?x=1", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.RemoteAddr = "127.0.0.1:40000"
	if got := RequestURL(req); got != "https://lens.example.com/jobs?x=1" {
		t.Fatalf("loopback proxy: %q", got)
	}
	req.RemoteAddr = "203.0.113.9:40000"
	if got := RequestURL(req); got != "http://lens.example.com/jobs?x=1" {
		t.Fatalf("remote peer: %q", got)
	}
}

func TestPyQuote(t *testing.T) {
	cases := map[string]string{
		"/jobs?x=1&y=a b": "%2Fjobs%3Fx%3D1%26y%3Da%20b",
		"a-b_c.d~e":       "a-b_c.d~e",
		"é":               "%C3%A9",
	}
	for in, want := range cases {
		if got := pyQuote(in, ""); got != want {
			t.Errorf("pyQuote(%q) = %q, want %q", in, got, want)
		}
	}
	if got := pyQuote("/a b", "/"); got != "/a%20b" {
		t.Errorf("safe slash: %q", got)
	}
}
