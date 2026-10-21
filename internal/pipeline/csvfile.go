// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package pipeline

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/ElcanoTek/lens/internal/categories"
)

// appendCSVRow appends one row the way Python's csv.writer did with its
// defaults (QUOTE_MINIMAL, '"' doubled, "\r\n" terminator): a field is
// quoted only when it contains the delimiter, a quote or a line break, and a
// row made of a single empty field is written as "" so it reads back as a
// row. encoding/csv differs (it also quotes fields with a leading space), and
// output files must stay byte-compatible with the Python release's.
func appendCSVRow(b *bytes.Buffer, fields []string) {
	if len(fields) == 1 && fields[0] == "" {
		b.WriteString("\"\"\r\n")
		return
	}
	for i, f := range fields {
		if i > 0 {
			b.WriteByte(',')
		}
		if strings.ContainsAny(f, ",\"\r\n") {
			b.WriteByte('"')
			b.WriteString(strings.ReplaceAll(f, `"`, `""`))
			b.WriteByte('"')
		} else {
			b.WriteString(f)
		}
	}
	b.WriteString("\r\n")
}

// ResultsWriter appends result rows to the output CSV. It replaces the
// Python csv.DictWriter + file pair; the mutex stands in for the asyncio
// single thread the Python relied on, so concurrent items never interleave
// partial rows. Each row is written with a single write call (the Python
// flushed after every row), so a crash loses at most the row in flight.
type ResultsWriter struct {
	mu      sync.Mutex
	w       io.Writer
	closer  io.Closer
	columns []string
}

// NewResultsWriter writes rows with the given columns to w (no header).
func NewResultsWriter(w io.Writer, columns []string) *ResultsWriter {
	return &ResultsWriter{w: w, columns: append([]string(nil), columns...)}
}

// Columns returns the writer's column order.
func (rw *ResultsWriter) Columns() []string { return append([]string(nil), rw.columns...) }

// WriteHeader writes the header row.
func (rw *ResultsWriter) WriteHeader() error {
	var b bytes.Buffer
	appendCSVRow(&b, rw.columns)
	rw.mu.Lock()
	defer rw.mu.Unlock()
	_, err := rw.w.Write(b.Bytes())
	return err
}

// WriteRow writes one record; absent columns are blank (DictWriter's
// restval), columns outside the schema are ignored.
func (rw *ResultsWriter) WriteRow(rec *Record) error {
	fields := make([]string, len(rw.columns))
	for i, c := range rw.columns {
		fields[i] = rec.String(c)
	}
	var b bytes.Buffer
	appendCSVRow(&b, fields)
	rw.mu.Lock()
	defer rw.mu.Unlock()
	if rw.w == nil {
		return errors.New("Results writer has not been initialised") //nolint:staticcheck // message kept from the Python release
	}
	_, err := rw.w.Write(b.Bytes())
	return err
}

// Close closes the underlying file, if the writer owns one.
func (rw *ResultsWriter) Close() error {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	rw.w = nil
	if rw.closer != nil {
		err := rw.closer.Close()
		rw.closer = nil
		return err
	}
	return nil
}

// readCSVDicts reads a CSV like csv.DictReader: the first row is the header,
// short rows leave the missing columns blank, extra fields are dropped.
func readCSVDicts(path string) (header []string, rows []map[string]string, err error) {
	f, err := os.Open(path) //nolint:gosec // operator-chosen output path
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = f.Close() }()
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	header, err = r.Read()
	if errors.Is(err, io.EOF) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		row := make(map[string]string, len(header))
		for i, name := range header {
			if i < len(rec) {
				// DictReader: a later duplicate column name wins.
				row[name] = rec[i]
			} else if _, ok := row[name]; !ok {
				row[name] = ""
			}
		}
		rows = append(rows, row)
	}
	return header, rows, nil
}

// writeCSVDicts rewrites path with header and rows (missing cells blank).
func writeCSVDicts(path string, header []string, rows []map[string]string) error {
	var b bytes.Buffer
	appendCSVRow(&b, header)
	fields := make([]string, len(header))
	for _, row := range rows {
		for i, c := range header {
			fields[i] = row[c]
		}
		appendCSVRow(&b, fields)
	}
	return os.WriteFile(path, b.Bytes(), 0o644) //nolint:gosec // output CSVs are downloaded through the dashboard
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil || !errors.Is(err, fs.ErrNotExist)
}

// openResultsFile creates the output CSV with a header when missing (else
// migrates an older header to columns) and opens it for appending.
func openResultsFile(path string, columns []string) (*ResultsWriter, error) {
	if !fileExists(path) {
		var b bytes.Buffer
		appendCSVRow(&b, columns)
		if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil { //nolint:gosec // output CSVs are downloaded through the dashboard
			return nil, err
		}
	} else {
		migrateOutputCSVSchema(path, columns)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644) //nolint:gosec // operator-chosen output path
	if err != nil {
		return nil, err
	}
	rw := NewResultsWriter(f, columns)
	rw.closer = f
	return rw, nil
}

// migrateOutputCSVSchema rewrites an output CSV left by an older version to
// the current schema.
//
// Resumed runs append to the existing file, so a header from before a
// column was added (e.g. Bot_Protection) would silently misalign every new
// row. Old rows get "" for columns they never had, and keep values from
// columns that were renamed (categories.LegacyColumnNames).
func migrateOutputCSVSchema(path string, columns []string) {
	header, rows, err := readCSVDicts(path)
	if err == nil && header != nil && slices.Equal(header, columns) {
		return
	}
	if err == nil {
		for _, row := range rows {
			for old, current := range categories.LegacyColumnNames {
				if v, ok := row[old]; ok {
					delete(row, old)
					if _, exists := row[current]; !exists {
						row[current] = v
					}
				}
			}
		}
		logger.Info(fmt.Sprintf("Output CSV uses an older column layout; rewriting %d row(s) to the current schema", len(rows)))
		err = writeCSVDicts(path, columns, rows)
	}
	if err != nil {
		logger.Warn(fmt.Sprintf("Could not migrate output CSV schema: %v", err))
	}
}

// dedupeOutputCSV collapses retried rows: one row per identifier, preferring
// success.
//
// The deep-crawl and research retries append a second row for each domain
// they rescue; the original Failed row is dropped here so downstream
// consumers see a single, best-effort result per identifier.
func dedupeOutputCSV(path string) {
	if !fileExists(path) {
		return
	}
	header, rows, err := readCSVDicts(path)
	if err != nil {
		logger.Warn(fmt.Sprintf("Could not dedupe output CSV: %v", err))
		return
	}
	if len(header) == 0 || !slices.Contains(header, "Domain") {
		return
	}
	best := map[string]map[string]string{}
	var order []string
	for _, row := range rows {
		key := row["Domain"]
		current, seen := best[key]
		if !seen {
			order = append(order, key)
			best[key] = row
			continue
		}
		// A later row wins unless it is a failure replacing a success.
		if row["Quality"] == "Failed" && current["Quality"] != "Failed" {
			continue
		}
		best[key] = row
	}
	if len(best) == len(rows) {
		return
	}
	deduped := make([]map[string]string, 0, len(order))
	for _, key := range order {
		deduped = append(deduped, best[key])
	}
	if err := writeCSVDicts(path, header, deduped); err != nil {
		logger.Warn(fmt.Sprintf("Could not dedupe output CSV: %v", err))
		return
	}
	logger.Info(fmt.Sprintf("Deduplicated output CSV: %d rows -> %d unique identifiers", len(rows), len(best)))
}
