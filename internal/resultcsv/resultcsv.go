// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package resultcsv prepares result CSVs for download. The pipeline records
// each row's OpenRouter spend in a Cost_USD column; the file on disk keeps it
// (operators and resumed runs read it), but customers download results
// without it.
package resultcsv

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
)

// CostColumn is the per-row spend column every download drops.
const CostColumn = "Cost_USD"

// Copy writes src to dst with the Cost_USD column removed from the header
// and every row, and returns the bytes written. Everything else is kept byte
// for byte (quoting, line endings, line breaks inside quoted fields), so the
// rest of the file is exactly what the pipeline wrote; a file without that
// header column is copied unchanged. It works one record at a time, so
// memory stays bounded by the longest row, not the file.
func Copy(dst io.Writer, src io.Reader) (int64, error) {
	return copyWithout(dst, src, CostColumn)
}

// Size is the length of src once Copy has removed the cost column.
func Size(src io.Reader) (int64, error) {
	return Copy(io.Discard, src)
}

// WithoutCost is Copy over an in-memory file.
func WithoutCost(data []byte) []byte {
	var out bytes.Buffer
	_, _ = Copy(&out, bytes.NewReader(data))
	return out.Bytes()
}

func copyWithout(dst io.Writer, src io.Reader, name string) (int64, error) {
	in := bufio.NewReaderSize(src, 64<<10)
	out := &countingWriter{w: bufio.NewWriterSize(dst, 64<<10)}
	header, err := readRecord(in)
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, err
	}
	col := -1
	for i, f := range split(header) {
		if unquote(header[f.start:f.end]) == name {
			col = i
			break
		}
	}
	if col < 0 {
		_, _ = out.Write(header)
		if _, err := io.Copy(out, in); err != nil {
			return out.n, err
		}
		return out.n, out.w.Flush()
	}
	rec := header
	for len(rec) > 0 {
		_, _ = out.Write(cut(rec, col))
		if errors.Is(err, io.EOF) {
			break
		}
		rec, err = readRecord(in)
		if err != nil && !errors.Is(err, io.EOF) {
			return out.n, err
		}
	}
	return out.n, out.w.Flush()
}

type countingWriter struct {
	w *bufio.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// readRecord returns the next raw record, its "\n" included: bytes up to a
// newline outside quotes. At the end of input it returns what is left with
// io.EOF.
func readRecord(in *bufio.Reader) ([]byte, error) {
	var rec []byte
	inQuotes := false
	for {
		c, err := in.ReadByte()
		if err != nil {
			return rec, err
		}
		rec = append(rec, c)
		switch {
		case c == '"':
			// A doubled quote inside quotes toggles twice: still inside.
			inQuotes = !inQuotes
		case c == '\n' && !inQuotes:
			return rec, nil
		}
	}
}

// cut removes field col from one raw record, with one adjacent comma: the
// one before it, or, for the first field, the one after it. A record too
// short to have that field is returned as is.
func cut(rec []byte, col int) []byte {
	fields := split(rec)
	if col >= len(fields) {
		return rec
	}
	start, end := fields[col].start, fields[col].end
	switch {
	case col > 0:
		start = fields[col-1].end
	case len(fields) > 1:
		end = fields[1].start
	}
	return append(rec[:start:start], rec[end:]...)
}

type span struct{ start, end int }

// split finds the field spans of one raw record. A span covers the raw
// bytes between separators, quotes included; the separators (',' and the
// "\n" or "\r\n" ending the record) are outside every span.
func split(rec []byte) []span {
	var fields []span
	start := 0
	inQuotes := false
	for i, c := range rec {
		switch {
		case c == '"':
			inQuotes = !inQuotes
		case inQuotes:
		case c == ',':
			fields = append(fields, span{start, i})
			start = i + 1
		case c == '\n':
			end := i
			if end > start && rec[end-1] == '\r' {
				end--
			}
			return append(fields, span{start, end})
		}
	}
	if start < len(rec) || len(fields) > 0 {
		fields = append(fields, span{start, len(rec)})
	}
	return fields
}

func unquote(field []byte) string {
	s := string(field)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = strings.ReplaceAll(s[1:len(s)-1], `""`, `"`)
	}
	return s
}
