// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
//
// Port of python-dotenv 1.2.3 src/dotenv/main.py (BSD-3-Clause; see LICENSE
// in this directory), minus get_key/unset_key, which Lens never called.

// Package dotenv reads and writes .env files with python-dotenv 1.2.3's
// exact grammar and ${VAR} interpolation rules.
//
// Lens's .env is three things at once: systemd's EnvironmentFile for
// lens.service, the file every process loads on start, and a file bootstrap
// writes with SetKey and operators hand-edit with `lens env edit`. Deployed
// boxes carry files written by the Python release, so the parser here is a
// faithful port of the one that read them (parser.go), not a simplification:
// export prefixes, single quotes (only \\ and \' escapes), double quotes
// (Python string escapes), multi-line quoted values, inline " #" comments on
// unquoted values, bare keys (nil value) and skip-and-warn on bad lines.
package dotenv

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ElcanoTek/lens/internal/logx"
)

var log = logx.New("dotenv.main")

// maxFileBytes bounds what one .env read may load; real files are a few KiB.
const maxFileBytes = 16 << 20

// Pair is one binding from a .env file. Value is nil for a bare key.
type Pair struct {
	Key   string
	Value *string
}

// readText reads r as UTF-8 text the way Python's open() in text mode does:
// universal newlines ("\r\n" and lone "\r" become "\n"). Invalid UTF-8 is an
// error, as it was for the Python reader.
func readText(r io.Reader) (string, error) {
	raw, err := io.ReadAll(io.LimitReader(r, maxFileBytes+1))
	if err != nil {
		return "", err
	}
	if len(raw) > maxFileBytes {
		return "", fmt.Errorf("dotenv: file larger than %d bytes", maxFileBytes)
	}
	if !utf8.Valid(raw) {
		return "", errors.New("dotenv: file is not valid UTF-8")
	}
	raw = bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
	raw = bytes.ReplaceAll(raw, []byte("\r"), []byte("\n"))
	return string(raw), nil
}

// pairsFromBindings keeps the bindings that carry a key, warning about the
// statements that failed to parse (with_warn_for_invalid_lines).
func pairsFromBindings(bindings []Binding) []Pair {
	out := make([]Pair, 0, len(bindings))
	for _, b := range bindings {
		if b.Error {
			log.Warn(fmt.Sprintf("python-dotenv could not parse statement starting at line %d", b.Original.Line))
		}
		if b.Key != nil {
			out = append(out, Pair{Key: *b.Key, Value: b.Value})
		}
	}
	return out
}

// Parse returns every key binding in r in file order, without interpolation
// and without collapsing repeated keys. Lines that do not parse are logged
// and skipped.
func Parse(r io.Reader) ([]Pair, error) {
	text, err := readText(r)
	if err != nil {
		return nil, err
	}
	return pairsFromBindings(ParseBindings(text)), nil
}

// dedupe collapses repeated keys the way a Python dict does: a key keeps the
// position of its first occurrence and the value of its last.
func dedupe(pairs []Pair) []Pair {
	index := make(map[string]int, len(pairs))
	out := make([]Pair, 0, len(pairs))
	for _, p := range pairs {
		if i, ok := index[p.Key]; ok {
			out[i].Value = p.Value
			continue
		}
		index[p.Key] = len(out)
		out = append(out, p)
	}
	return out
}

// environ snapshots the process environment.
func environ() map[string]*string {
	env := make(map[string]*string)
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = &v
		}
	}
	return env
}

