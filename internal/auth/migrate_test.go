// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package auth

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

func createMarkerDB(t *testing.T, path, marker string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, stmt := range []string{"CREATE TABLE state (marker TEXT NOT NULL)", "INSERT INTO state(marker) VALUES ('" + marker + "')"} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
}

func readMarker(t *testing.T, path string) string {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var marker string
	if err := db.QueryRow("SELECT marker FROM state").Scan(&marker); err != nil {
		t.Fatal(err)
	}
	return marker
}

func modeOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func migratePaths(t *testing.T) (string, string) {
	root := t.TempDir()
	return filepath.Join(root, "opt", "lens", "data", "access.db"), filepath.Join(root, "var", "lib", "lens", "access.db")
}

func TestMigrationCopiesAValidDatabaseWithoutRemovingTheLegacyCopy(t *testing.T) {
	legacy, target := migratePaths(t)
	createMarkerDB(t, legacy, "existing-client-state")
	migrated, err := MigrateAccessDatabase(context.Background(), legacy, target, os.Getuid(), os.Getgid())
	if err != nil || !migrated {
		t.Fatalf("migrate %v %v", migrated, err)
	}
	if readMarker(t, target) != "existing-client-state" || readMarker(t, legacy) != "existing-client-state" {
		t.Fatal("markers")
	}
	if modeOf(t, target) != 0o600 || modeOf(t, filepath.Dir(target)) != 0o750 {
		t.Fatalf("modes %v %v", modeOf(t, target), modeOf(t, filepath.Dir(target)))
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(target), ".access.db.migrating-*"))
	if len(leftovers) != 0 {
		t.Fatalf("temporary files left: %v", leftovers)
	}
	// A second run is idempotent.
	if again, err := MigrateAccessDatabase(context.Background(), legacy, target, os.Getuid(), os.Getgid()); err != nil || again {
		t.Fatalf("second run %v %v", again, err)
	}
}

func TestMigratedDatabaseOpensAsAnAccessStore(t *testing.T) {
	ctx := context.Background()
	legacy, target := migratePaths(t)
	store, err := OpenStore(ctx, legacy, DefaultIdleSeconds, DefaultAbsoluteSeconds)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.GrantAccess(ctx, "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	if ok, err := MigrateAccessDatabase(ctx, legacy, target, os.Getuid(), os.Getgid()); !ok || err != nil {
		t.Fatalf("%v %v", ok, err)
	}
	migrated, err := OpenStore(ctx, target, DefaultIdleSeconds, DefaultAbsoluteSeconds)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = migrated.Close() }()
	if ok, _ := migrated.IsAllowed(ctx, "alice@example.com"); !ok {
		t.Fatal("allowlist lost in migration")
	}
}

func TestMigrationNeverOverwritesAnExistingDestination(t *testing.T) {
	legacy, target := migratePaths(t)
	createMarkerDB(t, legacy, "legacy")
	createMarkerDB(t, target, "already-live")
	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatal(err)
	}
	migrated, err := MigrateAccessDatabase(context.Background(), legacy, target, os.Getuid(), os.Getgid())
	if err != nil || migrated {
		t.Fatalf("%v %v", migrated, err)
	}
	if readMarker(t, target) != "already-live" || readMarker(t, legacy) != "legacy" || modeOf(t, target) != 0o600 {
		t.Fatal("existing destination changed")
	}
}

func TestInvalidExistingDestinationStopsTheDeployment(t *testing.T) {
	legacy, target := migratePaths(t)
	createMarkerDB(t, legacy, "legacy")
	_ = os.MkdirAll(filepath.Dir(target), 0o755)
	_ = os.WriteFile(target, []byte("corrupted"), 0o600)
	if _, err := MigrateAccessDatabase(context.Background(), legacy, target, os.Getuid(), os.Getgid()); err == nil {
		t.Fatal("corrupt target accepted")
	}
	if data, _ := os.ReadFile(target); string(data) != "corrupted" {
		t.Fatal("target rewritten")
	}
}

