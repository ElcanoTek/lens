// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
//
// Cases ported from python-dotenv 1.2.3 tests/test_parser.py and
// tests/test_variables.py (BSD-3-Clause; see LICENSE in this directory).

package dotenv

import (
	"reflect"
	"testing"
)

func sp(s string) *string { return &s }

func TestParseBindings(t *testing.T) {
	cases := []struct {
		in   string
		want []Binding
	}{
		{"", []Binding{}},
		{"a=b", []Binding{{sp("a"), sp("b"), Original{"a=b", 1}, false}}},
		{"'a'=b", []Binding{{sp("a"), sp("b"), Original{"'a'=b", 1}, false}}},
		{"[=b", []Binding{{sp("["), sp("b"), Original{"[=b", 1}, false}}},
		{" a = b ", []Binding{{sp("a"), sp("b"), Original{" a = b ", 1}, false}}},
		{"export a=b", []Binding{{sp("a"), sp("b"), Original{"export a=b", 1}, false}}},
		{" export 'a'=b", []Binding{{sp("a"), sp("b"), Original{" export 'a'=b", 1}, false}}},
		{"# a=b", []Binding{{nil, nil, Original{"# a=b", 1}, false}}},
		{"a=b#c", []Binding{{sp("a"), sp("b#c"), Original{"a=b#c", 1}, false}}},
		{"a=b #c", []Binding{{sp("a"), sp("b"), Original{"a=b #c", 1}, false}}},
		{"a=b\t#c", []Binding{{sp("a"), sp("b"), Original{"a=b\t#c", 1}, false}}},
		{"a=b c", []Binding{{sp("a"), sp("b c"), Original{"a=b c", 1}, false}}},
		{"a=b\tc", []Binding{{sp("a"), sp("b\tc"), Original{"a=b\tc", 1}, false}}},
		{"a=b  c", []Binding{{sp("a"), sp("b  c"), Original{"a=b  c", 1}, false}}},
		{"a=b\u00a0 c", []Binding{{sp("a"), sp("b\u00a0 c"), Original{"a=b\u00a0 c", 1}, false}}},
		{"a=b c ", []Binding{{sp("a"), sp("b c"), Original{"a=b c ", 1}, false}}},
		{"a='b c '", []Binding{{sp("a"), sp("b c "), Original{"a='b c '", 1}, false}}},
		{"a=\"b c \"", []Binding{{sp("a"), sp("b c "), Original{"a=\"b c \"", 1}, false}}},
		{"export export_a=1", []Binding{{sp("export_a"), sp("1"), Original{"export export_a=1", 1}, false}}},
		{"export port=8000", []Binding{{sp("port"), sp("8000"), Original{"export port=8000", 1}, false}}},
		{"a=\"b\nc\"", []Binding{{sp("a"), sp("b\nc"), Original{"a=\"b\nc\"", 1}, false}}},
		{"a='b\nc'", []Binding{{sp("a"), sp("b\nc"), Original{"a='b\nc'", 1}, false}}},
		{"a=\"b\nc\"", []Binding{{sp("a"), sp("b\nc"), Original{"a=\"b\nc\"", 1}, false}}},
		{"a=\"b\\nc\"", []Binding{{sp("a"), sp("b\nc"), Original{"a=\"b\\nc\"", 1}, false}}},
		{"a='b\\nc'", []Binding{{sp("a"), sp("b\\nc"), Original{"a='b\\nc'", 1}, false}}},
		{"a=\"b\\\"c\"", []Binding{{sp("a"), sp("b\"c"), Original{"a=\"b\\\"c\"", 1}, false}}},
		{"a='b\\'c'", []Binding{{sp("a"), sp("b'c"), Original{"a='b\\'c'", 1}, false}}},
		{"a='b\\\\c'", []Binding{{sp("a"), sp("b\\c"), Original{"a='b\\\\c'", 1}, false}}},
		{"a=\"b\\\\c\"", []Binding{{sp("a"), sp("b\\c"), Original{"a=\"b\\\\c\"", 1}, false}}},
		{"a='b\\\\'\nc='d'", []Binding{{sp("a"), sp("b\\"), Original{"a='b\\\\'\n", 1}, false}, {sp("c"), sp("d"), Original{"c='d'", 2}, false}}},
		{"a=\"b\\\\\"\nc=\"d\"", []Binding{{sp("a"), sp("b\\"), Original{"a=\"b\\\\\"\n", 1}, false}, {sp("c"), sp("d"), Original{"c=\"d\"", 2}, false}}},
		{"a='b\\\\\\'c'", []Binding{{sp("a"), sp("b\\'c"), Original{"a='b\\\\\\'c'", 1}, false}}},
		{"a=\u00e0", []Binding{{sp("a"), sp("\u00e0"), Original{"a=\u00e0", 1}, false}}},
		{"a=\"\u00e0\"", []Binding{{sp("a"), sp("\u00e0"), Original{"a=\"\u00e0\"", 1}, false}}},
		{"no_value_var", []Binding{{sp("no_value_var"), nil, Original{"no_value_var", 1}, false}}},
		{"a: b", []Binding{{nil, nil, Original{"a: b", 1}, true}}},
		{"a=b\nc=d", []Binding{{sp("a"), sp("b"), Original{"a=b\n", 1}, false}, {sp("c"), sp("d"), Original{"c=d", 2}, false}}},
		{"a=b\rc=d", []Binding{{sp("a"), sp("b"), Original{"a=b\r", 1}, false}, {sp("c"), sp("d"), Original{"c=d", 2}, false}}},
		{"a=b\r\nc=d", []Binding{{sp("a"), sp("b"), Original{"a=b\r\n", 1}, false}, {sp("c"), sp("d"), Original{"c=d", 2}, false}}},
		{"a=\nb=c", []Binding{{sp("a"), sp(""), Original{"a=\n", 1}, false}, {sp("b"), sp("c"), Original{"b=c", 2}, false}}},
		{"\n\n", []Binding{{nil, nil, Original{"\n\n", 1}, false}}},
		{"a=b\n\n", []Binding{{sp("a"), sp("b"), Original{"a=b\n", 1}, false}, {nil, nil, Original{"\n", 2}, false}}},
		{"a=b\n\nc=d", []Binding{{sp("a"), sp("b"), Original{"a=b\n", 1}, false}, {sp("c"), sp("d"), Original{"\nc=d", 2}, false}}},
		{"a=\"\nb=c", []Binding{{nil, nil, Original{"a=\"\n", 1}, true}, {sp("b"), sp("c"), Original{"b=c", 2}, false}}},
		{"# comment\na=\"b\nc\"\nd=e\n", []Binding{{nil, nil, Original{"# comment\n", 1}, false}, {sp("a"), sp("b\nc"), Original{"a=\"b\nc\"\n", 2}, false}, {sp("d"), sp("e"), Original{"d=e\n", 4}, false}}},
		{"a=b\n# comment 1", []Binding{{sp("a"), sp("b"), Original{"a=b\n", 1}, false}, {nil, nil, Original{"# comment 1", 2}, false}}},
		{"# comment 1\n# comment 2", []Binding{{nil, nil, Original{"# comment 1\n", 1}, false}, {nil, nil, Original{"# comment 2", 2}, false}}},
		{"uglyKey[%$=\"S3cr3t_P4ssw#rD\" #\na=b", []Binding{{sp("uglyKey[%$"), sp("S3cr3t_P4ssw#rD"), Original{"uglyKey[%$=\"S3cr3t_P4ssw#rD\" #\n", 1}, false}, {sp("a"), sp("b"), Original{"a=b", 2}, false}}},
		{"\ufeffa=b", []Binding{{sp("a"), sp("b"), Original{"a=b", 1}, false}}},
		{"\ufeffa=b\nc=d", []Binding{{sp("a"), sp("b"), Original{"a=b\n", 1}, false}, {sp("c"), sp("d"), Original{"c=d", 2}, false}}},
		// Beyond upstream: Python's \s includes Unicode spaces and U+001C..1F.
		{"a=b　#c", []Binding{{sp("a"), sp("b"), Original{"a=b　#c", 1}, false}}},
		{"a=b ", []Binding{{sp("a"), sp("b"), Original{"a=b ", 1}, false}}},
		// A CRLF after a bad statement: the "\r" ends it, the "\n" starts the next.
		{"a: b\r\nc=d", []Binding{{nil, nil, Original{"a: b\r", 1}, true}, {sp("c"), sp("d"), Original{"\nc=d", 2}, false}}},
		{"'a", []Binding{{nil, nil, Original{"'a", 1}, true}}},
		{"export ", []Binding{{nil, nil, Original{"export ", 1}, true}}},
		{"export=1", []Binding{{sp("export"), sp("1"), Original{"export=1", 1}, false}}},
		{`a="b\x\n"`, []Binding{{sp("a"), sp("b\\x\n"), Original{`a="b\x\n"`, 1}, false}}},
		{`a="\a\b\f\r\t\v\'"`, []Binding{{sp("a"), sp("\a\b\f\r\t\v'"), Original{`a="\a\b\f\r\t\v\'"`, 1}, false}}},
		{`a='\"'`, []Binding{{sp("a"), sp(`\"`), Original{`a='\"'`, 1}, false}}},
		{"a=\"b\" junk", []Binding{{nil, nil, Original{"a=\"b\" junk", 1}, true}}},
		{"a=b # c\n#d\n", []Binding{{sp("a"), sp("b"), Original{"a=b # c\n", 1}, false}, {nil, nil, Original{"#d\n", 2}, false}}},
	}
	for _, tc := range cases {
		got := ParseBindings(tc.in)
		if len(got) == 0 && len(tc.want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ParseBindings(%q)\n got %s\nwant %s", tc.in, dump(got), dump(tc.want))
		}
	}
}

