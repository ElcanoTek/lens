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
	"slices"
	"strings"
	"sync"
	"time"
)

// JWKS caching policy.
const (
	JWKSCacheSeconds      = 10 * 60
	JWKSMinRefreshSeconds = 60
	MaxJWKSBytes          = 64 * 1024
	jwksTimeout           = 5 * time.Second
)

// StaticSigningKeys returns the statically configured Auth keys:
// AUTH_SIGNING_PUBKEY plus the comma-separated AUTH_SIGNING_PREVIOUS_PUBKEYS,
// trimmed, empties dropped.
func StaticSigningKeys(getenv func(string) (string, bool)) []string {
	primary, _ := getenv("AUTH_SIGNING_PUBKEY")
	previous, _ := getenv("AUTH_SIGNING_PREVIOUS_PUBKEYS")
	candidates := append([]string{primary}, strings.Split(previous, ",")...)
	out := make([]string, 0, len(candidates))
	for _, key := range candidates {
		if key = strings.TrimSpace(key); key != "" {
			out = append(out, key)
		}
	}
	return out
}

// RequireStaticSigningKeys is StaticSigningKeys for central mode, which cannot
// verify back-channel logout without the Auth public key: fail at startup
// instead of answering every revocation with 400 forever.
func RequireStaticSigningKeys(getenv func(string) (string, bool)) ([]string, error) {
	keys := StaticSigningKeys(getenv)
	if len(keys) == 0 {
		return nil, errors.New("AUTH_SIGNING_PUBKEY is required in central mode: run `auth pubkey` on the auth host. " +
			"Rotation keys are fetched from Auth's /jwks.json at runtime, but one static key " +
			"is needed so verification works even when Auth is unreachable at startup.")
	}
	for _, encoded := range keys {
		raw, ok := decodeStdBase64Strict(encoded)
		if !ok {
			return nil, errors.New("AUTH_SIGNING_PUBKEY is not valid base64")
		}
		if len(raw) != 32 {
			return nil, errors.New("AUTH_SIGNING_PUBKEY must decode to a 32-byte Ed25519 key")
		}
	}
	return keys, nil
}

// JWKSFetcher fetches the JWKS document at url.
type JWKSFetcher func(ctx context.Context, url string) ([]byte, error)

// HTTPJWKSFetcher fetches with client (redirects are never followed; nil
// means a default client), bounded by a 5-second timeout and MaxJWKSBytes.
func HTTPJWKSFetcher(client *http.Client) JWKSFetcher {
	if client == nil {
		client = noRedirectClient()
	}
	return func(ctx context.Context, url string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(ctx, jwksTimeout)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return nil, fmt.Errorf("JWKS fetch returned HTTP %d", resp.StatusCode)
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxJWKSBytes+1))
		if err != nil {
			return nil, err
		}
		if len(raw) > MaxJWKSBytes {
			return nil, newError(KindGeneric, "The JWKS document was too large", nil)
		}
		return raw, nil
	}
}

// KeyResolver supplies Auth's Ed25519 public keys: static env keys plus
// Auth's published JWKS.
//
// Auth rotates its signing key by publishing the new key alongside the old
// one at /jwks.json. Reading that document here makes rotation a one-sided
// change on Auth instead of an env edit on every application host. Static
// keys stay as the bootstrap and offline fallback: a fetch failure keeps
// whatever was cached, and verification never depends on Auth being up.
//
// Safe for concurrent use. The mutex is never held across a fetch, so a
// static-key token never waits behind a slow JWKS request.
type KeyResolver struct {
	jwksURL    string
	staticKeys []string
	fetch      JWKSFetcher
	now        func() time.Time

	mu          sync.Mutex
	remoteKeys  []string
	fetchedAt   time.Time // zero: never fetched
	lastAttempt time.Time // zero: never attempted
}

