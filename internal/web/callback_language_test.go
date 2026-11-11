// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package web

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/lens/internal/auth"
	"github.com/ElcanoTek/lens/internal/i18n"
)

// The fake issuer checks the fixed callback URI and S256 verifier, then signs
// an ID token for the requested nonce. All requests stay on local test servers.
func newCallbackLanguageHarness(t *testing.T) (*harness, *auth.CentralProvider) {
	t.Helper()
	key := newTestAuth(t)
	kid, _ := auth.KidForKey(key.pub)
	var issuer *httptest.Server
	issuer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.PostForm.Get("redirect_uri") != "http://lens.example.test/auth/callback" || r.PostForm.Get("client_id") != "lens" {
			t.Error("token exchange changed the registered callback or client")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		code := r.PostForm.Get("code")
		if code == "rejected" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if code == "unavailable" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		nonce, challenge, ok := strings.Cut(code, ".")
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
			t.Error("PKCE verifier did not match the authorization request")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "JWT", "kid": kid})
		now := time.Now().Unix()
		claims, _ := json.Marshal(map[string]any{"iss": issuer.URL, "aud": "lens", "sub": "tester", "email": "tester@example.com", "iat": now, "exp": now + 300, "nonce": nonce})
		body := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
		signature := ed25519.Sign(key.priv, []byte(body))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"id_token": body + "." + base64.RawURLEncoding.EncodeToString(signature)})
	}))
	t.Cleanup(issuer.Close)
	env := map[string]string{
		"LENS_AUTH_MODE": "central", "LENS_SESSION_SECRET": "test-session-secret-with-at-least-32-bytes",
		"LENS_ACCESS_DB": filepath.Join(t.TempDir(), "access.db"), "AUTH_ISSUER_URL": issuer.URL,
		"LENS_PUBLIC_URL": "http://lens.example.test", "AUTH_CLIENT_SECRET": "test-client-secret-with-at-least-32-bytes",
		"AUTH_SIGNING_PUBKEY": key.pub, "LENS_AUTH_COOKIE_SECURE": "0", "AUTH_ALLOW_INSECURE_HTTP": "1", "AUTH_CLIENT_ID": "lens",
	}
	provider, err := auth.NewProviderFromEnv(context.Background(), func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	if err != nil {
		t.Fatal(err)
	}
	central := provider.(*auth.CentralProvider)
	t.Cleanup(func() { _ = central.Store.Close() })
	if err := central.Store.GrantAccess(context.Background(), "tester@example.com"); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, false, func(o *Options) { o.Auth = NewAuthProvider(provider) })
	h.client.Jar = noLanguageCookieJar{h.client.Jar}
	return h, central
}

func startCallbackLanguageFlow(t *testing.T, h *harness, path, language string) url.Values {
	t.Helper()
	start, _ := h.get(path)
	authorize, err := url.Parse(location(start))
	if err != nil || start.StatusCode != http.StatusSeeOther {
		t.Fatalf("login: %d %q", start.StatusCode, location(start))
	}
	q := authorize.Query()
	if q.Get("ui_locales") != language || q.Get("redirect_uri") != "http://lens.example.test/auth/callback" || q.Get("state") == "" || q.Get("nonce") == "" || q.Get("code_challenge_method") != "S256" {
		t.Fatalf("authorization handoff changed: %v", q)
	}
	return q
}

func TestCentralCallbackKeepsSignedOutLinkLanguageWithoutStorage(t *testing.T) {
	for _, language := range []string{"es", "pt"} {
		t.Run(language, func(t *testing.T) {
			h, _ := newCallbackLanguageHarness(t)
			_, signedOut := h.get("/signed-out?lang=" + language)
			link := regexp.MustCompile(`href="([^"]*/auth/login\?[^"]+)"`).FindStringSubmatch(signedOut)
			if link == nil {
				t.Fatal("signed-out page has no sign-in link")
			}
			q := startCallbackLanguageFlow(t, h, html.UnescapeString(link[1]), language)
			callback, _ := h.get("/auth/callback?" + url.Values{"state": {q.Get("state")}, "code": {q.Get("nonce") + "." + q.Get("code_challenge")}}.Encode())
			if callback.StatusCode != http.StatusSeeOther || location(callback) != "/?lang="+language {
				t.Fatalf("callback lost language: %d %q", callback.StatusCode, location(callback))
			}
			page, body := h.get(location(callback))
			if page.StatusCode != http.StatusOK || page.Header.Get("Content-Language") != language || !strings.Contains(body, `<html lang="`+language+`">`) {
				t.Fatal("final dashboard language was not preserved")
			}
		})
	}
}