func TestSymlinkDestinationIsRejected(t *testing.T) {
	legacy, target := migratePaths(t)
	elsewhere := filepath.Join(t.TempDir(), "elsewhere.db")
	createMarkerDB(t, legacy, "legacy")
	createMarkerDB(t, elsewhere, "must-not-be-used")
	_ = os.MkdirAll(filepath.Dir(target), 0o755)
	if err := os.Symlink(elsewhere, target); err != nil {
		t.Fatal(err)
	}
	_, err := MigrateAccessDatabase(context.Background(), legacy, target, os.Getuid(), os.Getgid())
	if err == nil || !strings.Contains(err.Error(), "refusing symlink") {
		t.Fatalf("symlink target: %v", err)
	}
	if readMarker(t, elsewhere) != "must-not-be-used" {
		t.Fatal("symlink target changed")
	}
	// A symlinked state directory is refused too.
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	_ = os.MkdirAll(realDir, 0o755)
	_ = os.Symlink(realDir, filepath.Join(root, "link"))
	if _, err := MigrateAccessDatabase(context.Background(), legacy, filepath.Join(root, "link", "access.db"), os.Getuid(), os.Getgid()); err == nil ||
		!strings.Contains(err.Error(), "refusing symlink at access database directory") {
		t.Fatalf("symlink dir: %v", err)
	}
}

func TestInvalidLegacyDatabaseCannotPublishAPartialDestination(t *testing.T) {
	legacy, target := migratePaths(t)
	_ = os.MkdirAll(filepath.Dir(legacy), 0o755)
	_ = os.WriteFile(legacy, []byte("not a sqlite database"), 0o600)
	if _, err := MigrateAccessDatabase(context.Background(), legacy, target, os.Getuid(), os.Getgid()); err == nil {
		t.Fatal("garbage legacy accepted")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("partial target published")
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(target), ".access.db.migrating-*"))
	if len(leftovers) != 0 {
		t.Fatalf("temporary files left: %v", leftovers)
	}
}

func TestMissingLegacyDatabaseOnlyPreparesTheDirectory(t *testing.T) {
	legacy, target := migratePaths(t)
	migrated, err := MigrateAccessDatabase(context.Background(), legacy, target, os.Getuid(), os.Getgid())
	if err != nil || migrated {
		t.Fatalf("%v %v", migrated, err)
	}
	if modeOf(t, filepath.Dir(target)) != 0o750 {
		t.Fatal("directory not prepared")
	}
}

func TestGeneratedLegacyEnvPathIsRewrittenAtomically(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), ".env")
	_ = os.WriteFile(envFile, []byte("OPENROUTER_API_KEY=\"secret\"\r\nLENS_ACCESS_DB=\"/opt/lens/data/access.db\"\nexport  LENS_ACCESS_DB = '/opt/lens/data/access.db'  # old\nAUTH_CLIENT_ID=\"lens\"\n"), 0o600)
	changed, err := RewriteLegacyEnvPath(envFile, "/opt/lens/data/access.db", "/var/lib/lens/access.db")
	if err != nil || !changed {
		t.Fatalf("%v %v", changed, err)
	}
	data, _ := os.ReadFile(envFile)
	want := "OPENROUTER_API_KEY=\"secret\"\nLENS_ACCESS_DB=\"/var/lib/lens/access.db\"\nexport  LENS_ACCESS_DB = \"/var/lib/lens/access.db\"\nAUTH_CLIENT_ID=\"lens\"\n"
	if string(data) != want {
		t.Fatalf("rewritten:\n%q\nwant\n%q", data, want)
	}
	if modeOf(t, envFile) != 0o600 {
		t.Fatal("mode changed")
	}
}

func TestCustomDatabasePathIsLeftUnchanged(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), ".env")
	original := "LENS_ACCESS_DB=\"/srv/lens-private/access.db\"\n"
	_ = os.WriteFile(envFile, []byte(original), 0o600)
	changed, err := RewriteLegacyEnvPath(envFile, "/opt/lens/data/access.db", "/var/lib/lens/access.db")
	if err != nil || changed {
		t.Fatalf("%v %v", changed, err)
	}
	if data, _ := os.ReadFile(envFile); string(data) != original {
		t.Fatal("custom path rewritten")
	}
	configured, found, err := ConfiguredAccessPath(envFile)
	if err != nil || !found || configured != "/srv/lens-private/access.db" {
		t.Fatalf("configured %q %v %v", configured, found, err)
	}
}

