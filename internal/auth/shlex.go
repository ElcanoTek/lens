// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package auth

import (
	"errors"
	"strings"
)

// shlexSplit is Python's shlex.split(s, comments=True, posix=True): POSIX
// shell word splitting with '…' and "…" quoting, backslash escapes, and '#'
// comments (which, as in Python's shlex, start even mid-word).
func shlexSplit(s string) ([]string, error) {
	const (
		stateSpace = iota
		stateWord
		stateSingle
		stateDouble
		stateEscape
	)
	var (
		tokens  []string
		token   strings.Builder
		quoted  bool
		state   = stateSpace
		escaped int // the state an escape returns to
	)
	isSpace := func(c rune) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\n' }
	flush := func() {
		if token.Len() > 0 || quoted {
			tokens = append(tokens, token.String())
		}
		token.Reset()
		quoted = false
	}
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch state {
		case stateSpace, stateWord:
			switch {
			case isSpace(c):
				if state == stateWord {
					flush()
				}
				state = stateSpace
			case c == '#':
				// Comment to end of line.
				for i < len(runes) && runes[i] != '\n' {
					i++
				}
				flush()
				state = stateSpace
			case c == '\'':
				state, quoted = stateSingle, true
			case c == '"':
				state, quoted = stateDouble, true
			case c == '\\':
				escaped, state = stateWord, stateEscape
			default:
				token.WriteRune(c)
				state = stateWord
			}
		case stateSingle:
			if c == '\'' {
				state = stateWord
			} else {
				token.WriteRune(c)
			}
		case stateDouble:
			switch c {
			case '"':
				state = stateWord
			case '\\':
				escaped, state = stateDouble, stateEscape
			default:
				token.WriteRune(c)
			}
		case stateEscape:
			// Inside double quotes a backslash escapes only " and itself;
			// before anything else it stays literal.
			if escaped == stateDouble && c != '"' && c != '\\' {
				token.WriteRune('\\')
			}
			token.WriteRune(c)
			state = escaped
		}
	}
	switch state {
	case stateSingle, stateDouble:
		return nil, errors.New("No closing quotation") //nolint:staticcheck // Python's shlex message
	case stateEscape:
		return nil, errors.New("No escaped character") //nolint:staticcheck // Python's shlex message
	}
	flush()
	return tokens, nil
}