// Resolve expands ${VAR} references in pairs in order (resolve_variables).
// Each value sees the bindings above it and the process environment; with
// override the file's earlier bindings win over the environment, without it
// the environment wins (what load_dotenv(override=False) needs: a variable
// that will not be overwritten must also be what later lines expand to).
// The result is deduplicated like a dict.
func Resolve(pairs []Pair, override bool) []Pair {
	osEnv := environ()
	resolved := make(map[string]*string, len(pairs))
	out := make([]Pair, 0, len(pairs))
	for _, p := range pairs {
		var result *string
		if p.Value != nil {
			env := make(map[string]*string, len(osEnv)+len(resolved))
			first, second := osEnv, resolved
			if !override {
				first, second = resolved, osEnv
			}
			for k, v := range first {
				env[k] = v
			}
			for k, v := range second {
				env[k] = v
			}
			var b strings.Builder
			for _, atom := range ParseVariables(*p.Value) {
				b.WriteString(atom.resolve(env))
			}
			s := b.String()
			result = &s
		}
		resolved[p.Key] = result
		out = append(out, Pair{Key: p.Key, Value: result})
	}
	return dedupe(out)
}

// Values is dotenv_values(stream=r, interpolate=interpolate): the file's
// bindings in dict order (first position, last value), with ${VAR}
// references expanded file-first when interpolate is set.
func Values(r io.Reader, interpolate bool) ([]Pair, error) {
	pairs, err := Parse(r)
	if err != nil {
		return nil, err
	}
	if interpolate {
		return Resolve(pairs, true), nil
	}
	return dedupe(pairs), nil
}

// ValuesFile is Values on the file at path.
func ValuesFile(path string, interpolate bool) ([]Pair, error) {
	f, err := os.Open(path) //nolint:gosec // operator-chosen .env path
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Values(f, interpolate)
}

// ReadFile parses path with ${VAR} interpolation (dotenv_values(path)).
func ReadFile(path string) ([]Pair, error) { return ValuesFile(path, true) }

// Map turns pairs into a lookup table (later duplicates win).
func Map(pairs []Pair) map[string]*string {
	m := make(map[string]*string, len(pairs))
	for _, p := range pairs {
		m[p.Key] = p.Value
	}
	return m
}

// Disabled reports whether PYTHON_DOTENV_DISABLED turns .env loading off.
// The variable name is kept from the Python release so existing unit files
// and shells keep working.
func Disabled() bool {
	v, ok := os.LookupEnv("PYTHON_DOTENV_DISABLED")
	if !ok {
		return false
	}
	switch strings.ToLower(v) {
	case "1", "true", "t", "yes", "y":
		return true
	}
	return false
}

// isFileOrFIFO is upstream's _is_file_or_fifo: path exists and is a regular
// file or a named pipe. Any stat error means no.
func isFileOrFIFO(path string) bool {
	st, err := os.Stat(path)
	if err != nil {
		return false
	}
	return st.Mode().IsRegular() || st.Mode()&fs.ModeNamedPipe != 0
}

// LoadReader sets the bindings read from r into the process environment
// (load_dotenv(stream=r)). Variables already set are kept unless override.
// It reports whether the file held any binding at all.
func LoadReader(r io.Reader, override bool) (bool, error) {
	if Disabled() {
		log.Debug("python-dotenv: .env loading disabled by PYTHON_DOTENV_DISABLED environment variable")
		return false, nil
	}
	pairs, err := Parse(r)
	if err != nil {
		return false, err
	}
	return apply(Resolve(pairs, override), override), nil
}

func apply(pairs []Pair, override bool) bool {
	if len(pairs) == 0 {
		return false
	}
	for _, p := range pairs {
		if _, set := os.LookupEnv(p.Key); set && !override {
			continue
		}
		if p.Value != nil {
			_ = os.Setenv(p.Key, *p.Value)
		}
	}
	return true
}

// Load sets every binding in path into the process environment. Existing
// variables win unless override is set. A path that is not a file (missing,
// a directory) loads nothing and is not an error, as upstream; a file that
// cannot be read (a root-owned 0600 .env) is.
func Load(path string, override bool) error {
	if Disabled() {
		log.Debug("python-dotenv: .env loading disabled by PYTHON_DOTENV_DISABLED environment variable")
		return nil
	}
	if path == "" || !isFileOrFIFO(path) {
		return nil
	}
	f, err := os.Open(path) //nolint:gosec // operator-chosen .env path
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = LoadReader(f, override)
	return err
}

// Find returns the nearest .env at or above the working directory, or "".
func Find() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	return FindFrom(dir, ".env")
}

