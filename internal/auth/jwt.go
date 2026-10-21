// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package auth

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Event URIs Auth puts in the "events" claim of back-channel tokens.
const (
	BackchannelLogoutEvent = "http://schemas.openid.net/event/backchannel-logout"
	ApplicationAccessEvent = "urn:elcanotek:event:application-access"
	// ClockSkewSeconds is the tolerance on exp and iat.
	ClockSkewSeconds = 60
	// maxSignedTokenBytes bounds any compact JWS before it is parsed.
	maxSignedTokenBytes = 16_384
)

// Safe, credential-free messages for the three token kinds.
const (
	errLogoutToken = "The logout token was invalid"
	errAccessToken = "The access token was invalid"
	errAuthResp    = "The authentication response was invalid"
)

// AuthenticatedPrincipal is the identity Auth vouched for in an id_token.
type AuthenticatedPrincipal struct {
	Subject string
	Email   string
	Nonce   string
}

// LogoutEvent is a verified back-channel logout.
type LogoutEvent struct {
	EventID  string
	Subject  string
	Issuer   string
	IssuedAt int64
}

// AccessProvisionEvent is a verified application-access grant or revoke.
type AccessProvisionEvent struct {
	EventID  string
	Subject  string
	Email    string
	Issuer   string
	IssuedAt int64
	Version  int64
	Allowed  bool
}

var kidRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

const b64urlAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

func decodeSegment(segment string) ([]byte, bool) {
	if segment == "" {
		return nil, false
	}
	for i := 0; i < len(segment); i++ {
		if strings.IndexByte(b64urlAlphabet, segment[i]) < 0 {
			return nil, false
		}
	}
	out, err := base64.RawURLEncoding.DecodeString(segment)
	return out, err == nil
}

// hasLoneSurrogateEscape reports whether a JSON document escapes a UTF-16
// surrogate that is not part of a valid pair ("\ud800"). Python decodes such
// an escape into a string it then cannot encode, and the Python release
// refused those tokens; Go's decoder would silently substitute U+FFFD, so
// they are refused here before decoding.
func hasLoneSurrogateEscape(doc []byte) bool {
	hex4 := func(b []byte) (rune, bool) {
		if len(b) < 4 {
			return 0, false
		}
		var v rune
		for _, c := range b[:4] {
			switch {
			case c >= '0' && c <= '9':
				v = v<<4 | rune(c-'0')
			case c >= 'a' && c <= 'f':
				v = v<<4 | rune(c-'a'+10)
			case c >= 'A' && c <= 'F':
				v = v<<4 | rune(c-'A'+10)
			default:
				return 0, false
			}
		}
		return v, true
	}
	for i := 0; i < len(doc); i++ {
		if doc[i] != '\\' {
			continue
		}
		if i+1 >= len(doc) {
			return false
		}
		if doc[i+1] != 'u' {
			i++ // skip the escaped character, which may itself be a backslash
			continue
		}
		r, ok := hex4(doc[i+2:])
		if !ok {
			return false // malformed; the JSON decoder rejects it
		}
		switch {
		case r >= 0xDC00 && r <= 0xDFFF:
			return true
		case r >= 0xD800 && r <= 0xDBFF:
			rest := doc[i+6:]
			if len(rest) < 6 || rest[0] != '\\' || rest[1] != 'u' {
				return true
			}
			low, ok := hex4(rest[2:])
			if !ok || low < 0xDC00 || low > 0xDFFF {
				return true
			}
			i += 11
		default:
			i += 5
		}
	}
	return false
}

// decodeJSONObject decodes one JSON object with numbers kept as literals, so
// integer claims can be told apart from floats and booleans exactly as
// Python's isinstance(x, int) checks did.
func decodeJSONObject(doc []byte) (map[string]any, bool) {
	if !utf8.Valid(doc) || hasLoneSurrogateEscape(doc) {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, false
	}
	// Trailing data after the object is a decode error in Python too.
	if len(bytes.TrimSpace(doc[dec.InputOffset():])) != 0 {
		return nil, false
	}
	obj, ok := value.(map[string]any)
	return obj, ok
}

// intClaim is the value of an integer claim: a JSON integer literal (not a
// float, not a boolean, not a string) that fits in int64.
func intClaim(v any) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	text := string(n)
	if strings.ContainsAny(text, ".eE") {
		return 0, false
	}
	i, err := strconv.ParseInt(text, 10, 64)
	return i, err == nil
}

