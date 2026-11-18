// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package resultcsv

import "testing"

func TestWithoutCost(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{
			"last column",
			"Domain,Quality,Cost_USD\r\nexample.com,High,0.0012\r\n",
			"Domain,Quality\r\nexample.com,High\r\n",
		},
		{
			"middle column, quoted neighbours and embedded line breaks kept byte for byte",
			"Domain,Cost_USD,Description\r\n\"a,b.com\",0.5,\"two\r\nlines, \"\"quoted\"\"\"\r\nc.com,,plain\r\n",
			"Domain,Description\r\n\"a,b.com\",\"two\r\nlines, \"\"quoted\"\"\"\r\nc.com,plain\r\n",
		},
		{
			"first column",
			"Cost_USD,Domain\nx,\"y\"\n",
			"Domain\n\"y\"\n",
		},
		{
			"short row and no trailing newline",
			"Domain,Quality,Cost_USD\r\nonly.com\r\nb.com,Low,1",
			"Domain,Quality\r\nonly.com\r\nb.com,Low",
		},
		{
			"only column",
			"Cost_USD\r\n1\r\n",
			"\r\n\r\n",
		},
		{"no cost column", "Domain,Quality\r\na.com,High\r\n", "Domain,Quality\r\na.com,High\r\n"},
		{"empty", "", ""},
	}
	for _, c := range cases {
		if got := string(WithoutCost([]byte(c.in))); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}
