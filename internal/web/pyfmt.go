// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package web

import (
	"strconv"
	"strings"

	"github.com/ElcanoTek/lens/internal/categories/pyjson"
)

// The dashboard markup used to be rendered by Jinja2 from Python values, and
// main.js plus the operators' muscle memory depend on how those values
// printed. These helpers reproduce the few Python formatting rules the page
// relies on, so the Go-rendered page is byte-for-byte the page Python sent.

// pyFloat is str(float): "60.0", "33.33", "1e-05".
func pyFloat(f float64) string { return pyjson.FormatFloat(f) }

// pyRound is Python's round(x, n) for floats: correctly rounded from the
// exact binary value, which is what strconv's fixed formatting does too.
func pyRound(x float64, n int) float64 {
	f, err := strconv.ParseFloat(strconv.FormatFloat(x, 'f', n, 64), 64)
	if err != nil {
		return x
	}
	return f
}

// pyQuote is urllib.parse.quote(s, safe=safe).
func pyQuote(s, safe string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			strings.IndexByte("_.-~", c) >= 0 || (c < 0x80 && strings.IndexByte(safe, c) >= 0) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&15])
	}
	return b.String()
}

// pyQuotePlus is urllib.parse.quote_plus (spaces become "+").
func pyQuotePlus(s string) string {
	return strings.ReplaceAll(pyQuote(s, " "), " ", "+")
}

// param is one query parameter; redirects keep the order Python's dict did.
type param struct{ key, value string }

// urlencode is urllib.parse.urlencode over ordered pairs.
func urlencode(params []param) string {
	parts := make([]string, 0, len(params))
	for _, p := range params {
		parts = append(parts, pyQuotePlus(p.key)+"="+pyQuotePlus(p.value))
	}
	return strings.Join(parts, "&")
}

// htmlSafeJSON is Jinja's `| tojson`: json.dumps(obj, sort_keys=True) with
// the HTML-significant characters escaped, safe inside any attribute quote.
func htmlSafeJSON(v any) string {
	return strings.NewReplacer(
		"<", `<`,
		">", `>`,
		"&", `&`,
		"'", `'`,
	).Replace(pyjson.Dumps(v))
}

// pyTruthy is Python's bool() for a decoded JSON value.
func pyTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case float64:
		return t != 0
	case string:
		return t != ""
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	}
	return true
}

// pyStr is str() for a decoded JSON scalar, used where Python did
// str(request.get(...)).
func pyStr(v any) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case string:
		return t
	case bool:
		if t {
			return "True"
		}
		return "False"
	case float64:
		if t == float64(int64(t)) && t < 1e16 && t > -1e16 {
			return strconv.FormatInt(int64(t), 10)
		}
		return pyFloat(t)
	}
	return pyjson.Dumps(v)
}
