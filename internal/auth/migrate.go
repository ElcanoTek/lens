// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	sqlite "modernc.org/sqlite"
)

// pyWS is the character class Python's re \s matches in a str pattern.
const pyWS = `[\t\n\v\f\r \x{1c}-\x{1f}\x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}]`

var accessDBAssignment = regexp.MustCompile(
	`^(` + pyWS + `*(?:export` + pyWS + `+)?LENS_ACCESS_DB` + pyWS + `*=` + pyWS + `*)(.*?)(\r?\n?)$`)

// syncDirectory fsyncs a directory so a rename or link in it is durable.
func syncDirectory(path string) error {
	dir, err := os.Open(path) //nolint:gosec // operator-supplied deployment path
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}

func ownerOf(info fs.FileInfo) (int, int, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}

func ensureOwner(path string, uid, gid int) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if u, g, ok := ownerOf(info); ok && (u != uid || g != gid) {
		return os.Chown(path, uid, gid)
	}
	return nil
}

func ensureStateDirectory(path string, uid, gid int) error {
	if info, err := os.Lstat(path); err == nil && info.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("refusing symlink at access database directory: %s", path)
	}
	if err := os.MkdirAll(path, 0o777); err != nil { //nolint:gosec // narrowed to 0750 right below, as the Python did
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("access database parent is not a directory: %s", path)
	}
	if err := ensureOwner(path, uid, gid); err != nil {
		return err
	}
	return os.Chmod(path, 0o750) //nolint:gosec // the service group reads its state directory
}

func quickCheck(ctx context.Context, db *sql.DB, what string) error {
	var result string
	if err := db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("%s failed quick_check: ('%s',)", what, result)
	}
	return nil
}

func validateDatabase(ctx context.Context, path string) error {
	dsn, err := sqliteDSN(path, "mode=ro")
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	return quickCheck(ctx, db, "access database")
}

type backupConn interface {
	NewBackup(dstURI string) (*sqlite.Backup, error)
}

// backupDatabase copies the SQLite database at source (opened read-only) into
// destination with SQLite's online backup API, then checks the copy.
func backupDatabase(ctx context.Context, source, destination string) error {
	sourceDSN, err := sqliteDSN(source, "mode=ro")
	if err != nil {
		return err
	}
	destinationDSN, err := sqliteDSN(destination)
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", sourceDSN)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	err = conn.Raw(func(driverConn any) error {
		src, ok := driverConn.(backupConn)
		if !ok {
			return errors.New("sqlite driver does not support online backup")
		}
		backup, err := src.NewBackup(destinationDSN)
		if err != nil {
			return err
		}
		for {
			more, err := backup.Step(-1)
			if err != nil {
				_ = backup.Finish()
				return err
			}
			if !more {
				break
			}
		}
		return backup.Finish()
	})
	if err != nil {
		return err
	}
	dest, err := sql.Open("sqlite", destinationDSN)
	if err != nil {
		return err
	}
	defer func() { _ = dest.Close() }()
	return quickCheck(ctx, dest, "migrated access database")
}

// MigrateAccessDatabase atomically copies a legacy SQLite access database to
// target, never replacing an existing target. It returns true when it
// created target. An existing target is validated and re-owned/re-moded
// instead; a missing legacy database is not an error.
func MigrateAccessDatabase(ctx context.Context, legacy, target string, uid, gid int) (bool, error) {
	if err := ensureStateDirectory(filepath.Dir(target), uid, gid); err != nil {
		return false, err
	}
	if info, err := os.Lstat(target); err == nil {
		if info.Mode()&fs.ModeSymlink != 0 {
			return false, fmt.Errorf("refusing symlink at access database target: %s", target)
		}
		if !info.Mode().IsRegular() {
			return false, fmt.Errorf("access database target is not a regular file: %s", target)
		}
		if err := validateDatabase(ctx, target); err != nil {
			return false, err
		}
		if err := ensureOwner(target, uid, gid); err != nil {
			return false, err
		}
		return false, os.Chmod(target, 0o600)
	}
	info, err := os.Lstat(legacy)
	if err == nil && info.Mode()&fs.ModeSymlink != 0 {
		return false, fmt.Errorf("refusing symlink at legacy access database path: %s", legacy)
	}
	if err != nil || !info.Mode().IsRegular() {
		return false, nil
	}

	temp, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+".migrating-*")
	if err != nil {
		return false, err
	}
	tempPath := temp.Name()
	_ = temp.Close()
	defer func() { _ = os.Remove(tempPath) }()

	if err := backupDatabase(ctx, legacy, tempPath); err != nil {
		return false, err
	}
	if err := os.Chmod(tempPath, 0o600); err != nil {
		return false, err
	}
	if err := ensureOwner(tempPath, uid, gid); err != nil {
		return false, err
	}
	migrated, err := os.Open(tempPath) //nolint:gosec // our own temporary file
	if err != nil {
		return false, err
	}
	syncErr := migrated.Sync()
	_ = migrated.Close()
	if syncErr != nil {
		return false, syncErr
	}
	// A hard link is an atomic create-without-replace operation. Both paths
	// live in the target directory, so it also cannot cross a filesystem
	// boundary.
	if err := os.Link(tempPath, target); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return false, nil
		}
		return false, err
	}
	return true, syncDirectory(filepath.Dir(target))
}

