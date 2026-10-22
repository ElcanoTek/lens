// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"strings"
)

// maxBackchannelBodyBytes caps a back-channel logout/provisioning POST.
const maxBackchannelBodyBytes = 20_000

// CSPNonPage is the closed policy every non-HTML response carries.
//
// Pages carry one inline <script> (the theme bootstrap) which gets a
// per-response nonce; everything else loads from this origin. Scripts are
// the strict part: no inline handlers, no eval, no other origins, so an
// injected <script> or onclick cannot run. Styles are 'self' only: the
// progress bars carry their widths as data-pct attributes and main.js
// applies them through the CSSOM (element.style.width), which CSP permits;
// there are no style attributes in the markup. No form-action: Elcano-mode
// logout is a form POST that redirects to the auth host, and browsers apply
// form-action to that redirect. frame-ancestors 'none' mirrors
// X-Frame-Options: DENY.
const CSPNonPage = "default-src 'none'; base-uri 'none'; frame-ancestors 'none'"

func cspForPage(nonce string) string {
	return "default-src 'self'; script-src 'self' 'nonce-" + nonce + "'; " +
		"style-src 'self'; img-src 'self' data:; font-src 'self'; " +
		"connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'"
}

type ctxKey int

const (
	nonceKey ctxKey = iota
	clientIPKey
)

// Nonce returns this response's CSP nonce.
func Nonce(r *http.Request) string {
	s, _ := r.Context().Value(nonceKey).(string)
	return s
}

// ClientIP is the browser's address: X-Forwarded-For when the request came
// through the local reverse proxy, else the peer.
func ClientIP(r *http.Request) string {
	if s, ok := r.Context().Value(clientIPKey).(string); ok {
		return s
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func newNonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// trustedProxy mirrors uvicorn's --forwarded-allow-ips=127.0.0.1: only the
// local reverse proxy (Caddy/nginx on the same host) may say which scheme
// and client the browser used.
func trustedProxy(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	return err == nil && host == "127.0.0.1"
}

// proxyHeaders fills r.URL.Scheme and r.URL.Host the way Starlette's
// request.url saw them behind uvicorn --proxy-headers, so the Elcano login
// redirect's return_to and public base URLs name the https origin the
// browser used, not the loopback hop. RemoteAddr is left alone (the auth
// package checks it for the same trust decision); the forwarded client is
// available through ClientIP.
func proxyHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		ctx := r.Context()
		if trustedProxy(r) {
			if proto := lastToken(r.Header.Get("X-Forwarded-Proto")); proto != "" {
				switch strings.ToLower(proto) {
				case "http", "https":
					scheme = strings.ToLower(proto)
				}
			}
			if client := lastToken(r.Header.Get("X-Forwarded-For")); client != "" {
				ctx = context.WithValue(ctx, clientIPKey, client)
			}
		}
		r = r.WithContext(ctx)
		r.URL.Scheme = scheme
		r.URL.Host = r.Host
		next.ServeHTTP(w, r)
	})
}

func lastToken(header string) string {
	parts := strings.Split(header, ",")
	return strings.TrimSpace(parts[len(parts)-1])
}

// hookWriter runs before() once, just before the status line goes out, so
// header policy can depend on what the handler set (Content-Type) and on the
// status code.
type hookWriter struct {
	http.ResponseWriter
	before func(h http.Header, status int)
	wrote  bool
}

func (w *hookWriter) WriteHeader(code int) {
	if code >= 100 && code < 200 && code != http.StatusSwitchingProtocols {
		w.ResponseWriter.WriteHeader(code)
		return
	}
	if !w.wrote {
		w.wrote = true
		w.before(w.Header(), code)
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *hookWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

func (w *hookWriter) Flush() {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the real writer.
func (w *hookWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// headerPolicy sets the security headers, the CSP (nonce'd for pages) and
// the cache policy on every response.
//
// Cache policy, backing the versioned asset URLs:
//   - versioned /static URLs (/static/<content-hash>/…, or the older
//     ?v=<content-hash>) at the current version that resolve (200, a 206
//     range, or 304 on revalidation): immutable, cached for a year; a deploy
//     changes the hash, which is a brand-new URL. An error on a versioned URL
//     (404 for a missing asset, 405) is never immutable — it gets no-cache
//     like any unversioned asset, so a fixed asset or a rollback is picked up.
//   - unversioned /static URLs, and ones naming another (older) version,
//     which serve the current bytes: always revalidate (defends direct
//     links and pages cached before a deploy)
//   - everything else (HTML, API): never stored, so the page that carries
//     the asset versions is itself always fresh
func headerPolicy(staticVersion string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nonce := newNonce()
		r = r.WithContext(context.WithValue(r.Context(), nonceKey, nonce))
		static := strings.HasPrefix(r.URL.Path, "/static")
		versioned := false
		if static {
			if v, _ := splitStaticVersion(strings.TrimPrefix(r.URL.Path, "/static/")); v == staticVersion {
				versioned = true
			} else if v := r.URL.Query()["v"]; len(v) > 0 && v[len(v)-1] == staticVersion {
				versioned = true
			}
		}
		hw := &hookWriter{ResponseWriter: w, before: func(h http.Header, status int) {
			if strings.HasPrefix(h.Get("Content-Type"), "text/html") {
				h.Set("Content-Security-Policy", cspForPage(nonce))
			} else if h.Get("Content-Security-Policy") == "" {
				h.Set("Content-Security-Policy", CSPNonPage)
			}
			setDefault(h, "X-Content-Type-Options", "nosniff")
			setDefault(h, "X-Frame-Options", "DENY")
			setDefault(h, "Referrer-Policy", "strict-origin-when-cross-origin")
			switch {
			case versioned && (status == http.StatusOK || status == http.StatusPartialContent || status == http.StatusNotModified):
				h.Set("Cache-Control", "public, max-age=31536000, immutable")
			case static:
				h.Set("Cache-Control", "no-cache")
			default:
				h.Set("Cache-Control", "no-store")
			}
		}}
		next.ServeHTTP(hw, r)
		if !hw.wrote {
			hw.WriteHeader(http.StatusOK)
		}
	})
}

func setDefault(h http.Header, key, value string) {
	if h.Get(key) == "" {
		h.Set(key, value)
	}
}

// limitBackchannelBody rejects back-channel posts without a small, declared
// body before any parsing happens: Auth always sends a Content-Length, and
// an unbounded body would otherwise be read into memory by the form parser.
func limitBackchannelBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/backchannel-logout" && r.Method == http.MethodPost {
			raw, ok := r.Header["Content-Length"]
			if !ok || len(raw) == 0 {
				w.WriteHeader(http.StatusLengthRequired)
				return
			}
			if !isDecimal(raw[0]) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if len(strings.TrimLeft(raw[0], "0")) > 6 || r.ContentLength > maxBackchannelBodyBytes {
				w.WriteHeader(http.StatusRequestEntityTooLarge)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, maxBackchannelBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}

func isDecimal(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// writeJSON writes a FastAPI-style JSON response (compact, UTF-8, HTML
// characters left alone).
func writeJSON(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
}

// writeDetail is FastAPI's HTTPException response: {"detail": ...}, with
// the status phrase when no detail is given.
func writeDetail(w http.ResponseWriter, status int, detail string) {
	if detail == "" {
		detail = http.StatusText(status)
	}
	writeJSON(w, status, map[string]string{"detail": detail})
}

// seeOther is Starlette's RedirectResponse(url, 303): an empty body and a
// Location header.
func seeOther(w http.ResponseWriter, location string) {
	w.Header().Set("Location", location)
	w.WriteHeader(http.StatusSeeOther)
}