// FindFrom returns the first file called filename in dir or one of its
// parents that is a regular file or FIFO, or "" when there is none.
func FindFrom(dir, filename string) string {
	if isFileOrFIFO(dir) {
		dir = filepath.Dir(dir)
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	for {
		candidate := filepath.Join(dir, filename)
		if isFileOrFIFO(candidate) {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// QuoteMode says when SetKey wraps the value in single quotes.
type QuoteMode int

// Quote modes, as set_key's quote_mode.
const (
	QuoteAlways QuoteMode = iota // always (the default)
	QuoteAuto                    // unless the value is non-empty and alphanumeric
	QuoteNever                   // never
)

// SetKeyOptions are set_key's optional arguments.
type SetKeyOptions struct {
	Quote QuoteMode
	// Export writes "export KEY=..." instead of "KEY=...".
	Export bool
	// FollowSymlinks rewrites a symlink's target. Off by default, so a .env
	// symlinked somewhere untrusted is replaced by a regular file instead of
	// writing through the link.
	FollowSymlinks bool
}

func isAlnum(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !unicode.IsLetter(c) && !unicode.IsDigit(c) && !unicode.IsNumber(c) {
			return false
		}
	}
	return true
}

// SetKey adds or replaces key in the .env at path (creating the file), the
// way bootstrap writes secrets. The file is rewritten through a temporary
// file in the same directory and renamed into place, keeping a regular
// file's permission bits; a new file (or a replaced symlink) gets 0600.
func SetKey(path, key, value string, opts SetKeyOptions) error {
	quote := opts.Quote == QuoteAlways || (opts.Quote == QuoteAuto && !isAlnum(value))
	valueOut := value
	if quote {
		// The single-quoted-value parser decodes `\\` and `\'`, so both have
		// to be escaped for the value to survive a write/read round trip.
		// Backslashes first, or the quote escaping's own backslash would be
		// escaped in turn.
		escaped := strings.ReplaceAll(value, `\`, `\\`)
		escaped = strings.ReplaceAll(escaped, `'`, `\'`)
		valueOut = "'" + escaped + "'"
	}
	lineOut := key + "=" + valueOut + "\n"
	if opts.Export {
		lineOut = "export " + lineOut
	}

	if opts.FollowSymlinks {
		path = realpath(path)
	}
	source := ""
	var mode fs.FileMode
	keepMode := false
	f, err := os.Open(path) //nolint:gosec // operator-chosen .env path
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return err
	default:
		source, err = readText(f)
		_ = f.Close()
		if err != nil {
			return err
		}
		if st, lerr := os.Lstat(path); lerr == nil && st.Mode().IsRegular() {
			mode, keepMode = st.Mode().Perm(), true
		}
	}

	var out strings.Builder
	replaced, missingNewline := false, false
	for _, b := range ParseBindings(source) {
		if b.Error {
			log.Warn(fmt.Sprintf("python-dotenv could not parse statement starting at line %d", b.Original.Line))
		}
		if b.Key != nil && *b.Key == key {
			out.WriteString(lineOut)
			replaced = true
			continue
		}
		out.WriteString(b.Original.String)
		missingNewline = !strings.HasSuffix(b.Original.String, "\n")
	}
	if !replaced {
		if missingNewline {
			out.WriteString("\n")
		}
		out.WriteString(lineOut)
	}
	return replaceFile(path, out.String(), mode, keepMode)
}

// realpath resolves symlinks in path even when the final target does not
// exist yet (os.path.realpath semantics, which EvalSymlinks lacks).
func realpath(path string) string {
	if p, err := filepath.EvalSymlinks(path); err == nil {
		return p
	}
	for range 40 {
		target, err := os.Readlink(path)
		if err != nil {
			return path
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		path = target
	}
	return path
}

func replaceFile(path, content string, mode fs.FileMode, keepMode bool) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(abs), ".tmp_")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if keepMode {
		if err := os.Chmod(tmpName, mode); err != nil {
			_ = os.Remove(tmpName)
			return err
		}
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}
