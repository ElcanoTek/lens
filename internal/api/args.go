// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package api

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

// Arguments arrive loosely typed: REST query values are strings, JSON and
// MCP values are whatever the caller sent. These helpers apply one set of
// rules to both (the Python release's), so a bad value gets the documented
// invalid_argument body whichever surface it came through.

// truthy is Python truthiness for decoded JSON values.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0
	case int:
		return x != 0
	case json.Number:
		f, err := x.Float64()
		return err != nil || f != 0
	case []any:
		return len(x) > 0
	case []string:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// pyStr is Python's str() for decoded JSON scalars.
func pyStr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case string:
		return x
	case bool:
		if x {
			return "True"
		}
		return "False"
	case float64:
		// Decoded JSON integers are float64 here; render them as the
		// integers the caller sent.
		if x == math.Trunc(x) && math.Abs(x) < 1e16 {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'g', -1, 64)
	case int:
		return strconv.Itoa(x)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(raw)
}

// strOrEmpty is str(value or "") : falsy values become "".
func strOrEmpty(v any) string {
	if !truthy(v) {
		return ""
	}
	return pyStr(v)
}

// pyRepr renders a string the way Python's repr() does (quotes chosen the
// same way), for messages that quote the caller's value.
func pyRepr(s string) string {
	quote := "'"
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		quote = `"`
	}
	var b strings.Builder
	b.WriteString(quote)
	for _, r := range s {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case string(r) == quote:
			b.WriteString(`\` + quote)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			b.WriteString(`\x` + strconv.FormatInt(int64(r)|0x100, 16)[1:])
		default:
			b.WriteRune(r)
		}
	}
	b.WriteString(quote)
	return b.String()
}

// pyList renders a list of strings like Python's str(list).
func pyList(items []string) string {
	parts := make([]string, len(items))
	for i, s := range items {
		parts[i] = pyRepr(s)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// boundedInt parses an integer option: absent or "" gives def; a bool, a
// non-number or a value outside [low, high] is refused.
func boundedInt(value any, name string, def, low, high int) (int, error) {
	var n int
	switch x := value.(type) {
	case nil:
		return def, nil
	case string:
		if x == "" {
			return def, nil
		}
		i, err := strconv.Atoi(strings.TrimSpace(x))
		if err != nil {
			return 0, invalidArg("%s must be an integer", name)
		}
		n = i
	case bool:
		return 0, invalidArg("%s must be an integer", name)
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return 0, invalidArg("%s must be an integer", name)
		}
		n = int(x) // int() truncates, as Python's int(5.7) does
	case int:
		n = x
	case json.Number:
		if i, err := x.Int64(); err == nil {
			n = int(i)
		} else if f, err := x.Float64(); err == nil {
			n = int(f)
		} else {
			return 0, invalidArg("%s must be an integer", name)
		}
	default:
		return 0, invalidArg("%s must be an integer", name)
	}
	if n < low || n > high {
		return 0, invalidArg("%s must be between %d and %d", name, low, high)
	}
	return n, nil
}

// optionalBool parses a boolean option: absent gives def; true/false and
// the strings true/false/on/off/1/0 are accepted.
func optionalBool(value any, name string, def bool) (bool, error) {
	switch x := value.(type) {
	case nil:
		return def, nil
	case bool:
		return x, nil
	case string:
		switch strings.ToLower(x) {
		case "true", "on", "1":
			return true, nil
		case "false", "off", "0":
			return false, nil
		}
	}
	return false, invalidArg("%s must be true or false", name)
}
