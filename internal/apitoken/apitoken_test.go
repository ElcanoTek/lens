// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package apitoken

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store := NewStore(filepath.Join(t.TempDir(), "state", "api_tokens.db"))
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func isTokenError(err error, contains string) bool {
	var te *TokenError
	return errors.As(err, &te) && strings.Contains(te.Error(), contains)
}

func TestTokensStoreOnlyAHashAndRevocationIsImmediate(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	token, raw, err := store.Create(ctx, "fleet-elcano", []string{"jobs:read"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(raw, "lens_") || len(raw) <= 40 {
		t.Fatalf("raw token %q", raw)
	}
	db, err := sql.Open("sqlite", store.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var hash, prefix string
	if err := db.QueryRow("SELECT token_hash, prefix FROM api_tokens").Scan(&hash, &prefix); err != nil {
		t.Fatal(err)
	}
	if hash == raw || prefix != raw[:12] || len(hash) != 64 {
		t.Fatalf("stored hash=%q prefix=%q", hash, prefix)
	}
	info, err := os.Stat(store.Path())
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("db mode %v %v", info.Mode(), err)
	}
	verified, err := store.Verify(ctx, raw)
	if err != nil || strings.Join(verified.Scopes, " ") != "jobs:read" {
		t.Fatalf("verify: %v %v", verified, err)
	}
	if verified.Actor() != "token:fleet-elcano" || !verified.Allows(ScopeRead) || verified.Allows(ScopeRun) {
		t.Fatalf("token %+v", verified)
	}
	if ok, err := store.Revoke(ctx, token.ID); !ok || err != nil {
		t.Fatalf("revoke: %v %v", ok, err)
	}
	if _, err := store.Verify(ctx, raw); !isTokenError(err, "revoked") {
		t.Fatalf("revoked verify: %v", err)
	}
	if _, err := store.Verify(ctx, "lens_not-a-real-token"); !isTokenError(err, "Invalid API token") {
		t.Fatalf("unknown verify: %v", err)
	}
	for _, bad := range []string{"", "nope", "lens_" + strings.Repeat("x", 200)} {
		if _, err := store.Verify(ctx, bad); !isTokenError(err, "Invalid") {
			t.Fatalf("Verify(%q) = %v", bad, err)
		}
	}
}

func TestTokenScopesAndLabelsAreValidated(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	cases := []struct {
		label  string
		scopes []string
		want   string
	}{
		{"x", []string{"admin"}, "unknown scope"},
		{"", []string{"jobs:read"}, "label"},
		{"-dash-first", []string{"jobs:read"}, "label"},
		{strings.Repeat("a", 81), []string{"jobs:read"}, "label"},
		{"ok", nil, "at least one scope"},
	}
	for _, tc := range cases {
		_, _, err := store.Create(ctx, tc.label, tc.scopes)
		var verr *ValidationError
		if !errors.As(err, &verr) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Create(%q, %v) = %v, want %q", tc.label, tc.scopes, err, tc.want)
		}
	}
	token, _, err := store.Create(ctx, "  spaced \t label ", []string{"jobs:run", "jobs:read", "jobs:run"})
	if err != nil {
		t.Fatal(err)
	}
	if token.Label != "spaced label" || strings.Join(token.Scopes, " ") != "jobs:read jobs:run" {
		t.Fatalf("normalized %+v", token)
	}
}

func TestUnauthenticatedLookupsDoNotCreateTheTokenDatabase(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	if _, err := store.Verify(ctx, "lens_anything"); !isTokenError(err, "Invalid") {
		t.Fatal(err)
	}
	if _, _, _, err := store.VerifyDownload(ctx, "abc.def"); !isTokenError(err, "Invalid download link") {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.Path()); !os.IsNotExist(err) {
		t.Fatalf("database exists: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(store.Path())); !os.IsNotExist(err) {
		t.Fatalf("directory exists: %v", err)
	}
}

func TestLastUsedIsWrittenAtMostOncePerMinute(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	clock := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	store.SetClock(func() time.Time { return clock })
	_, raw, err := store.Create(ctx, "fleet", Scopes)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := store.Verify(ctx, raw)
	if first.LastUsedAt != "2026-10-21T12:00:00+00:00" {
		t.Fatalf("last used %q", first.LastUsedAt)
	}
	clock = clock.Add(59 * time.Second)
	second, _ := store.Verify(ctx, raw)
	if second.LastUsedAt != first.LastUsedAt {
		t.Fatalf("touched too early: %q", second.LastUsedAt)
	}
	clock = clock.Add(time.Second)
	third, _ := store.Verify(ctx, raw)
	if third.LastUsedAt != "2026-10-21T12:01:00+00:00" {
		t.Fatalf("not touched: %q", third.LastUsedAt)
	}
}

func TestSignedDownloadLinkIsScopedTamperProofAndExpires(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	token, raw, err := store.Create(ctx, "fleet", Scopes)
	if err != nil {
		t.Fatal(err)
	}
	ticket, expires, err := store.SignDownload(ctx, token, "20261015143000-a1b2c3", "output", 0)
	if err != nil {
		t.Fatal(err)
	}
	if expires-time.Now().Unix() < DownloadLinkSeconds-5 {
		t.Fatalf("expiry %d", expires)
	}
	got, job, kind, err := store.VerifyDownload(ctx, ticket)
	if err != nil || got.ID != token.ID || job != "20261015143000-a1b2c3" || kind != "output" {
		t.Fatalf("verify download: %v %q %q %v", got, job, kind, err)
	}

	// Tampering with the payload (another file kind) breaks the signature.
	payloadText, signature, _ := strings.Cut(ticket, ".")
	payload, err := decodeB64Lenient(payloadText)
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(payload, []byte(`{"t":`)) {
		t.Fatalf("payload layout %s", payload)
	}
	claims["k"] = "log"
	forged, _ := json.Marshal(claims)
	if _, _, _, err := store.VerifyDownload(ctx, b64(forged)+"."+signature); !isTokenError(err, "Invalid download link") {
		t.Fatalf("forged: %v", err)
	}
	for _, bad := range []string{"", "nodot", "!!.!!", payloadText + ".AAAA"} {
		if _, _, _, err := store.VerifyDownload(ctx, bad); !isTokenError(err, "Invalid download link") {
			t.Fatalf("VerifyDownload(%q) = %v", bad, err)
		}
	}

	store.SetClock(func() time.Time { return time.Now().Add(time.Hour) })
	if _, _, _, err := store.VerifyDownload(ctx, ticket); !isTokenError(err, "expired") {
		t.Fatalf("expired: %v", err)
	}
	store.SetClock(time.Now)

	verified, err := store.Verify(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Revoke(ctx, verified.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.VerifyDownload(ctx, ticket); !isTokenError(err, "no longer valid") {
		t.Fatalf("revoked: %v", err)
	}
}

func TestDownloadLinkNeedsAReadScopedToken(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	token, _, err := store.Create(ctx, "runner", []string{ScopeRun})
	if err != nil {
		t.Fatal(err)
	}
	ticket, _, err := store.SignDownload(ctx, token, "job", "log", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.VerifyDownload(ctx, ticket); !isTokenError(err, "no longer valid") {
		t.Fatalf("run-only token: %v", err)
	}
}

// A signing key written by the Python release (urlsafe base64 with padding)
// keeps verifying tickets: the key is reused, never regenerated.
func TestSigningKeyIsCreatedOnceAndReused(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	var wg sync.WaitGroup
	keys := make([][]byte, 8)
	for i := range keys {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key, err := store.signingKey(ctx)
			if err != nil {
				t.Error(err)
			}
			keys[i] = key
		}()
	}
	wg.Wait()
	for _, key := range keys {
		if len(key) != 32 || !bytes.Equal(key, keys[0]) {
			t.Fatalf("keys differ: %x vs %x", key, keys[0])
		}
	}
	db, _ := sql.Open("sqlite", store.Path())
	defer func() { _ = db.Close() }()
	var stored string
	if err := db.QueryRow("SELECT value FROM api_meta WHERE key='download_signing_key'").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(stored, "=") || len(stored) != 44 {
		t.Fatalf("stored key format %q", stored)
	}
}

func TestTokenCLICreateListRevoke(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "api_tokens.db")
	var out, errOut bytes.Buffer
	cli := func(args ...string) int {
		out.Reset()
		errOut.Reset()
		return run(ctx, args, &out, &errOut, NewStore(path), "https://lens.example.test/")
	}

	if code := cli("create", "--label", "fleet-a", "--json"); code != 0 {
		t.Fatalf("create: %d %s", code, errOut.String())
	}
	var created struct {
		ID     int64    `json:"id"`
		Label  string   `json:"label"`
		Scopes []string `json:"scopes"`
		Token  string   `json:"token"`
	}
	if err := json.Unmarshal(out.Bytes(), &created); err != nil {
		t.Fatal(err, out.String())
	}
	if strings.Join(created.Scopes, ",") != "jobs:read,jobs:run" || created.Label != "fleet-a" {
		t.Fatalf("created %+v", created)
	}
	if !strings.HasPrefix(out.String(), `{"id": 1, "label": "fleet-a", "scopes": ["jobs:read", "jobs:run"], "token": "lens_`) {
		t.Fatalf("json layout %s", out.String())
	}

	if code := cli("create", "--label=reporting", "--scope", "jobs:read"); code != 0 {
		t.Fatalf("create plain: %d %s", code, errOut.String())
	}
	for _, want := range []string{
		"Created token 2 (reporting) with scopes: jobs:read",
		"Copy it now; Lens stores only its hash",
		"LENS_MCP_URL=https://lens.example.test/mcp",
		"LENS_API_TOKEN=<the token above>",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("create output lacks %q:\n%s", want, out.String())
		}
	}

	if code := cli("list"); code != 0 {
		t.Fatal(errOut.String())
	}
	if !strings.Contains(out.String(), "fleet-a") || strings.Contains(out.String(), created.Token) {
		t.Fatalf("listing:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "  ID  STATE     PREFIX") || !strings.Contains(out.String(), created.Token[:12]+"…") {
		t.Fatalf("listing layout:\n%s", out.String())
	}

	if code := cli("revoke", "1"); code != 0 || !strings.Contains(out.String(), "Revoked token 1.") {
		t.Fatalf("revoke: %d %s", code, out.String())
	}
	if code := cli("revoke", "1"); code != 1 || !strings.Contains(errOut.String(), "No active token with id 1") {
		t.Fatalf("second revoke: %d %s", code, errOut.String())
	}
	if code := cli("list"); code != 0 || !strings.Contains(out.String(), "revoked") {
		t.Fatalf("list after revoke: %s", out.String())
	}

	for _, args := range [][]string{{}, {"bogus"}, {"create"}, {"create", "--label", "x", "--scope", "admin"}, {"revoke", "abc"}} {
		if code := cli(args...); code != 2 {
			t.Errorf("%v exit %d, want 2", args, code)
		}
	}
	if code := cli("create", "--label", "!bad"); code != 1 || !strings.Contains(errOut.String(), "lens token: label must be") {
		t.Fatalf("bad label: %d %s", code, errOut.String())
	}
}

func TestEmptyListAndDefaultFleetHost(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "api_tokens.db")
	var out, errOut bytes.Buffer
	if code := run(ctx, []string{"list"}, &out, &errOut, NewStore(path), ""); code != 0 {
		t.Fatal(errOut.String())
	}
	if !strings.Contains(out.String(), "No API tokens. Create one with: lens token create --label NAME") {
		t.Fatal(out.String())
	}
	out.Reset()
	if code := run(ctx, []string{"create", "--label", "x"}, &out, &errOut, NewStore(path), ""); code != 0 {
		t.Fatal(errOut.String())
	}
	if !strings.Contains(out.String(), "LENS_MCP_URL=https://<this host>/mcp") {
		t.Fatal(out.String())
	}
}

func TestDBPathDefaultsToTheStateDirectory(t *testing.T) {
	t.Setenv("LENS_API_TOKEN_DB", "/srv/tokens.db")
	if DBPath() != "/srv/tokens.db" {
		t.Fatal(DBPath())
	}
	_ = os.Unsetenv("LENS_API_TOKEN_DB")
	if DBPath() != DefaultDBPath || DefaultDBPath != "/var/lib/lens/api_tokens.db" {
		t.Fatal(DBPath())
	}
}