func dump(bs []Binding) string {
	s := "["
	for _, b := range bs {
		k, v := "<nil>", "<nil>"
		if b.Key != nil {
			k = *b.Key
		}
		if b.Value != nil {
			v = *b.Value
		}
		s += " {" + k + "|" + v + "|" + b.Original.String + "|" + string(rune('0'+b.Original.Line)) + "}"
		if b.Error {
			s += "!"
		}
	}
	return s + " ]"
}

func TestParseVariables(t *testing.T) {
	lit := func(s string) Atom { return Atom{Literal: s} }
	v := func(name string, def *string) Atom { return Atom{IsVar: true, Name: name, Default: def} }
	cases := []struct {
		in   string
		want []Atom
	}{
		{"", nil},
		{"a", []Atom{lit("a")}},
		{"${a}", []Atom{v("a", nil)}},
		{"${a:-b}", []Atom{v("a", sp("b"))}},
		{"${a:-}", []Atom{v("a", sp(""))}},
		{"${a}${b}", []Atom{v("a", nil), v("b", nil)}},
		{"a${b}c${d}e", []Atom{lit("a"), v("b", nil), lit("c"), v("d", nil), lit("e")}},
		{"$a ${a:b}", []Atom{lit("$a ${a:b}")}},
	}
	for _, tc := range cases {
		if got := ParseVariables(tc.in); (len(got) > 0 || len(tc.want) > 0) && !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ParseVariables(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}
