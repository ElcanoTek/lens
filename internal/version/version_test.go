// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package version

import (
	"strings"
	"testing"
)

func TestStringCarriesStampedValues(t *testing.T) {
	oldV, oldC := Version, Commit
	t.Cleanup(func() { Version, Commit = oldV, oldC })

	Version, Commit = "v1.2.3-4-gabc", "0123456789abcdef0123456789abcdef01234567"
	got := String()
	if got != "lens v1.2.3-4-gabc (0123456789abcdef0123456789abcdef01234567)" {
		t.Fatalf("String() = %q", got)
	}
	if Revision() != Commit {
		t.Fatalf("Revision() = %q, want stamped commit", Revision())
	}
}

func TestUnstampedFallsBack(t *testing.T) {
	oldV, oldC := Version, Commit
	t.Cleanup(func() { Version, Commit = oldV, oldC })

	Version, Commit = "dev", "  "
	rev := Revision()
	if rev == "" {
		t.Fatal("Revision() must never be empty")
	}
	if !strings.HasPrefix(String(), "lens dev (") {
		t.Fatalf("String() = %q", String())
	}
}
