// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package auth

import (
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/ElcanoTek/lens/internal/pystr"
)

// ErrInvalidEmail is returned by NormalizeEmail for anything that is not a
// plausible single address. Its message is what operators see from
// `lens access`.
var ErrInvalidEmail = errors.New("A valid email address is required") //nolint:staticcheck // operator-facing sentence, kept verbatim from the Python release

// ErrEmailControlChars rejects addresses carrying control, format, private
// use or unassigned code points.
var ErrEmailControlChars = errors.New("Email contains unsupported control characters") //nolint:staticcheck // operator-facing sentence, kept verbatim from the Python release

// NormalizeEmail canonicalises an address the way the Python release did
// (NFC, strip, casefold) and rejects anything that is not one plausible
// address, so allowlist entries written by either release compare equal.
func NormalizeEmail(email string) (string, error) {
	if !utf8.ValidString(email) {
		return "", ErrInvalidEmail
	}
	normalized := pystr.CaseFold(pystr.Strip(pystr.NFC(email)))
	if normalized == "" || utf8.RuneCountInString(normalized) > 254 || strings.Count(normalized, "@") != 1 {
		return "", ErrInvalidEmail
	}
	local, domain, _ := strings.Cut(normalized, "@")
	if local == "" || domain == "" || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
		return "", ErrInvalidEmail
	}
	for _, r := range normalized {
		if pystr.IsControl(r) {
			return "", ErrEmailControlChars
		}
	}
	return normalized, nil
}