// NewKeyResolver builds a resolver for issuerURL's /jwks.json. fetch nil
// means HTTPJWKSFetcher(nil); now nil means time.Now.
func NewKeyResolver(issuerURL string, staticKeys []string, fetch JWKSFetcher, now func() time.Time) *KeyResolver {
	if fetch == nil {
		fetch = HTTPJWKSFetcher(nil)
	}
	if now == nil {
		now = time.Now
	}
	return &KeyResolver{
		jwksURL:    strings.TrimRight(issuerURL, "/") + "/jwks.json",
		staticKeys: slices.Clone(staticKeys),
		fetch:      fetch,
		now:        now,
	}
}

// JWKSURL is the document this resolver reads.
func (k *KeyResolver) JWKSURL() string { return k.jwksURL }

func (k *KeyResolver) merged() []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(k.staticKeys)+len(k.remoteKeys))
	for _, key := range append(slices.Clone(k.staticKeys), k.remoteKeys...) {
		if !seen[key] {
			seen[key] = true
			out = append(out, key)
		}
	}
	return out
}

// PublicKeys returns every trusted key, refreshing the JWKS first when the
// cache is empty or older than JWKSCacheSeconds.
func (k *KeyResolver) PublicKeys(ctx context.Context) []string {
	k.mu.Lock()
	stale := k.fetchedAt.IsZero() || k.now().Sub(k.fetchedAt) > JWKSCacheSeconds*time.Second
	k.mu.Unlock()
	if stale {
		k.Refresh(ctx, false)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.merged()
}

type jwksDocument struct {
	Keys []json.RawMessage `json:"keys"`
}

// Refresh fetches the JWKS; true when the cache was updated. Rate-limited to
// once a minute (unless force) so unknown-kid tokens cannot amplify requests
// to Auth.
func (k *KeyResolver) Refresh(ctx context.Context, force bool) bool {
	k.mu.Lock()
	now := k.now()
	if !force && !k.lastAttempt.IsZero() && now.Sub(k.lastAttempt) < JWKSMinRefreshSeconds*time.Second {
		k.mu.Unlock()
		return false
	}
	k.lastAttempt = now
	k.mu.Unlock()

	raw, err := k.fetch(ctx, k.jwksURL)
	if err != nil {
		return false
	}
	var document jwksDocument
	if err := json.Unmarshal(raw, &document); err != nil || document.Keys == nil {
		return false
	}
	parsed := []string{}
	for _, entryRaw := range document.Keys {
		var entry map[string]any
		if json.Unmarshal(entryRaw, &entry) != nil {
			continue
		}
		kty, _ := entry["kty"].(string)
		crv, _ := entry["crv"].(string)
		x, ok := entry["x"].(string)
		if kty != "OKP" || crv != "Ed25519" || !ok {
			continue
		}
		key, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(x, "="))
		if err != nil || len(key) != 32 {
			continue
		}
		parsed = append(parsed, base64.StdEncoding.EncodeToString(key))
	}
	k.mu.Lock()
	k.remoteKeys = parsed
	k.fetchedAt = now
	k.mu.Unlock()
	return true
}

// KeysForToken returns the keys to try for one token.
//
// A kid naming a static (env) key never waits on Auth's JWKS endpoint, so
// revocation and provisioning keep working while Auth restarts. A key known
// only from /jwks.json is trusted while the cache is fresh; once stale, the
// document is fetched again, so a key Auth has retired stops verifying (a
// failed fetch keeps the cached keys). An unknown kid triggers one throttled
// refresh.
func (k *KeyResolver) KeysForToken(ctx context.Context, rawToken string) []string {
	kid, hasKid := tokenKid(rawToken)
	names := func(keys []string) bool {
		for _, key := range keys {
			if keyKid, ok := KidForKey(key); ok && keyKid == kid {
				return true
			}
		}
		return false
	}
	if hasKid && names(k.staticKeys) {
		k.mu.Lock()
		defer k.mu.Unlock()
		return k.merged()
	}
	keys := k.PublicKeys(ctx)
	if hasKid && !names(keys) && k.Refresh(ctx, false) {
		keys = k.PublicKeys(ctx)
	}
	return keys
}
