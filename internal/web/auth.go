// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package web

import (
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/ElcanoTek/lens/internal/auth"
)

// AuthProvider is the browser sign-in seam. Lens has two: the legacy shared
// elcano_auth cookie ("elcano", the default) and central Auth's one-use
// application handoff ("central"). The dashboard routes only ever talk to
// this interface, so the provider that is configured is the one and only
// source of truth for "is this browser signed in".
//
// The API (/api/v1) and MCP (/mcp) surfaces never use it: programmatic
// callers authenticate with scoped bearer tokens instead.
type AuthProvider interface {
	// Mode is "elcano" or "central"; /health reports it.
	Mode() string
	// Authenticated reports whether the request carries a valid session.
	Authenticated(r *http.Request) bool
	// Unauthenticated answers a signed-out browser on a page route
	// (a 303 into the sign-in flow).
	Unauthenticated(w http.ResponseWriter, r *http.Request)
	// CSRFToken is the token the page's logout form posts; "" when the mode
	// needs none (the elcano logout is a plain redirect to the auth host).
	CSRFToken(r *http.Request) string
	// AccountURL is where /signed-out sends people to manage the session
	// that outlives a local logout.
	AccountURL() string
	// Logout handles POST /logout (the form is already parsed).
	Logout(w http.ResponseWriter, r *http.Request)
	// BeginLogin starts a sign-in for an anonymous browser at /login and
	// /auth/login; next is the local path to return to ("/" by default).
	BeginLogin(w http.ResponseWriter, r *http.Request, next string)
	// Callback handles GET /auth/callback (404 outside central mode).
	Callback(w http.ResponseWriter, r *http.Request)
	// BackchannelLogout handles POST /auth/backchannel-logout (404 outside
	// central mode). The body-size guard runs before it.
	BackchannelLogout(w http.ResponseWriter, r *http.Request)
}

// CentralProviderFactory builds the central-auth provider from the
// environment. internal/auth's central provider is wired here by the binary
// (it validates every credential and opens the access database, so a broken
// deployment fails at startup instead of returning 200 until the first user
// arrives).
var CentralProviderFactory func(getenv func(string) (string, bool)) (AuthProvider, error)

// ProviderFromEnv returns the provider LENS_AUTH_MODE selects.
func ProviderFromEnv(getenv func(string) (string, bool)) (AuthProvider, error) {
	if getenv == nil {
		getenv = os.LookupEnv
	}
	mode, _ := getenv("LENS_AUTH_MODE")
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = "elcano"
	}
	switch mode {
	case "elcano":
		return NewElcanoProvider(auth.NewCookieVerifierFromEnv(getenv)), nil
	case "central":
		if CentralProviderFactory == nil {
			return nil, errors.New("central auth is not available in this build")
		}
		return CentralProviderFactory(getenv)
	}
	return nil, errors.New("LENS_AUTH_MODE must be either 'elcano' or 'central'")
}

// ElcanoProvider is the unchanged shared magic-link cookie path: Lens owns
// no login, verifies the elcano_auth cookie with Auth's public key and
// bounces anonymous browsers to the auth host.
type ElcanoProvider struct {
	Verifier *auth.CookieVerifier
}

// NewElcanoProvider wraps a cookie verifier.
func NewElcanoProvider(v *auth.CookieVerifier) *ElcanoProvider { return &ElcanoProvider{Verifier: v} }

// Mode implements AuthProvider.
func (p *ElcanoProvider) Mode() string { return "elcano" }

// Authenticated implements AuthProvider.
func (p *ElcanoProvider) Authenticated(r *http.Request) bool {
	return p.Verifier.CurrentIdentity(r) != nil
}

// Unauthenticated implements AuthProvider.
func (p *ElcanoProvider) Unauthenticated(w http.ResponseWriter, r *http.Request) {
	p.Verifier.LoginRedirect(w, r)
}

// CSRFToken implements AuthProvider.
func (p *ElcanoProvider) CSRFToken(*http.Request) string { return "" }

// AccountURL implements AuthProvider.
func (p *ElcanoProvider) AccountURL() string { return p.Verifier.LoginURL }

// Logout implements AuthProvider: the auth host owns the session.
func (p *ElcanoProvider) Logout(w http.ResponseWriter, _ *http.Request) {
	seeOther(w, p.Verifier.LoginURL+"/logout")
}

// BeginLogin implements AuthProvider.
func (p *ElcanoProvider) BeginLogin(w http.ResponseWriter, r *http.Request, _ string) {
	p.Unauthenticated(w, r)
}

// Callback implements AuthProvider.
func (p *ElcanoProvider) Callback(w http.ResponseWriter, _ *http.Request) {
	writeDetail(w, http.StatusNotFound, "")
}

// BackchannelLogout implements AuthProvider.
func (p *ElcanoProvider) BackchannelLogout(w http.ResponseWriter, _ *http.Request) {
	writeDetail(w, http.StatusNotFound, "")
}
