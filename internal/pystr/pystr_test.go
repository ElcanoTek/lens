// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package pystr

import "testing"

// Expected values are Python 3.13's str.casefold / unicodedata.normalize.
func TestMatchesPython(t *testing.T) {
	fold := map[string]string{
		"Straße": "strasse", "ẞ": "ss", "ﬀ": "ff", "ΣΑΣ": "σασ", "İ": "i̇", "Ǆ": "ǆ", "plain": "plain",
	}
	for in, want := range fold {
		if got := CaseFold(in); got != want {
			t.Errorf("CaseFold(%q) = %q, want %q", in, got, want)
		}
	}
	if got := NFC("é"); got != "é" {
		t.Errorf("NFC = %q", got)
	}
	if got := Strip("\x1c a 　"); got != "a" {
		t.Errorf("Strip = %q", got)
	}
	if !IsControl('​') || IsControl('a') {
		t.Error("IsControl")
	}
}
