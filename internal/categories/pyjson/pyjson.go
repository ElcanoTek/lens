// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package pyjson reads and writes JSON exactly the way Python's json module
// does, for the few places where Lens's output bytes were produced by
// json.dumps and must not change across the Go rewrite: the
// Decision_Answers CSV cell, the custom-category research instructions, the
// Decisions request body and the <output>.categories.json sidecar.
//
// The differences from encoding/json that matter:
//
//   - objects keep their key order (Python dicts are insertion ordered), so
//     an [Object] is an ordered list of members, and [Decode] returns one for
//     every JSON object;
//   - numbers keep Python's int/float split: an integer literal decodes to
//     int64 and anything with a fraction or exponent to float64, and floats
//     are written with Python's repr ("1.0", "0.73", "1e-05");
//   - the default separators are ", " and ": ";
//   - ensure_ascii escapes every non-ASCII character as \uXXXX (lowercase
//     hex, surrogate pairs above the BMP); with it off only control
//     characters, quotes and backslashes are escaped. HTML characters and
//     U+2028/U+2029 are never escaped (encoding/json escapes all of them).
package pyjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Member is one key/value pair of an [Object].
type Member struct {
	Key   string
	Value any
}

// Object is a JSON object that remembers its key order.
type Object []Member

// Get returns the value stored under key.
func (o Object) Get(key string) (any, bool) {
	for _, m := range o {
		if m.Key == key {
			return m.Value, true
		}
	}
	return nil, false
}

// Keys returns the keys in order.
func (o Object) Keys() []string {
	keys := make([]string, len(o))
	for i, m := range o {
		keys[i] = m.Key
	}
	return keys
}

// set replaces an existing key in place (Python keeps the first position
// and the last value for duplicate keys) or appends it.
func (o Object) set(key string, value any) Object {
	for i := range o {
		if o[i].Key == key {
			o[i].Value = value
			return o
		}
	}
	return append(o, Member{Key: key, Value: value})
}

// Decode parses one JSON document. Objects become [Object], arrays []any,
// integer literals int64 (float64 when they overflow), other numbers
// float64. Trailing data is an error, as it is for json.loads.
func Decode(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("extra data after JSON value")
	}
	return v, nil
}

func decodeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			obj := Object{}
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, ok := keyTok.(string)
				if !ok {
					return nil, errors.New("object key is not a string")
				}
				value, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				obj = obj.set(key, value)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return obj, nil
		case '[':
			list := []any{}
			for dec.More() {
				value, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				list = append(list, value)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return list, nil
		default:
			return nil, fmt.Errorf("unexpected delimiter %q", t)
		}
	case json.Number:
		return number(t)
	default:
		// string, bool or nil
		return t, nil
	}
}

func number(n json.Number) (any, error) {
	s := n.String()
	if !strings.ContainsAny(s, ".eE") {
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return i, nil
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return nil, err
	}
	return f, nil
}

// Plain converts decoded values to the shapes encoding/json produces
// (map[string]any, float64), for code that does not care about order.
func Plain(v any) any {
	switch t := v.(type) {
	case Object:
		m := make(map[string]any, len(t))
		for _, member := range t {
			m[member.Key] = Plain(member.Value)
		}
		return m
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = Plain(item)
		}
		return out
	case int64:
		return float64(t)
	default:
		return v
	}
}

// Options control [Encode].
type Options struct {
	// EnsureASCII is json.dumps's ensure_ascii (true by default in Python).
	EnsureASCII bool
	// Indent, when positive, is json.dumps's indent: one member per line
	// and "," as the item separator.
	Indent int
}

// Dumps is json.dumps(v) with Python's defaults (ensure_ascii=True).
func Dumps(v any) string { return Encode(v, Options{EnsureASCII: true}) }

// DumpsUnicode is json.dumps(v, ensure_ascii=False).
func DumpsUnicode(v any) string { return Encode(v, Options{}) }

// Encode serialises v. Supported values: nil, bool, string, int, int64,
// float64, json.Number, []any, []string, Object, map[string]any (keys
// sorted, since a Go map has no order) and map[string]string. Anything else
// is written as its fmt %v string.
func Encode(v any, opts Options) string {
	var b strings.Builder
	encode(&b, v, opts, 0)
	return b.String()
}