func strClaim(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

// KidForKey is the key id Auth publishes for a base64 Ed25519 public key:
// urlsafe base64, unpadded, of the first 16 bytes of its SHA-256.
func KidForKey(encodedKey string) (string, bool) {
	raw, ok := decodeStdBase64Strict(strings.TrimSpace(encodedKey))
	if !ok || len(raw) != ed25519.PublicKeySize {
		return "", false
	}
	return kidForRaw(raw), true
}

func kidForRaw(raw []byte) string {
	sum := sha256.Sum256(raw)
	return base64.RawURLEncoding.EncodeToString(sum[:16])
}

// decodeStdBase64Strict is Python's base64.b64decode(s, validate=True):
// standard alphabet, padding required, nothing else (no whitespace).
func decodeStdBase64Strict(s string) ([]byte, bool) {
	if strings.ContainsAny(s, "\r\n") {
		return nil, false
	}
	out, err := base64.StdEncoding.DecodeString(s)
	return out, err == nil
}

// verifySignedToken returns the claims of an Auth-signed compact JWS once its
// structure, header and Ed25519 signature check out. typ and alg must match
// exactly and kid must name one of publicKeys; callers validate the claims.
func verifySignedToken(raw, typ string, publicKeys []string, msg string) (map[string]any, error) {
	fail := &CentralAuthError{msg: msg}
	parts := strings.Split(raw, ".")
	if len(raw) > maxSignedTokenBytes || len(parts) != 3 {
		return nil, fail
	}
	headerBytes, ok1 := decodeSegment(parts[0])
	claimsBytes, ok2 := decodeSegment(parts[1])
	signature, ok3 := decodeSegment(parts[2])
	if !ok1 || !ok2 || !ok3 {
		return nil, fail
	}
	header, ok1 := decodeJSONObject(headerBytes)
	claims, ok2 := decodeJSONObject(claimsBytes)
	if !ok1 || !ok2 {
		return nil, fail
	}
	if headerTyp, ok := strClaim(header["typ"]); !ok || headerTyp != typ {
		return nil, fail
	}
	if alg, ok := strClaim(header["alg"]); !ok || alg != "EdDSA" {
		return nil, fail
	}
	kid, ok := strClaim(header["kid"])
	if !ok || !kidRE.MatchString(kid) {
		return nil, fail
	}
	signed := []byte(parts[0] + "." + parts[1])
	for _, encoded := range publicKeys {
		public, ok := decodeStdBase64Strict(strings.TrimSpace(encoded))
		// kid names a public key, so plain equality leaks nothing.
		if !ok || len(public) != ed25519.PublicKeySize || kidForRaw(public) != kid {
			continue
		}
		if ed25519.Verify(ed25519.PublicKey(public), signed, signature) {
			return claims, nil
		}
	}
	return nil, fail
}

func timestampOr(now *int64) int64 {
	if now != nil {
		return *now
	}
	return time.Now().Unix()
}

// liveWindow checks exp (live, with skew) and iat (positive, not in the
// future beyond skew).
func liveWindow(claims map[string]any, timestamp int64) (int64, bool) {
	exp, ok := intClaim(claims["exp"])
	if !ok || exp+ClockSkewSeconds <= timestamp {
		return 0, false
	}
	iat, ok := intClaim(claims["iat"])
	if !ok || iat <= 0 || iat > timestamp+ClockSkewSeconds {
		return 0, false
	}
	return iat, true
}

func boundedString(v any) (string, bool) {
	s, ok := strClaim(v)
	return s, ok && s != "" && utf8.RuneCountInString(s) <= 255
}

// VerifyLogoutToken validates a back-channel logout token (typ logout+jwt).
// now is the Unix time to judge exp/iat against; nil means the clock.
func VerifyLogoutToken(raw, issuer, audience string, publicKeys []string, now *int64) (LogoutEvent, error) {
	fail := &CentralAuthError{msg: errLogoutToken}
	claims, err := verifySignedToken(raw, "logout+jwt", publicKeys, errLogoutToken)
	if err != nil {
		return LogoutEvent{}, err
	}
	timestamp := timestampOr(now)
	iat, live := liveWindow(claims, timestamp)
	tokenIssuer, issOK := strClaim(claims["iss"])
	aud, audOK := strClaim(claims["aud"])
	subject, subOK := boundedString(claims["sub"])
	eventID, jtiOK := boundedString(claims["jti"])
	events, _ := claims["events"].(map[string]any)
	_, hasEvent := events[BackchannelLogoutEvent].(map[string]any)
	_, hasNonce := claims["nonce"]
	if !live || !issOK || strings.TrimRight(tokenIssuer, "/") != strings.TrimRight(issuer, "/") ||
		!audOK || aud != audience || !subOK || !jtiOK || !hasEvent || hasNonce {
		return LogoutEvent{}, fail
	}
	return LogoutEvent{EventID: eventID, Subject: subject, Issuer: strings.TrimRight(issuer, "/"), IssuedAt: iat}, nil
}

// VerifyAccessToken validates an application-access provisioning token (typ
// access+jwt).
func VerifyAccessToken(raw, issuer, audience string, publicKeys []string, now *int64) (AccessProvisionEvent, error) {
	fail := &CentralAuthError{msg: errAccessToken}
	claims, err := verifySignedToken(raw, "access+jwt", publicKeys, errAccessToken)
	if err != nil {
		return AccessProvisionEvent{}, err
	}
	timestamp := timestampOr(now)
	events, _ := claims["events"].(map[string]any)
	access, _ := events[ApplicationAccessEvent].(map[string]any)
	rawEmail, ok := strClaim(claims["email"])
	if !ok {
		return AccessProvisionEvent{}, fail
	}
	email, err := NormalizeEmail(rawEmail)
	if err != nil {
		return AccessProvisionEvent{}, fail
	}
	iat, live := liveWindow(claims, timestamp)
	tokenIssuer, issOK := strClaim(claims["iss"])
	aud, audOK := strClaim(claims["aud"])
	subject, subOK := boundedString(claims["sub"])
	eventID, jtiOK := boundedString(claims["jti"])
	version, versionOK := intClaim(access["version"])
	action, _ := strClaim(access["action"])
	_, hasNonce := claims["nonce"]
	if !live || !issOK || strings.TrimRight(tokenIssuer, "/") != strings.TrimRight(issuer, "/") ||
		!audOK || aud != audience || !subOK || !jtiOK || !versionOK || version <= 0 ||
		(action != "grant" && action != "revoke") || hasNonce {
		return AccessProvisionEvent{}, fail
	}
	return AccessProvisionEvent{
		EventID: eventID, Subject: subject, Email: email, Issuer: strings.TrimRight(issuer, "/"),
		IssuedAt: iat, Version: version, Allowed: action == "grant",
	}, nil
}

// VerifyIDToken returns the identity Auth's /token vouches for, read only from
// its signed id_token (Auth's docs/INTEGRATION.md, code exchange): EdDSA under
// a key Auth publishes, exact iss and aud, live exp, and this login's nonce.
// The unsigned copies of the same claims in the /token JSON are not trusted.
func VerifyIDToken(raw, issuer, audience, expectedNonce string, publicKeys []string, now *int64) (AuthenticatedPrincipal, error) {
	fail := &CentralAuthError{msg: errAuthResp}
	claims, err := verifySignedToken(raw, "JWT", publicKeys, errAuthResp)
	if err != nil {
		return AuthenticatedPrincipal{}, err
	}
	timestamp := timestampOr(now)
	subject, subOK := boundedString(claims["sub"])
	email, emailOK := strClaim(claims["email"])
	nonce, nonceOK := strClaim(claims["nonce"])
	tokenIssuer, issOK := strClaim(claims["iss"])
	aud, audOK := strClaim(claims["aud"])
	_, live := liveWindow(claims, timestamp)
	if !subOK || !emailOK || !nonceOK ||
		subtle.ConstantTimeCompare([]byte(nonce), []byte(expectedNonce)) != 1 ||
		// Exact: Auth emits its issuer without a trailing slash, and the
		// configured issuer is canonicalised the same way (origin()).
		!issOK || tokenIssuer != strings.TrimRight(issuer, "/") ||
		!audOK || aud != audience || !live {
		return AuthenticatedPrincipal{}, fail
	}
	normalized, err := NormalizeEmail(email)
	if err != nil {
		return AuthenticatedPrincipal{}, fail
	}
	return AuthenticatedPrincipal{Subject: subject, Email: normalized, Nonce: nonce}, nil
}

// tokenKid is the kid header of a compact JWS, if it has a string one.
func tokenKid(raw string) (string, bool) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return "", false
	}
	headerBytes, ok := decodeSegment(parts[0])
	if !ok {
		return "", false
	}
	header, ok := decodeJSONObject(headerBytes)
	if !ok {
		return "", false
	}
	return strClaim(header["kid"])
}
