// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package auth

// ErrorKind tells the central-auth failures apart; the callback handler maps
// them to different statuses (400, 403, 502).
type ErrorKind int

// Error kinds. Every kind is also a KindGeneric for errors.Is purposes, the
// way the Python subclasses were all CentralAuthError.
const (
	// KindGeneric: a safe central-auth failure (bad token, Auth unreachable).
	KindGeneric ErrorKind = iota
	// KindAccessDenied: the central identity is not enabled for this Lens.
	KindAccessDenied
	// KindTransaction: the callback does not match a live browser login
	// transaction.
	KindTransaction
	// KindCodeRejected: Auth refused the code (consumed, expired, or
	// superseded) — a retry for the user, not an outage.
	KindCodeRejected
)

// CentralAuthError is a central-auth failure whose message contains no
// credential material and is safe to log. The underlying cause, when there is
// one, is kept for errors.Unwrap but never included in the message.
type CentralAuthError struct {
	Kind     ErrorKind
	msg      string
	cause    error
	sentinel bool
}

func (e *CentralAuthError) Error() string { return e.msg }

// Unwrap returns the underlying cause (an I/O error, say), if any.
func (e *CentralAuthError) Unwrap() error { return e.cause }

// Is makes errors.Is(err, ErrAccessDenied) and friends match by kind, and
// errors.Is(err, ErrCentralAuth) match every central-auth error.
func (e *CentralAuthError) Is(target error) bool {
	t, ok := target.(*CentralAuthError)
	if !ok || !t.sentinel {
		return false
	}
	return t.Kind == KindGeneric || t.Kind == e.Kind
}

// Sentinels for errors.Is.
var (
	ErrCentralAuth          = &CentralAuthError{Kind: KindGeneric, msg: "central auth error", sentinel: true}
	ErrAccessDenied         = &CentralAuthError{Kind: KindAccessDenied, msg: "access denied", sentinel: true}
	ErrTransaction          = &CentralAuthError{Kind: KindTransaction, msg: "login transaction error", sentinel: true}
	ErrCodeExchangeRejected = &CentralAuthError{Kind: KindCodeRejected, msg: "code exchange rejected", sentinel: true}
)

func newError(kind ErrorKind, msg string, cause error) *CentralAuthError {
	return &CentralAuthError{Kind: kind, msg: msg, cause: cause}
}