// readTextFile is Python's Path.read_text(): UTF-8 with universal newlines.
func readTextFile(path string) (string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // operator-supplied deployment path
	if err != nil {
		return "", err
	}
	if !utf8.Valid(data) {
		return "", fmt.Errorf("%s is not valid UTF-8", path)
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	return strings.ReplaceAll(text, "\r", "\n"), nil
}

// splitLinesKeepEnds is Python's str.splitlines(keepends=True) for text that
// has already been through universal-newline translation.
func splitLinesKeepEnds(text string) []string {
	var lines []string
	start := 0
	for i, r := range text {
		switch r {
		case '\n', '\v', '\f', '\x1c', '\x1d', '\x1e', '\u0085', ' ', ' ':
			end := i + utf8.RuneLen(r)
			lines = append(lines, text[start:end])
			start = end
		}
	}
	if start < len(text) {
		lines = append(lines, text[start:])
	}
	return lines
}

func isRegularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func unquoteShellValue(value string) (string, bool) {
	parsed, err := shlexSplit(value)
	if err != nil || len(parsed) != 1 {
		return "", false
	}
	return parsed[0], true
}

// ConfiguredAccessPath returns the last LENS_ACCESS_DB assignment in an env
// file ("", false when the file or the assignment is absent). An assignment
// that is not exactly one shell word is an error: guessing could migrate to
// the wrong place.
func ConfiguredAccessPath(envFile string) (string, bool, error) {
	if !isRegularFile(envFile) {
		return "", false, nil
	}
	text, err := readTextFile(envFile)
	if err != nil {
		return "", false, err
	}
	configured, found := "", false
	for _, line := range splitLinesKeepEnds(text) {
		match := accessDBAssignment.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		value, ok := unquoteShellValue(match[2])
		if !ok {
			return "", false, fmt.Errorf("invalid LENS_ACCESS_DB assignment in %s", envFile)
		}
		configured, found = value, true
	}
	return configured, found, nil
}

// RewriteLegacyEnvPath rewrites only Lens's generated legacy default
// (LENS_ACCESS_DB=<legacy>) to target, atomically and preserving the file's
// mode and owner; custom paths are left alone. True when the file changed.
func RewriteLegacyEnvPath(envFile, legacy, target string) (bool, error) {
	if !isRegularFile(envFile) {
		return false, nil
	}
	text, err := readTextFile(envFile)
	if err != nil {
		return false, err
	}
	var b strings.Builder
	changed := false
	for _, line := range splitLinesKeepEnds(text) {
		if match := accessDBAssignment.FindStringSubmatch(line); match != nil {
			if value, ok := unquoteShellValue(match[2]); ok && value == legacy {
				line = match[1] + `"` + target + `"` + match[3]
				changed = true
			}
		}
		b.WriteString(line)
	}
	if !changed {
		return false, nil
	}
	info, err := os.Stat(envFile)
	if err != nil {
		return false, err
	}
	dir := filepath.Dir(envFile)
	temp, err := os.CreateTemp(dir, "."+filepath.Base(envFile)+".*")
	if err != nil {
		return false, err
	}
	tempPath := temp.Name()
	defer func() { _ = os.Remove(tempPath) }()
	if _, err := io.WriteString(temp, b.String()); err != nil {
		_ = temp.Close()
		return false, err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return false, err
	}
	if err := temp.Close(); err != nil {
		return false, err
	}
	if err := os.Chmod(tempPath, info.Mode().Perm()|(info.Mode()&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky))); err != nil {
		return false, err
	}
	if u, g, ok := ownerOf(info); ok {
		if err := ensureOwner(tempPath, u, g); err != nil {
			return false, err
		}
	}
	if err := os.Rename(tempPath, envFile); err != nil {
		return false, err
	}
	return true, syncDirectory(dir)
}

