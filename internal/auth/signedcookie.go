// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maxSignedCookieBytes: browsers cap a cookie near 4 KB; anything longer was
// not written by us.
const maxSignedCookieBytes = 4096

// SignedCookie is an HMAC-SHA256-signed, expiring cookie holding one JSON
// value: the Go counterpart of the Starlette SessionMiddleware cookie
// (`lens_ui`) the Python release kept the central login transaction in. The
// wire format is ours (not itsdangerous's); an in-flight Python login
// transaction simply restarts after the upgrade.
//
// Value format: base64url(json) "." issued-unix-seconds "." base64url(mac),
// the MAC covering everything before the last dot. Expiry is enforced both by
// the cookie's Max-Age and by the signed issue time.
type SignedCookie struct {
	Name   string
	MaxAge time.Duration
	Secure bool
	key    []byte
	now    func() time.Time
}

// NewSignedCookie derives the signing key from secret (LENS_SESSION_SECRET)
// with a purpose label, so the same secret could sign other things without
// cross-use.
func NewSignedCookie(name string, secret []byte, maxAge time.Duration, secure bool) *SignedCookie {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("lens-signed-cookie-v1:" + name))
	return &SignedCookie{Name: name, MaxAge: maxAge, Secure: secure, key: mac.Sum(nil), now: time.Now}
}

func (c *SignedCookie) sign(body string) string {
	mac := hmac.New(sha256.New, c.key)
	mac.Write([]byte(body))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Set writes value as the cookie (HttpOnly, SameSite=Lax, Path=/, Max-Age).
func (c *SignedCookie) Set(w http.ResponseWriter, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	body := base64.RawURLEncoding.EncodeToString(data) + "." + strconv.FormatInt(c.now().Unix(), 10)
	//nolint:gosec // G124: Secure follows LENS_AUTH_COOKIE_SECURE (off only for plain-HTTP development)
	http.SetCookie(w, &http.Cookie{
		Name:     c.Name,
		Value:    body + "." + c.sign(body),
		Path:     "/",
		MaxAge:   int(c.MaxAge / time.Second),
		Secure:   c.Secure,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	return nil
}

// Get decodes the request's cookie into dst; false when it is absent,
// tampered with, expired or not decodable.
func (c *SignedCookie) Get(r *http.Request, dst any) bool {
	cookie, err := r.Cookie(c.Name)
	if err != nil || cookie.Value == "" || len(cookie.Value) > maxSignedCookieBytes {
		return false
	}
	cut := strings.LastIndexByte(cookie.Value, '.')
	if cut < 0 {
		return false
	}
	body, signature := cookie.Value[:cut], cookie.Value[cut+1:]
	if !hmac.Equal([]byte(c.sign(body)), []byte(signature)) {
		return false
	}
	payload, issuedText, ok := strings.Cut(body, ".")
	if !ok {
		return false
	}
	issued, err := strconv.ParseInt(issuedText, 10, 64)
	if err != nil {
		return false
	}
	age := c.now().Unix() - issued
	if age < 0 || age > int64(c.MaxAge/time.Second) {
		return false
	}
	data, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return false
	}
	return json.Unmarshal(data, dst) == nil
}

// Present reports whether the request carries the cookie at all (valid or
// not).
func (c *SignedCookie) Present(r *http.Request) bool {
	_, err := r.Cookie(c.Name)
	return err == nil
}

// Clear expires the cookie in the browser.
func (c *SignedCookie) Clear(w http.ResponseWriter) {
	//nolint:gosec // G124: see Set
	http.SetCookie(w, &http.Cookie{
		Name:     c.Name,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
		Secure:   c.Secure,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}
