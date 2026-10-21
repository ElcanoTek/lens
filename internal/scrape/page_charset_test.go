// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package scrape

import "testing"

func TestDecodeBodyNonLatinCharsets(t *testing.T) {
	cases := []struct {
		ct   string
		raw  []byte
		want string
	}{
		{"text/html; charset=Shift_JIS", []byte{0x93, 0xfa, 0x96, 0x7b}, "日本"},
		{"text/html; charset=GBK", []byte{0xd6, 0xd0, 0xce, 0xc4}, "中文"},
		{"text/html; charset=koi8-r", []byte{0xf0, 0xd2, 0xc9, 0xd7, 0xc5, 0xd4}, "Привет"},
		{"text/html; charset=no-such-charset", []byte("plain"), "plain"},
	}
	for _, c := range cases {
		if got := DecodeBody(c.raw, c.ct); got != c.want {
			t.Errorf("DecodeBody(%q) = %q, want %q", c.ct, got, c.want)
		}
	}
}