// pyPath is str(pathlib.PurePosixPath(p)): repeated slashes and "."
// components collapse and a trailing slash goes, but ".." is kept.
func pyPath(p string) string {
	if p == "" {
		return "."
	}
	prefix := ""
	switch {
	case strings.HasPrefix(p, "//") && !strings.HasPrefix(p, "///"):
		prefix = "//"
	case strings.HasPrefix(p, "/"):
		prefix = "/"
	}
	var parts []string
	for _, part := range strings.Split(p, "/") {
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	out := prefix + strings.Join(parts, "/")
	if out == "" {
		return "."
	}
	return out
}

// MigrateAccessDBMain is `lens migrate-access-db --legacy PATH --target PATH
// --env-file PATH --owner USER`, run by bootstrap.sh and update.sh with the
// service stopped: it safely moves Lens's persistent access database out of
// the release tree. Idempotent: an existing target is only validated and
// re-owned; a custom LENS_ACCESS_DB is never moved or rewritten.
func MigrateAccessDBMain(args []string) int {
	return migrateMain(context.Background(), args, os.Stdout, os.Stderr)
}

func migrateMain(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	const usage = "usage: lens migrate-access-db [-h] --legacy LEGACY --target TARGET --env-file ENV_FILE --owner OWNER\n"
	values := map[string]string{}
	for rest := args; len(rest) > 0; {
		arg := rest[0]
		rest = rest[1:]
		name, value, hasValue := strings.Cut(arg, "=")
		switch name {
		case "-h", "--help":
			_, _ = fmt.Fprint(stdout, usage+"\nSafely move Lens's persistent access database out of the release tree.\n")
			return 0
		case "--legacy", "--target", "--env-file", "--owner":
			if !hasValue {
				if len(rest) == 0 {
					_, _ = fmt.Fprintf(stderr, "%slens migrate-access-db: error: argument %s: expected one argument\n", usage, name)
					return 2
				}
				value, rest = rest[0], rest[1:]
			}
			values[name] = value
		default:
			_, _ = fmt.Fprintf(stderr, "%slens migrate-access-db: error: unrecognized arguments: %s\n", usage, arg)
			return 2
		}
	}
	var missing []string
	for _, name := range []string{"--legacy", "--target", "--env-file", "--owner"} {
		if _, ok := values[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		_, _ = fmt.Fprintf(stderr, "%slens migrate-access-db: error: the following arguments are required: %s\n", usage, strings.Join(missing, ", "))
		return 2
	}
	legacy, target, envFile := pyPath(values["--legacy"]), pyPath(values["--target"]), pyPath(values["--env-file"])

	fail := func(err error) int {
		_, _ = fmt.Fprintf(stderr, "lens migrate-access-db: %v\n", err)
		return 1
	}
	account, err := user.Lookup(values["--owner"])
	if err != nil {
		return fail(err)
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		return fail(err)
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil {
		return fail(err)
	}
	configured, found, err := ConfiguredAccessPath(envFile)
	if err != nil {
		return fail(err)
	}
	if found && configured != legacy && configured != target {
		_, _ = fmt.Fprintf(stdout, "custom LENS_ACCESS_DB preserved: %s\n", configured)
		return 0
	}
	migrated, err := MigrateAccessDatabase(ctx, legacy, target, uid, gid)
	if err != nil {
		return fail(err)
	}
	rewritten, err := RewriteLegacyEnvPath(envFile, legacy, target)
	if err != nil {
		return fail(err)
	}
	switch {
	case migrated:
		_, _ = fmt.Fprintf(stdout, "migrated Lens access database to %s\n", target)
	case pathExists(target):
		_, _ = fmt.Fprintf(stdout, "Lens access database ready at %s\n", target)
	default:
		_, _ = fmt.Fprintf(stdout, "Lens access database directory ready at %s\n", pyPath(filepath.Dir(target)))
	}
	if rewritten {
		_, _ = fmt.Fprintf(stdout, "updated LENS_ACCESS_DB in %s\n", envFile)
	}
	return 0
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