func TestCentralCallbackPreservesOpaqueReturnQuery(t *testing.T) {
	for _, language := range []string{"es", "pt"} {
		t.Run(language, func(t *testing.T) {
			h, _ := newCallbackLanguageHarness(t)
			next := "/?q=alpha;beta&opaque=100%done&filter=a%26b&filter=c&l%61ng=en&%6cang=pt#saved"
			q := startCallbackLanguageFlow(t, h, "/auth/login?"+url.Values{"next": {next}, "lang": {language + "-XX"}}.Encode(), language)
			callback, _ := h.get("/auth/callback?" + url.Values{"state": {q.Get("state")}, "code": {q.Get("nonce") + "." + q.Get("code_challenge")}}.Encode())
			want := "/?q=alpha;beta&opaque=100%done&filter=a%26b&filter=c&lang=" + language + "#saved"
			if callback.StatusCode != http.StatusSeeOther || location(callback) != want {
				t.Fatalf("return query changed: %d %q, want %q", callback.StatusCode, location(callback), want)
			}
		})
	}
}

func TestCentralCallbackErrorsUseOnlyVerifiedTransactionLanguage(t *testing.T) {
	for _, language := range []string{"es", "pt"} {
		for _, tc := range []struct {
			name, source string
			status       int
			fallback     bool
		}{
			{"missing code", "Sign-in was not completed", 400, false},
			{"Auth denied", "Sign-in was not completed", 400, false},
			{"rejected code", "Sign-in expired. Try again.", 400, false},
			{"unavailable", "The authentication service is unavailable", 502, false},
			{"access denied", "Your account does not have Lens access", 403, false},
			{"storage failure", "Internal Server Error", 500, false},
			{"invalid state", "Sign-in expired. Try again.", 400, true},
			{"missing state", "Sign-in was not completed", 400, true},
			{"tampered cookie", "Sign-in expired. Try again.", 400, true},
			{"missing transaction", "Sign-in expired. Try again.", 400, true},
			{"expired transaction", "Sign-in expired. Try again.", 400, true},
			{"missing nonce", "Sign-in expired. Try again.", 400, true},
			{"missing verifier", "Sign-in expired. Try again.", 400, true},
			{"explicit English", "Sign-in expired. Try again.", 400, true},
		} {
			t.Run(language+"/"+tc.name, func(t *testing.T) {
				h, provider := newCallbackLanguageHarness(t)
				q := startCallbackLanguageFlow(t, h, "/auth/login?next=%2F&lang="+language, language)
				callback := url.Values{"state": {q.Get("state")}, "code": {q.Get("nonce") + "." + q.Get("code_challenge")}}
				u, _ := url.Parse(h.ts.URL)
				switch tc.name {
				case "missing code":
					callback.Del("code")
				case "Auth denied":
					callback.Set("error", "access_denied")
				case "rejected code", "explicit English":
					callback.Set("code", "rejected")
					if tc.fallback {
						callback.Set("lang", "en")
					}
				case "unavailable":
					callback.Set("code", "unavailable")
				case "access denied":
					if _, err := provider.Store.RevokeAccess(context.Background(), "tester@example.com"); err != nil {
						t.Fatal(err)
					}
				case "storage failure":
					if err := provider.Store.Close(); err != nil {
						t.Fatal(err)
					}
				case "invalid state":
					callback.Set("state", "tampered")
				case "missing state":
					callback.Del("state")
				case "tampered cookie":
					h.client.Jar.SetCookies(u, []*http.Cookie{{Name: auth.TransactionCookieName, Value: "tampered", Path: "/"}})
				case "missing transaction":
					h.client.Jar.SetCookies(u, []*http.Cookie{{Name: auth.TransactionCookieName, Value: "", Path: "/", MaxAge: -1}})
				case "expired transaction", "missing nonce", "missing verifier":
					r := httptest.NewRequest(http.MethodGet, h.ts.URL, nil)
					for _, cookie := range h.client.Jar.Cookies(u) {
						r.AddCookie(cookie)
					}
					var tx map[string]any
					if !provider.Transactions.Get(r, &tx) {
						t.Fatal("transaction missing")
					}
					switch tc.name {
					case "expired transaction":
						tx["created_at"] = time.Now().Unix() - auth.LoginTransactionSeconds - 1
					case "missing nonce":
						tx["nonce"] = ""
					case "missing verifier":
						tx["verifier"] = ""
					}
					w := httptest.NewRecorder()
					if err := provider.Transactions.Set(w, tx); err != nil {
						t.Fatal(err)
					}
					h.client.Jar.SetCookies(u, w.Result().Cookies())
				}
				response, body := h.get("/auth/callback?" + callback.Encode())
				wantLanguage := language
				if tc.fallback {
					wantLanguage = "en"
				}
				if response.StatusCode != tc.status || !strings.Contains(body, i18n.T(wantLanguage, tc.source)) {
					t.Fatalf("callback = %d %s, want %d %s", response.StatusCode, body, tc.status, i18n.T(wantLanguage, tc.source))
				}
				for _, cookie := range h.client.Jar.Cookies(u) {
					if cookie.Name == provider.CookieName || cookie.Name == auth.TransactionCookieName || cookie.Name == "elcano_language" {
						t.Fatalf("failed callback retained unexpected cookie %s", cookie.Name)
					}
				}
			})
		}
	}
}
