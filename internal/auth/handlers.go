// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/ElcanoTek/lens/internal/logx"
)

var logger = logx.New("central_auth")

// MaxBackchannelBodyBytes caps /auth/backchannel-logout bodies. A logout or
// access token is at most 16 KB; anything bigger is refused before parsing.
const MaxBackchannelBodyBytes = 20_000

// maxLogoutFormBytes bounds the /logout form (one CSRF token).
const maxLogoutFormBytes = 64 * 1024

// Handlers are the browser-auth routes, ready to mount:
//
//	GET  /login, /auth/login   -> Login
//	GET  /auth/callback        -> Callback
//	POST /auth/backchannel-logout -> BackchannelLogout
//	POST /logout               -> Logout
//	GET  /signed-out           -> SignedOut
//
// Each handler resolves the provider per request through Provider, so a
// ProviderCache's Get (or a closure over a provider built at startup) plugs
// straight in.
type Handlers struct {
	// Provider returns the active provider. Required.
	Provider func(ctx context.Context) (Provider, error)
	// RenderSignedOut writes the signed-out page (templates/signed_out.html)
	// with the given account URL. Required for SignedOut.
	RenderSignedOut func(w http.ResponseWriter, r *http.Request, accountURL string)
	// Error writes an error response. nil means FastAPI's shape:
	// {"detail": "..."} as JSON with the status.
	Error func(w http.ResponseWriter, r *http.Request, status int, detail string)
}

func (h *Handlers) fail(w http.ResponseWriter, r *http.Request, status int, detail string) {
	if h.Error != nil {
		h.Error(w, r, status, detail)
		return
	}
	WriteJSONError(w, status, detail)
}

// WriteJSONError writes {"detail": detail} with status, FastAPI's
// HTTPException body.
func WriteJSONError(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"detail": detail})
}

