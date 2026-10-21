// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// `lens token` / `lens access` must read the instance's .env even when run
// (as the service user) from a directory with no .env of its own.
func TestLoadInstanceEnvUsesAppDirNotCwd(t *testing.T) {
	app := t.TempDir()
	if err := os.WriteFile(filepath.Join(app, ".env"), []byte("LENS_API_TOKEN_DB=/srv/tokens.db\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	t.Setenv("LENS_HOME", "")
	t.Setenv("LENS_APP_DIR", app)
	t.Setenv("LENS_API_TOKEN_DB", "")
	if err := os.Unsetenv("LENS_API_TOKEN_DB"); err != nil {
		t.Fatal(err)
	}
	loadInstanceEnv()
	if got := os.Getenv("LENS_API_TOKEN_DB"); got != "/srv/tokens.db" {
		t.Fatalf("LENS_API_TOKEN_DB = %q, want the app dir's value", got)
	}
}

func TestLoadInstanceEnvPrefersLensHome(t *testing.T) {
	home, app := t.TempDir(), t.TempDir()
	_ = os.WriteFile(filepath.Join(home, ".env"), []byte("LENS_ACCESS_DB=/home.db\n"), 0o600)
	_ = os.WriteFile(filepath.Join(app, ".env"), []byte("LENS_ACCESS_DB=/app.db\n"), 0o600)
	t.Setenv("LENS_HOME", home)
	t.Setenv("LENS_APP_DIR", app)
	t.Setenv("LENS_ACCESS_DB", "")
	_ = os.Unsetenv("LENS_ACCESS_DB")
	loadInstanceEnv()
	if got := os.Getenv("LENS_ACCESS_DB"); got != "/home.db" {
		t.Fatalf("LENS_ACCESS_DB = %q, want LENS_HOME's", got)
	}
}