func encode(b *strings.Builder, v any, opts Options, depth int) {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if t {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case string:
		writeString(b, t, opts.EnsureASCII)
	case int:
		b.WriteString(strconv.Itoa(t))
	case int64:
		b.WriteString(strconv.FormatInt(t, 10))
	case float64:
		b.WriteString(jsonFloat(t))
	case json.Number:
		b.WriteString(t.String())
	case []string:
		items := make([]any, len(t))
		for i, s := range t {
			items[i] = s
		}
		encodeList(b, items, opts, depth)
	case []any:
		encodeList(b, t, opts, depth)
	case Object:
		encodeObject(b, t, opts, depth)
	case map[string]any:
		encodeObject(b, sortedObject(t), opts, depth)
	case map[string]string:
		m := make(map[string]any, len(t))
		for k, s := range t {
			m[k] = s
		}
		encodeObject(b, sortedObject(m), opts, depth)
	default:
		writeString(b, fmt.Sprint(t), opts.EnsureASCII)
	}
}

func sortedObject(m map[string]any) Object {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	obj := make(Object, 0, len(keys))
	for _, k := range keys {
		obj = append(obj, Member{Key: k, Value: m[k]})
	}
	return obj
}

func newline(b *strings.Builder, opts Options, depth int) {
	b.WriteByte('\n')
	b.WriteString(strings.Repeat(" ", opts.Indent*depth))
}

func separator(b *strings.Builder, opts Options) {
	if opts.Indent > 0 {
		b.WriteByte(',')
	} else {
		b.WriteString(", ")
	}
}

func encodeList(b *strings.Builder, items []any, opts Options, depth int) {
	if len(items) == 0 {
		b.WriteString("[]")
		return
	}
	b.WriteByte('[')
	for i, item := range items {
		if i > 0 {
			separator(b, opts)
		}
		if opts.Indent > 0 {
			newline(b, opts, depth+1)
		}
		encode(b, item, opts, depth+1)
	}
	if opts.Indent > 0 {
		newline(b, opts, depth)
	}
	b.WriteByte(']')
}

func encodeObject(b *strings.Builder, obj Object, opts Options, depth int) {
	if len(obj) == 0 {
		b.WriteString("{}")
		return
	}
	b.WriteByte('{')
	for i, m := range obj {
		if i > 0 {
			separator(b, opts)
		}
		if opts.Indent > 0 {
			newline(b, opts, depth+1)
		}
		writeString(b, m.Key, opts.EnsureASCII)
		b.WriteString(": ")
		encode(b, m.Value, opts, depth+1)
	}
	if opts.Indent > 0 {
		newline(b, opts, depth)
	}
	b.WriteByte('}')
}

func writeString(b *strings.Builder, s string, ensureASCII bool) {
	b.WriteByte('"')
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			switch {
			case r < 0x20:
				fmt.Fprintf(b, `\u%04x`, r)
			case ensureASCII && r > 0x7e:
				if r > 0xffff {
					r -= 0x10000
					fmt.Fprintf(b, `\u%04x\u%04x`, 0xd800+(r>>10), 0xdc00+(r&0x3ff))
				} else {
					fmt.Fprintf(b, `\u%04x`, r)
				}
			default:
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

func jsonFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	return FormatFloat(f)
}

// FormatFloat is Python's repr(float) / str(float): the shortest digits that
// round-trip, in fixed notation when the decimal exponent is in [-4, 16)
// (always with a fractional part, "1.0") and in exponent notation otherwise
// ("1e-05", "1e+16").
func FormatFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	sign := ""
	if math.Signbit(f) {
		sign = "-"
		f = -f
	}
	if f == 0 {
		return sign + "0.0"
	}
	e := strconv.FormatFloat(f, 'e', -1, 64) // d.ddde±XX
	mant, expPart, _ := strings.Cut(e, "e")
	digits := strings.Replace(mant, ".", "", 1)
	exp, _ := strconv.Atoi(expPart)
	decpt := exp + 1 // value = 0.digits × 10^decpt
	if decpt > -4 && decpt <= 16 {
		switch {
		case decpt <= 0:
			return sign + "0." + strings.Repeat("0", -decpt) + digits
		case decpt >= len(digits):
			return sign + digits + strings.Repeat("0", decpt-len(digits)) + ".0"
		default:
			return sign + digits[:decpt] + "." + digits[decpt:]
		}
	}
	out := sign + digits[:1]
	if len(digits) > 1 {
		out += "." + digits[1:]
	}
	expSign := "+"
	if exp < 0 {
		expSign = "-"
		exp = -exp
	}
	return fmt.Sprintf("%se%s%02d", out, expSign, exp)
}