func (h *Handlers) provider(w http.ResponseWriter, r *http.Request) (Provider, bool) {
	p, err := h.Provider(r.Context())
	if err != nil {
		// A misconfiguration: the message names the setting, never a value.
		logger.Error("authentication provider unavailable", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return nil, false
	}
	return p, true
}

// Login is GET /login and /auth/login: already signed in goes home; central
// mode starts a login transaction toward ?next=; elcano mode redirects to the
// auth service.
func (h *Handlers) Login(w http.ResponseWriter, r *http.Request) {
	p, ok := h.provider(w, r)
	if !ok {
		return
	}
	if p.Identity(r) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if central, ok := p.(*CentralProvider); ok {
		next := r.URL.Query().Get("next")
		if next == "" {
			next = "/"
		}
		central.BeginLogin(w, r, next)
		return
	}
	p.UnauthenticatedResponse(w, r)
}

// Callback is GET /auth/callback. A stale or mismatched transaction or a
// rejected (consumed, superseded) code is 400 "Sign-in expired. Try again.";
// an account without Lens access is 403; anything else Auth-side is 502.
func (h *Handlers) Callback(w http.ResponseWriter, r *http.Request) {
	p, ok := h.provider(w, r)
	if !ok {
		return
	}
	central, ok := p.(*CentralProvider)
	if !ok {
		h.fail(w, r, http.StatusNotFound, "Not Found")
		return
	}
	query := r.URL.Query()
	code, state := query.Get("code"), query.Get("state")
	if query.Get("error") != "" || code == "" || state == "" {
		if central.Transactions.Present(r) {
			central.Transactions.Clear(w)
		}
		h.fail(w, r, http.StatusBadRequest, "Sign-in was not completed")
		return
	}
	token, destination, err := central.CompleteLogin(r.Context(), w, r, code, state)
	if err != nil {
		var authErr *CentralAuthError
		switch {
		case errors.Is(err, ErrTransaction), errors.Is(err, ErrCodeExchangeRejected):
			h.fail(w, r, http.StatusBadRequest, "Sign-in expired. Try again.")
		case errors.Is(err, ErrAccessDenied):
			h.fail(w, r, http.StatusForbidden, "Your account does not have Lens access")
		case errors.As(err, &authErr):
			logger.Warn("central sign-in failed", "reason", authErr.Error())
			h.fail(w, r, http.StatusBadGateway, "The authentication service is unavailable")
		default:
			logger.Error("central sign-in failed", "error", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
		return
	}
	w.Header().Set("Referrer-Policy", "no-referrer")
	central.SetSessionCookie(w, token)
	http.Redirect(w, r, destination, http.StatusSeeOther) //nolint:gosec // G710: destination went through SafeLocalPath (same-origin path only)
}

// CheckBackchannelBody applies the back-channel size rules before anything
// reads the body: no Content-Length is 411, a non-decimal one 400, more than
// MaxBackchannelBodyBytes 413. It writes the response and returns false when
// the request is refused. BackchannelLogout calls it itself; it is exported
// for a server that wants the check in middleware, as the Python had.
func CheckBackchannelBody(w http.ResponseWriter, r *http.Request) bool {
	values, present := r.Header["Content-Length"]
	if !present || len(values) == 0 {
		w.WriteHeader(http.StatusLengthRequired)
		return false
	}
	raw := values[0]
	if raw == "" || strings.TrimLeft(raw, "0123456789") != "" {
		w.WriteHeader(http.StatusBadRequest)
		return false
	}
	digits := strings.TrimLeft(raw, "0")
	if n, err := strconv.Atoi("0" + digits); len(digits) > 6 || err != nil || n > MaxBackchannelBodyBytes {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxBackchannelBodyBytes)
	return true
}

// BackchannelLogout is POST /auth/backchannel-logout: Auth's signed
// back-channel, carrying exactly one of logout_token (end every session of
// a subject) or access_token (grant/revoke this deployment's access). 204 on
// success (including a replay), 400 for anything unverifiable.
func (h *Handlers) BackchannelLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		h.fail(w, r, http.StatusMethodNotAllowed, "Method Not Allowed")
		return
	}
	if !CheckBackchannelBody(w, r) {
		return
	}
	p, ok := h.provider(w, r)
	if !ok {
		return
	}
	central, ok := p.(*CentralProvider)
	if !ok {
		h.fail(w, r, http.StatusNotFound, "Not Found")
		return
	}
	//nolint:gosec // G120: CheckBackchannelBody wrapped the body in http.MaxBytesReader above
	if err := r.ParseMultipartForm(MaxBackchannelBodyBytes); err != nil && !errors.Is(err, http.ErrNotMultipart) {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		h.fail(w, r, http.StatusBadRequest, "Invalid back-channel request")
		return
	}
	logoutToken, accessToken := r.PostForm.Get("logout_token"), r.PostForm.Get("access_token")
	if (logoutToken != "") == (accessToken != "") {
		h.fail(w, r, http.StatusBadRequest, "Invalid back-channel request")
		return
	}
	ctx := r.Context()
	issuer, audience := central.Client.IssuerURL, central.Client.ClientID
	if accessToken != "" {
		event, err := VerifyAccessToken(accessToken, issuer, audience, central.KeyResolver.KeysForToken(ctx, accessToken), nil)
		if err != nil {
			h.fail(w, r, http.StatusBadRequest, "Invalid access token")
			return
		}
		if _, err := central.Store.ApplyAccessProvisioning(ctx, event); err != nil {
			logger.Error("could not apply access provisioning", "error", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	event, err := VerifyLogoutToken(logoutToken, issuer, audience, central.KeyResolver.KeysForToken(ctx, logoutToken), nil)
	if err != nil {
		h.fail(w, r, http.StatusBadRequest, "Invalid logout token")
		return
	}
	if _, err := central.Store.ConsumeLogoutEvent(ctx, event); err != nil {
		logger.Error("could not record back-channel logout", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Logout is POST /logout. Elcano mode delegates to the auth service's logout.
// Central mode checks the CSRF token (403 otherwise), revokes the Lens
// session, clears the cookies and hands the browser to Auth's RP-initiated
// logout, which ends the central session and fans a back-channel logout out
// to every registered application (this one included, harmlessly); without
// that, a still-valid Auth cookie would silently sign the user straight back
// in. /signed-out remains for direct visits.
func (h *Handlers) Logout(w http.ResponseWriter, r *http.Request) {
	p, ok := h.provider(w, r)
	if !ok {
		return
	}
	switch prov := p.(type) {
	case *ElcanoProvider:
		http.Redirect(w, r, prov.LogoutURL(), http.StatusSeeOther) //nolint:gosec // G710: the configured AUTH_LOGIN_URL, not request input
	case *CentralProvider:
		r.Body = http.MaxBytesReader(w, r.Body, maxLogoutFormBytes)
		//nolint:gosec // G120: the body is wrapped in http.MaxBytesReader on the line above
		if err := r.ParseMultipartForm(maxLogoutFormBytes); err != nil && !errors.Is(err, http.ErrNotMultipart) {
			h.fail(w, r, http.StatusForbidden, "Forbidden")
			return
		}
		if !prov.ValidCSRF(r, r.PostForm.Get("csrf_token")) {
			h.fail(w, r, http.StatusForbidden, "Forbidden")
			return
		}
		if _, err := prov.Store.RevokeSession(r.Context(), prov.sessionToken(r)); err != nil {
			logger.Error("could not revoke session at logout", "error", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		if prov.Transactions.Present(r) {
			prov.Transactions.Clear(w)
		}
		prov.ClearSessionCookie(w)
		http.Redirect(w, r, prov.LogoutURL(), http.StatusSeeOther) //nolint:gosec // G710: the configured Auth origin, not request input
	default:
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

// SignedOut is GET /signed-out: deliberately public and identity-free.
// Redirecting back to / would start central SSO immediately while the Auth
// cookie is still valid, making the local logout appear ineffective. The
// page therefore also points at the central account page, which is the one
// place the Auth session itself can be ended.
func (h *Handlers) SignedOut(w http.ResponseWriter, r *http.Request) {
	p, ok := h.provider(w, r)
	if !ok {
		return
	}
	h.RenderSignedOut(w, r, AccountURL(p))
}
