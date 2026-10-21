// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

type signer struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
}

func newSigner(t *testing.T) signer {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return signer{priv: priv, pub: pub}
}

func (s signer) pubB64() string { return base64.StdEncoding.EncodeToString(s.pub) }
func (s signer) kid() string    { return kidForRaw(s.pub) }

func enc(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(data)
}

// signRaw signs header.claims where both are already-encoded JSON text.
func (s signer) signSegments(header, claims string) string {
	body := header + "." + claims
	return body + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(s.priv, []byte(body)))
}

// mint signs a JWS. A nil value in overrides deletes the claim; header
// overrides work the same way.
func (s signer) mint(t *testing.T, header, claims, headerOverrides, overrides map[string]any) string {
	t.Helper()
	apply := func(base, over map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range base {
			out[k] = v
		}
		for k, v := range over {
			if v == nil {
				delete(out, k)
			} else {
				out[k] = v
			}
		}
		return out
	}
	return s.signSegments(enc(t, apply(header, headerOverrides)), enc(t, apply(claims, overrides)))
}

func (s signer) logoutToken(t *testing.T, overrides map[string]any) string {
	return s.mint(t, map[string]any{"typ": "logout+jwt", "alg": "EdDSA", "kid": s.kid()}, map[string]any{
		"iss": "https://auth.example.com", "sub": "account-123", "aud": "lens", "iat": 1000, "exp": 1300,
		"jti": "event-123", "events": map[string]any{BackchannelLogoutEvent: map[string]any{}},
	}, nil, overrides)
}

func (s signer) accessToken(t *testing.T, action string, version any, overrides map[string]any) string {
	jti := "access-" + strings.Trim(string(mustJSON(t, version)), `"`)
	return s.mint(t, map[string]any{"typ": "access+jwt", "alg": "EdDSA", "kid": s.kid()}, map[string]any{
		"iss": "https://auth.example.com", "sub": "account-123", "aud": "lens", "email": "alice@example.com",
		"iat": 1000, "exp": 1300, "jti": jti,
		"events": map[string]any{ApplicationAccessEvent: map[string]any{"action": action, "version": version}},
	}, nil, overrides)
}

func (s signer) idToken(t *testing.T, headerOverrides, overrides map[string]any) string {
	return s.mint(t, map[string]any{"typ": "JWT", "alg": "EdDSA", "kid": s.kid()}, map[string]any{
		"iss": "https://auth.example.com", "sub": "account-123", "aud": "lens", "email": "Alice@Example.com",
		"iat": 1000, "exp": 1300, "nonce": "expected-nonce", "auth_time": 1000, "amr": []string{"pwd"},
		"acr": "urn:elcanotek:loa:1",
	}, headerOverrides, overrides)
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func i64(v int64) *int64 { return &v }

func envMap(m map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := m[name]
		return v, ok
	}
}