func TestMalformedDatabasePathStopsMigrationInsteadOfGuessing(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), ".env")
	for _, content := range []string{"LENS_ACCESS_DB=\"unterminated\n", "LENS_ACCESS_DB=two words\n", "LENS_ACCESS_DB=\n"} {
		_ = os.WriteFile(envFile, []byte(content), 0o600)
		if _, _, err := ConfiguredAccessPath(envFile); err == nil || !strings.Contains(err.Error(), "invalid LENS_ACCESS_DB") {
			t.Errorf("%q: %v", content, err)
		}
	}
	if _, found, err := ConfiguredAccessPath(filepath.Join(t.TempDir(), "missing")); found || err != nil {
		t.Fatal("missing env file")
	}
}

func currentUserName(t *testing.T) string {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Skip("no current user:", err)
	}
	return u.Username
}

func TestMigrationCommandPreservesStateAndRepointsTheEnvironment(t *testing.T) {
	legacy, target := migratePaths(t)
	envFile := filepath.Join(filepath.Dir(filepath.Dir(legacy)), ".env")
	createMarkerDB(t, legacy, "command-migrated")
	_ = os.WriteFile(envFile, []byte(`LENS_ACCESS_DB="`+legacy+"\"\n"), 0o600)
	var out, errOut bytes.Buffer
	code := migrateMain(context.Background(), []string{"--legacy", legacy, "--target=" + target, "--env-file", envFile, "--owner", currentUserName(t)}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "migrated Lens access database to "+target) || !strings.Contains(out.String(), "updated LENS_ACCESS_DB in "+envFile) {
		t.Fatalf("output %q", out.String())
	}
	if readMarker(t, target) != "command-migrated" || readMarker(t, legacy) != "command-migrated" {
		t.Fatal("markers")
	}
	if data, _ := os.ReadFile(envFile); string(data) != `LENS_ACCESS_DB="`+target+"\"\n" {
		t.Fatalf("env %q", data)
	}

	// Re-running (update.sh on every deploy) is a no-op that says so.
	out.Reset()
	if code := migrateMain(context.Background(), []string{"--legacy", legacy, "--target", target, "--env-file", envFile, "--owner", currentUserName(t)}, &out, &errOut); code != 0 {
		t.Fatal(errOut.String())
	}
	if out.String() != "Lens access database ready at "+target+"\n" {
		t.Fatalf("rerun output %q", out.String())
	}
}

func TestMigrationCommandPreservesACustomPathAndValidatesFlags(t *testing.T) {
	legacy, target := migratePaths(t)
	envFile := filepath.Join(t.TempDir(), ".env")
	createMarkerDB(t, legacy, "x")
	_ = os.WriteFile(envFile, []byte("LENS_ACCESS_DB=/srv/custom.db\n"), 0o600)
	var out, errOut bytes.Buffer
	if code := migrateMain(context.Background(), []string{"--legacy", legacy, "--target", target, "--env-file", envFile, "--owner", currentUserName(t)}, &out, &errOut); code != 0 {
		t.Fatal(errOut.String())
	}
	if out.String() != "custom LENS_ACCESS_DB preserved: /srv/custom.db\n" {
		t.Fatalf("output %q", out.String())
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("custom path migrated anyway")
	}

	// No env assignment and no legacy DB: only the directory is prepared.
	out.Reset()
	_ = os.WriteFile(envFile, []byte("OTHER=1\n"), 0o600)
	if code := migrateMain(context.Background(), []string{"--legacy", legacy + ".missing", "--target", target, "--env-file", envFile, "--owner", currentUserName(t)}, &out, &errOut); code != 0 {
		t.Fatal(errOut.String())
	}
	if out.String() != "Lens access database directory ready at "+filepath.Dir(target)+"\n" {
		t.Fatalf("output %q", out.String())
	}

	if code := migrateMain(context.Background(), []string{"--legacy", legacy}, &out, &errOut); code != 2 {
		t.Fatalf("missing flags exit %d", code)
	}
	if code := migrateMain(context.Background(), []string{"--legacy", legacy, "--target", target, "--env-file", envFile, "--owner", "no-such-user-lens-test"}, &out, &errOut); code != 1 {
		t.Fatalf("unknown owner exit %d", code)
	}
}

func TestPyPath(t *testing.T) {
	for in, want := range map[string]string{
		"/opt/lens//data/": "/opt/lens/data", "./a/./b": "a/b", "/a/../b": "/a/../b", "//x": "//x", "": ".",
	} {
		if got := pyPath(in); got != want {
			t.Errorf("pyPath(%q) = %q, want %q", in, got, want)
		}
	}
}
