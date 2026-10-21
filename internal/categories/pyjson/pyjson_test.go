// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package pyjson

import (
	"math"
	"testing"
)

func TestFormatFloatMatchesPythonRepr(t *testing.T) {
	cases := map[float64]string{
		0.73: "0.73", 1.0: "1.0", 0.1: "0.1", 1e-05: "1e-05", 0.0001: "0.0001", 1e16: "1e+16",
		1e15: "1000000000000000.0", 123.456: "123.456", -0.5: "-0.5", 0: "0.0", 2.5e-10: "2.5e-10",
		1.7976931348623157e308: "1.7976931348623157e+308", 0.30000000000000004: "0.30000000000000004",
		math.Copysign(0, -1): "-0.0",
	}
	for f, want := range cases {
		if got := FormatFloat(f); got != want {
			t.Errorf("FormatFloat(%v) = %q, want %q", f, got, want)
		}
	}
	if FormatFloat(math.NaN()) != "nan" || FormatFloat(math.Inf(-1)) != "-inf" {
		t.Error("specials")
	}
}

func TestDumpsMatchesPython(t *testing.T) {
	v := Object{
		{Key: "b", Value: []any{int64(1), 1.0, "é <&>\"\\\n\x01\x7f", nil, true}},
		{Key: "a", Value: Object{}},
		{Key: "emoji", Value: "😀"},
		{Key: "list", Value: []any{}},
	}
	ascii := `{"b": [1, 1.0, "\u00e9\u2028<&>\"\\\n\u0001\u007f", null, true], "a": {}, "emoji": "\ud83d\ude00", "list": []}`
	if got := Dumps(v); got != ascii {
		t.Errorf("Dumps =\n%s\nwant\n%s", got, ascii)
	}
	unicode := "{\"b\": [1, 1.0, \"é <&>\\\"\\\\\\n\\u0001\x7f\", null, true], \"a\": {}, \"emoji\": \"😀\", \"list\": []}"
	if got := DumpsUnicode(v); got != unicode {
		t.Errorf("DumpsUnicode =\n%s\nwant\n%s", got, unicode)
	}
	indented := "{\n  \"x\": [\n    1,\n    2\n  ],\n  \"y\": {}\n}"
	if got := Encode(Object{{Key: "x", Value: []any{int64(1), int64(2)}}, {Key: "y", Value: Object{}}}, Options{EnsureASCII: true, Indent: 2}); got != indented {
		t.Errorf("indent =\n%s", got)
	}
}

func TestDecodeKeepsOrderAndNumberKinds(t *testing.T) {
	v, err := Decode([]byte(`{"z": 1, "a": 1.0, "m": 1e2, "z": 2, "n": [true, null, "s"]}`))
	if err != nil {
		t.Fatal(err)
	}
	obj := v.(Object)
	if got := obj.Keys(); len(got) != 4 || got[0] != "z" || got[1] != "a" || got[2] != "m" || got[3] != "n" {
		t.Fatalf("keys = %v", got)
	}
	if z, _ := obj.Get("z"); z != int64(2) {
		t.Errorf("z = %#v (duplicate keys: first position, last value)", z)
	}
	if a, _ := obj.Get("a"); a != 1.0 {
		t.Errorf("a = %#v", a)
	}
	if m, _ := obj.Get("m"); m != 100.0 {
		t.Errorf("m = %#v", m)
	}
	if Dumps(v) != `{"z": 2, "a": 1.0, "m": 100.0, "n": [true, null, "s"]}` {
		t.Errorf("round trip = %s", Dumps(v))
	}
	for _, bad := range []string{`{"a": 1} x`, `{`, `NaN`, ``} {
		if _, err := Decode([]byte(bad)); err == nil {
			t.Errorf("Decode(%q) accepted", bad)
		}
	}
	plain := Plain(v).(map[string]any)
	if plain["z"] != 2.0 {
		t.Errorf("plain = %v", plain)
	}
}
