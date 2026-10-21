// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package pipeline

import (
	"bytes"
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/ElcanoTek/lens/internal/categories"
	"github.com/ElcanoTek/lens/internal/categories/pyjson"
)

// Record is one output row: column name -> value, in insertion order.
//
// The Python release built rows as dicts and handed them to csv.DictWriter
// (which wrote str(value)) and to the progress file's "result" payload
// (json.dump, insertion order). Record keeps both shapes: values are Go
// strings, ints, float64s (Python floats), nil, or whatever a custom-category
// result carries, and are rendered by [FormatCell] / [Record.MarshalJSON]
// exactly as Python rendered them.
type Record struct {
	keys []string
	vals map[string]any
}

// NewRecord returns an empty record.
func NewRecord() *Record { return &Record{vals: map[string]any{}} }

// Set assigns a column, keeping its first position (dict semantics).
func (r *Record) Set(key string, value any) {
	if _, ok := r.vals[key]; !ok {
		r.keys = append(r.keys, key)
	}
	r.vals[key] = value
}

// SetDefault assigns a column only when it is absent (dict.setdefault).
func (r *Record) SetDefault(key string, value any) {
	if _, ok := r.vals[key]; !ok {
		r.Set(key, value)
	}
}

// Get returns a column's value and whether it is present.
func (r *Record) Get(key string) (any, bool) {
	v, ok := r.vals[key]
	return v, ok
}

// String returns the cell text for key ("" when absent), as written to CSV.
func (r *Record) String(key string) string {
	v, ok := r.vals[key]
	if !ok {
		return ""
	}
	return FormatCell(v)
}

// Keys returns the columns in insertion order.
func (r *Record) Keys() []string { return append([]string(nil), r.keys...) }

// Update merges a custom-category result (dict.update): existing keys keep
// their position, new keys are appended in sorted order (Go maps have no
// order; the CSV column order comes from the header, so only the progress
// payload sees this).
func (r *Record) Update(result categories.Result) {
	for _, k := range sortedKeys(result) {
		r.Set(k, result[k])
	}
}

// MarshalJSON writes the record as an ordered JSON object, numbers in
// Python's json.dumps form (1.0 stays 1.0).
func (r *Record) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range r.keys {
		if i > 0 {
			b.WriteString(", ")
		}
		key, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		b.Write(key)
		b.WriteString(": ")
		val, err := marshalValue(r.vals[k])
		if err != nil {
			return nil, err
		}
		b.Write(val)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func marshalValue(v any) ([]byte, error) {
	switch t := v.(type) {
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			return []byte("null"), nil
		}
		return []byte(pyjson.FormatFloat(t)), nil
	case float32:
		return marshalValue(float64(t))
	default:
		return json.Marshal(v)
	}
}

// FormatCell renders a value the way Python's csv module did (str(value)):
// None -> "", floats in repr form ("4.5", "1.0"), ints as digits.
func FormatCell(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case int:
		return strconv.Itoa(t)
	case *float64:
		if t == nil {
			return ""
		}
		return pyjson.FormatFloat(*t)
	case *int:
		if t == nil {
			return ""
		}
		return strconv.Itoa(*t)
	default:
		return categories.FormatCell(v)
	}
}

// pyRound2 is Python's round(x, 2): correctly rounded to two decimals, as a
// float (so it prints "0.5", not "0.50").
func pyRound2(x float64) float64 {
	f, err := strconv.ParseFloat(strconv.FormatFloat(x, 'f', 2, 64), 64)
	if err != nil {
		return x
	}
	return f
}

// isoNow is Python's datetime.now().isoformat(): local, naive, microseconds
// omitted when they are exactly zero.
func isoNow() string { return isoFormat(time.Now()) }

func isoFormat(t time.Time) string {
	if t.Nanosecond()/1000 == 0 {
		return t.Format("2006-01-02T15:04:05")
	}
	return t.Format("2006-01-02T15:04:05.000000")
}

// seconds converts a duration to Python's float seconds.
func seconds(d time.Duration) float64 { return d.Seconds() }

func sortedKeys(m categories.Result) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
