// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package version carries the build identity of the lens binary.
//
// The Makefile stamps both variables at link time from git:
//
//	go build -ldflags "-X github.com/ElcanoTek/lens/internal/version.Version=$(git describe --tags --always --dirty) \
//	  -X github.com/ElcanoTek/lens/internal/version.Commit=$(git rev-parse HEAD)" ./cmd/lens
//
// scripts/update.sh builds in a staging copy without .git, so it passes both
// values to make explicitly. Commit is the full 40-hex revision on purpose:
// update.sh writes the same full SHA to /opt/lens/.deployed-revision, and
// `lens doctor` compares the two to prove the binary on disk is the release
// that was deployed (not a leftover from an interrupted swap).
//
// An unstamped build (`go build`, `go test`, `go run`) keeps the "dev"
// sentinel and recovers the revision from the toolchain's embedded VCS info
// when there is any. The package is a leaf: stdlib only.
package version

import (
	"runtime/debug"
	"strings"
)

// Version is the human-facing release name (git describe), or "dev".
// A var, not a const, so the linker's -X can override it.
var Version = "dev"

// Commit is the full git revision the binary was built from, or "" when the
// build was not stamped (Revision then falls back to the embedded VCS info).
var Commit = ""

// Revision returns the stamped Commit, else the toolchain-recorded VCS
// revision (with a "+dirty" marker for a modified tree), else "unknown".
func Revision() string {
	if c := strings.TrimSpace(Commit); c != "" {
		return c
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	var rev string
	var dirty bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "unknown"
	}
	if dirty {
		rev += "+dirty"
	}
	return rev
}

// String is what `lens --version` prints: "lens <version> (<revision>)".
// Doctor and update.sh search this line for the deployed revision, so keep
// the full revision in it.
func String() string {
	return "lens " + Version + " (" + Revision() + ")"
}
