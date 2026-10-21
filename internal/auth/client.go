// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// MaxTokenResponseBytes caps Auth's /token response.
	MaxTokenResponseBytes = 64 * 1024
	tokenExchangeTimeout  = 10 * time.Second
)

func noRedirectClient() *http.Client {
	return &http.Client{
		// Auth never redirects /token or /jwks.json; following one would send
		// the client secret (or trust keys) somewhere else.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// origin canonicalises an exact origin: https (or http when allowed), a host,
// no credentials, query, fragment or path.
func origin(name, raw string, allowHTTP bool) (string, error) {
	fail := fmt.Errorf("%s must be an HTTPS origin without a path or query", name)
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fail
	}
	schemeOK := parsed.Scheme == "https" || (allowHTTP && parsed.Scheme == "http")
	if !schemeOK || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" ||
		parsed.Fragment != "" || parsed.Opaque != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", fail
	}
	return strings.TrimRight(parsed.Scheme+"://"+parsed.Host, "/"), nil
}

// Client is Lens's confidential client of central Auth: authorization-code
// flow with PKCE (S256) and a nonce, token exchange with HTTP Basic client
// credentials.
type Client struct {
	IssuerURL string
	PublicURL string
	ClientID  string
	secret    string
	// HTTPClient performs the token exchange. It must not follow redirects;
	// the default does not.
	HTTPClient *http.Client
}

// NewClient validates and canonicalises the client configuration.
func NewClient(issuerURL, publicURL, clientID, clientSecret string, allowInsecureHTTP bool) (*Client, error) {
	issuer, err := origin("AUTH_ISSUER_URL", issuerURL, allowInsecureHTTP)
	if err != nil {
		return nil, err
	}
	public, err := origin("LENS_PUBLIC_URL", publicURL, allowInsecureHTTP)
	if err != nil {
		return nil, err
	}
	clientID = strings.TrimSpace(clientID)
	if clientID == "" || strings.Contains(clientID, ":") || utf8.RuneCountInString(clientID) > 128 {
		return nil, errors.New("AUTH_CLIENT_ID must be 1-128 characters without ':'")
	}
	if len(clientSecret) < 32 || utf8.RuneCountInString(clientSecret) > 256 || strings.Contains(clientSecret, ":") {
		return nil, errors.New("AUTH_CLIENT_SECRET must contain 32-256 bytes without ':'")
	}
	return &Client{IssuerURL: issuer, PublicURL: public, ClientID: clientID, secret: clientSecret, HTTPClient: noRedirectClient()}, nil
}

// NewClientFromEnv reads AUTH_ISSUER_URL, LENS_PUBLIC_URL, AUTH_CLIENT_ID
// (default "lens"), AUTH_CLIENT_SECRET and AUTH_ALLOW_INSECURE_HTTP.
func NewClientFromEnv(getenv func(string) (string, bool)) (*Client, error) {
	get := func(name, fallback string) string {
		if v, ok := getenv(name); ok {
			return v
		}
		return fallback
	}
	// Deliberately the Python client's own parse (lower-cased, unstripped,
	// no "on"); the provider applies the stricter Secure-cookie rule first.
	insecure := strings.ToLower(get("AUTH_ALLOW_INSECURE_HTTP", "0"))
	return NewClient(get("AUTH_ISSUER_URL", ""), get("LENS_PUBLIC_URL", ""), get("AUTH_CLIENT_ID", "lens"),
		get("AUTH_CLIENT_SECRET", ""), insecure == "1" || insecure == "true" || insecure == "yes")
}

// CallbackURL is where Auth sends the browser back.
func (c *Client) CallbackURL() string { return c.PublicURL + "/auth/callback" }

// orderedQuery is Python's urllib.parse.urlencode over ordered pairs.
func orderedQuery(pairs ...[2]string) string {
	parts := make([]string, len(pairs))
	for i, pair := range pairs {
		parts[i] = url.QueryEscape(pair[0]) + "=" + url.QueryEscape(pair[1])
	}
	return strings.Join(parts, "&")
}

// AuthorizationURL is Auth's /authorize for one login transaction.
func (c *Client) AuthorizationURL(state, codeChallenge, nonce string) string {
	return c.IssuerURL + "/authorize?" + orderedQuery(
		[2]string{"response_type", "code"},
		[2]string{"client_id", c.ClientID},
		[2]string{"redirect_uri", c.CallbackURL()},
		[2]string{"scope", "openid email"},
		[2]string{"state", state},
		[2]string{"code_challenge", codeChallenge},
		[2]string{"code_challenge_method", "S256"},
		[2]string{"nonce", nonce},
	)
}

// Exchange redeems a single-use code at Auth's /token and returns the
// identity from the signed id_token. Errors are *CentralAuthError: an HTTP
// 400 (Auth's invalid_grant: consumed, expired, or superseded by a newer
// /authorize for this browser, e.g. two tabs) is KindCodeRejected — the user
// retries; anything else is KindGeneric.
func (c *Client) Exchange(ctx context.Context, code, verifier, expectedNonce string, keys *KeyResolver) (AuthenticatedPrincipal, error) {
	rejected := newError(KindGeneric, "The authentication service rejected the code exchange", nil)
	invalid := newError(KindGeneric, errAuthResp, nil)
	body := orderedQuery(
		[2]string{"grant_type", "authorization_code"},
		[2]string{"code", code},
		[2]string{"redirect_uri", c.CallbackURL()},
		[2]string{"client_id", c.ClientID},
		[2]string{"code_verifier", verifier},
	)
	ctx, cancel := context.WithTimeout(ctx, tokenExchangeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.IssuerURL+"/token", strings.NewReader(body))
	if err != nil {
		return AuthenticatedPrincipal{}, rejected
	}
	credentials := base64.StdEncoding.EncodeToString([]byte(c.ClientID + ":" + c.secret))
	req.Header.Set("Authorization", "Basic "+credentials)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Cache-Control", "no-store")
	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = noRedirectClient()
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		// The transport error names only the /token URL, never the secret,
		// but it stays out of the message anyway.
		return AuthenticatedPrincipal{}, newError(KindGeneric, rejected.msg, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusBadRequest {
		return AuthenticatedPrincipal{}, newError(KindCodeRejected, "The authentication service rejected the sign-in code", nil)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return AuthenticatedPrincipal{}, rejected
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxTokenResponseBytes+1))
	if err != nil {
		return AuthenticatedPrincipal{}, newError(KindGeneric, rejected.msg, err)
	}
	if len(raw) > MaxTokenResponseBytes {
		return AuthenticatedPrincipal{}, invalid
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return AuthenticatedPrincipal{}, invalid
	}
	idToken, ok := payload["id_token"].(string)
	if !ok || idToken == "" {
		return AuthenticatedPrincipal{}, invalid
	}
	// Auth's contract: the identity is the signed id_token, verified against
	// Auth's published keys, not the unsigned copy of its claims in this JSON.
	// The keys come from the same issuer over the same TLS, so this does not
	// defend against a mis-pointed AUTH_ISSUER_URL or a party terminating that
	// TLS; it binds the sign-in to Auth's key.
	return VerifyIDToken(idToken, c.IssuerURL, c.ClientID, expectedNonce, keys.KeysForToken(ctx, idToken), nil)
}
