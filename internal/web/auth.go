// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package web

import (
	"context"
	"net/http"

	"github.com/ElcanoTek/lens/internal/auth"
)

// AuthProvider is the browser sign-in seam. Lens has two modes: the legacy
// shared elcano_auth cookie ("elcano", the default) and central Auth's
// one-use application handoff ("central"). The dashboard routes only talk to
// this interface; NewAuthProvider adapts internal/auth's provider and route
// handlers to it, and tests substitute fakes.
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
	// Logout handles POST /logout.
	Logout(w http.ResponseWriter, r *http.Request)
	// Login handles GET /login and /auth/login (?next= is the local path
	// to return to).
	Login(w http.ResponseWriter, r *http.Request)
	// Callback handles GET /auth/callback (404 outside central mode).
	Callback(w http.ResponseWriter, r *http.Request)
	// BackchannelLogout handles POST /auth/backchannel-logout (404 outside
	// central mode). The body-size guard has already run.
	BackchannelLogout(w http.ResponseWriter, r *http.Request)
}

// ProviderFromEnv builds (and so validates: credentials, access database)
// the provider LENS_AUTH_MODE selects.
func ProviderFromEnv(ctx context.Context, getenv func(string) (string, bool)) (AuthProvider, error) {
	p, err := auth.NewProviderFromEnv(ctx, getenv)
	if err != nil {
		return nil, err
	}
	return NewAuthProvider(p), nil
}

// NewAuthProvider adapts an internal/auth provider, using auth.Handlers for
// the sign-in routes.
func NewAuthProvider(p auth.Provider) AuthProvider {
	a := &authAdapter{p: p}
	a.h = &auth.Handlers{
		Provider: func(context.Context) (auth.Provider, error) { return p, nil },
		Error: func(w http.ResponseWriter, _ *http.Request, status int, detail string) {
			writeDetail(w, status, detail)
		},
	}
	return a
}

type authAdapter struct {
	p auth.Provider
	h *auth.Handlers
}

func (a *authAdapter) Mode() string                       { return a.p.Mode() }
func (a *authAdapter) Authenticated(r *http.Request) bool { return a.p.Identity(r) != nil }
func (a *authAdapter) Unauthenticated(w http.ResponseWriter, r *http.Request) {
	a.p.UnauthenticatedResponse(w, r)
}

func (a *authAdapter) CSRFToken(r *http.Request) string {
	if c, ok := a.p.(*auth.CentralProvider); ok {
		return c.CSRFToken(r)
	}
	return ""
}

func (a *authAdapter) AccountURL() string                              { return auth.AccountURL(a.p) }
func (a *authAdapter) Logout(w http.ResponseWriter, r *http.Request)   { a.h.Logout(w, r) }
func (a *authAdapter) Login(w http.ResponseWriter, r *http.Request)    { a.h.Login(w, r) }
func (a *authAdapter) Callback(w http.ResponseWriter, r *http.Request) { a.h.Callback(w, r) }
func (a *authAdapter) BackchannelLogout(w http.ResponseWriter, r *http.Request) {
	a.h.BackchannelLogout(w, r)
}
