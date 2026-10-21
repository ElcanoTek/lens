// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package jobs

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Upload limits shared by the dashboard, the API and `lens submit`.
const (
	MaxUploadBytes = 100 * 1024 * 1024
	// JobsStateFilename lives in the outputs directory; it is never exposed
	// to file operations.
	JobsStateFilename = "_jobs.json"
	// BreakdownCacheDirname holds per-file content breakdowns. A directory,
	// not a file, so input listings skip it.
	BreakdownCacheDirname = ".breakdowns"
)

var xlsxMagic = []byte("PK\x03\x04")

// Dirs are the managed-files directories under the Lens home.
type Dirs struct {
	Inputs  string
	Outputs string
	// Spool is where `lens submit` hands requests to the running service:
	// one subdirectory per request. Only root and the service user can write.
	Spool string
}

// DirsFor returns the managed-files layout under home.
func DirsFor(home string) Dirs {
	base := filepath.Join(home, "managed-files")
	return Dirs{
		Inputs:  filepath.Join(base, "inputs"),
		Outputs: filepath.Join(base, "outputs"),
		Spool:   filepath.Join(base, "spool"),
	}
}

// Ensure creates the directories (spool 0700).
func (d Dirs) Ensure() error {
	for _, dir := range []string{d.Inputs, d.Outputs} {
		if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // managed-files is readable by the operator, as before
			return err
		}
	}
	if err := os.MkdirAll(d.Spool, 0o700); err != nil {
		return err
	}
	return nil
}

// StatePath is the _jobs.json path.
func (d Dirs) StatePath() string { return filepath.Join(d.Outputs, JobsStateFilename) }

var unsafeFilenameChars = regexp.MustCompile(`[^A-Za-z0-9._ ()-]`)

// SafeFilename sanitizes a user-supplied filename. Path separators and empty
// names are rejected (the security-critical guards); other characters are
// replaced rather than rejected, because real filenames routinely carry
// "(2)", ampersands or accents.
func SafeFilename(name string) (string, error) {
	if strings.ContainsAny(name, `/\`) {
		return "", errors.New("Filename must not contain path separators") //nolint:revive,staticcheck // user-facing message, kept from the Python release
	}
	cleaned := strings.TrimSpace(filepath.Base(name))
	if name == "" || cleaned == "" || cleaned == "." {
		return "", errors.New("Filename cannot be empty") //nolint:revive,staticcheck // user-facing message
	}
	sanitized := unsafeFilenameChars.ReplaceAllString(cleaned, "_")
	if strings.Trim(sanitized, "._ -") == "" {
		return "", errors.New("Filename cannot be empty") //nolint:revive,staticcheck // user-facing message
	}
	return sanitized, nil
}

// SafeFilenameOrEmpty sanitizes a filename read from persisted state; ""
// when missing or unsafe.
func SafeFilenameOrEmpty(name string) string {
	if name == "" {
		return ""
	}
	s, err := SafeFilename(name)
	if err != nil {
		return ""
	}
	return s
}

// IsProtectedOutput reports whether a file op targets the job-state file.
func IsProtectedOutput(destination, filename string) bool {
	return destination == "outputs" && filename == JobsStateFilename
}

// ValidateInputFilename sanitizes name and requires .csv or .xlsx.
func ValidateInputFilename(name string) (string, error) {
	cleaned, err := SafeFilename(name)
	if err != nil {
		return "", err
	}
	switch strings.ToLower(filepath.Ext(cleaned)) {
	case ".csv", ".xlsx":
		return cleaned, nil
	}
	return "", errors.New("Input files must be .csv or .xlsx") //nolint:revive,staticcheck // user-facing message
}

// VerifyInputPayload sanity-checks upload bytes against the extension.
func VerifyInputPayload(filename string, payload []byte) error {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".xlsx":
		if !bytes.HasPrefix(payload, xlsxMagic) {
			return errors.New("File does not look like a valid .xlsx workbook (wrong file contents)") //nolint:revive,staticcheck // user-facing message
		}
	case ".csv":
		// CSV has no magic bytes, but a NUL in the leading sample reliably
		// marks a binary file mislabeled as CSV.
		sample := payload
		if len(sample) > 4096 {
			sample = sample[:4096]
		}
		if bytes.IndexByte(sample, 0) >= 0 {
			return errors.New("File does not look like a valid CSV (binary contents detected)") //nolint:revive,staticcheck // user-facing message
		}
	}
	return nil
}

// NormalizeJobName collapses whitespace and caps a job name at 120 runes;
// "" means no name.
func NormalizeJobName(raw string) string {
	collapsed := strings.Join(strings.Fields(raw), " ")
	r := []rune(collapsed)
	if len(r) > 120 {
		r = r[:120]
	}
	return string(r)
}

var modelIDRe = regexp.MustCompile(`^[~A-Za-z0-9/._:-]+$`)

// CleanModelID trims a model id; "" for none, an error for garbage. Model
// IDs feed subprocess argv (no shell), but obvious garbage stays out.
func CleanModelID(value string) (string, error) {
	m := strings.TrimSpace(value)
	if m == "" {
		return "", nil
	}
	if !modelIDRe.MatchString(m) {
		return "", fmt.Errorf("Invalid model id: %s", m) //nolint:revive,staticcheck // user-facing message
	}
	return m, nil
}

// StoreUniqueInput writes payload into the input library and returns the
// name it got: the requested name unless a different file already has it,
// then "name (2).csv", "name (3).csv", ... A name is claimed with
// O_CREATE|O_EXCL, so two submissions racing for one name can never both
// write the same path. Identical bytes reuse the existing file.
func StoreUniqueInput(inputsDir, filename string, payload []byte) (string, error) {
	ext := filepath.Ext(filename)
	stem := strings.TrimSuffix(filename, ext)
	candidate := filename
	for n := 1; ; {
		path := filepath.Join(inputsDir, candidate)
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) //nolint:gosec // inputs are readable by the operator
		if errors.Is(err, fs.ErrExist) {
			if existing, rerr := os.ReadFile(path); rerr == nil && bytes.Equal(existing, payload) { //nolint:gosec // path is inside the library
				return candidate, nil
			}
			n++
			candidate = fmt.Sprintf("%s (%d)%s", stem, n, ext)
			continue
		}
		if err != nil {
			return "", err
		}
		if _, err := f.Write(payload); err != nil {
			_ = f.Close()
			_ = os.Remove(path)
			return "", err
		}
		if err := f.Close(); err != nil {
			_ = os.Remove(path)
			return "", err
		}
		return candidate, nil
	}
}
