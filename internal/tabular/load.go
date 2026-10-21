// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package tabular

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// maxCSVBytes bounds a CSV read into memory. Uploads are capped far lower;
// this only stops a runaway CLI input from exhausting memory.
const maxCSVBytes = 1 << 30

// ErrUnsupported is returned for a path that is neither .csv nor .xlsx. Its
// message is the Python release's, which the dashboard shows verbatim.
var ErrUnsupported error = unsupportedError{}

type unsupportedError struct{}

func (unsupportedError) Error() string { return "Input file must be a .csv or .xlsx file" }

// Load reads a .csv or .xlsx input file into a Frame: the loader both the
// pipeline and the dashboard's breakdown preview use. CSVs are decoded as
// UTF-8 with a BOM stripped (invalid bytes become U+FFFD, with a warning),
// re-read with ';' or tab when that is evidently the delimiter, and both
// formats lose their empty "Unnamed" artifact columns.
//
// log receives the loader's progress lines (the Python release's
// orchestration messages); nil keeps it quiet.
func Load(path string, log *slog.Logger) (*Frame, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	var (
		f   *Frame
		err error
	)
	switch strings.ToLower(filepath.Ext(path)) {
	case ".xlsx":
		f, err = loadXLSX(path)
	case ".csv":
		f, err = loadCSV(path, log)
	default:
		return nil, ErrUnsupported
	}
	if err != nil {
		return nil, err
	}
	f, dropped := DropEmptyColumns(f)
	if dropped > 0 {
		log.Info(fmt.Sprintf("Dropped %d empty spreadsheet columns", dropped))
	}
	log.Info(fmt.Sprintf("Loaded %d rows from %s", f.NumRows(), path))
	return f, nil
}

func loadCSV(path string, log *slog.Logger) (*Frame, error) {
	fh, err := os.Open(path) //nolint:gosec // operator-chosen input path
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	raw, err := io.ReadAll(io.LimitReader(fh, maxCSVBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxCSVBytes {
		return nil, fmt.Errorf("%s is larger than %d MB", path, maxCSVBytes>>20)
	}
	return ParseCSV(raw, path, log)
}

// ParseCSV parses CSV bytes (name is only used in log lines).
func ParseCSV(raw []byte, name string, log *slog.Logger) (*Frame, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	text, problem := decodeText(raw)
	if problem != "" {
		log.Warn(fmt.Sprintf("Failed to decode %s as UTF-8 (%s). Retrying with replacement characters.", name, problem))
	}
	f, delim := parseCSV(text)
	if f.NumColumns() == 0 {
		return nil, errors.New("no columns to parse from file")
	}
	if delim != ',' {
		log.Info(fmt.Sprintf("Re-read %s with sniffed delimiter (%d columns)", name, f.NumColumns()))
	}
	if problem != "" {
		log.Info(fmt.Sprintf("Loaded %d rows from %s with replacement characters for invalid bytes", f.NumRows(), name))
	}
	return f, nil
}

func loadXLSX(path string) (*Frame, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, fmt.Errorf("%s is not a readable .xlsx workbook: %w", path, err)
	}
	defer zr.Close()
	return readXLSX(&zr.Reader)
}

// ParseXLSX parses an .xlsx workbook held in memory: its first worksheet,
// first non-blank row as the header.
func ParseXLSX(data []byte) (*Frame, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("not a readable .xlsx workbook: %w", err)
	}
	return readXLSX(zr)
}
