// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package resultcsv prepares result CSVs for download. The pipeline records
// each row's OpenRouter spend in a Cost_USD column; the file on disk keeps it
// (operators and resumed runs read it), but customers download results
// without it.
package resultcsv

import (
	"bytes"
	"strings"
)

// CostColumn is the per-row spend column every download drops.
const CostColumn = "Cost_USD"

// WithoutCost returns data with the Cost_USD column removed from the header
// and every row. Everything else is kept byte for byte (quoting, line
// endings, line breaks inside quoted fields), so the rest of the file is
// exactly what the pipeline wrote. Data without that header column is
// returned unchanged.
func WithoutCost(data []byte) []byte {
	return WithoutColumn(data, CostColumn)
}

// WithoutColumn removes the column named name (matched on the first row,
// the header) from every record of a comma-separated, '"'-quoted file.
func WithoutColumn(data []byte, name string) []byte {
	records := split(data)
	if len(records) == 0 {
		return data
	}
	col := -1
	for i, f := range records[0].fields {
		if unquote(data[f.start:f.end]) == name {
			col = i
			break
		}
	}
	if col < 0 {
		return data
	}
	var out bytes.Buffer
	out.Grow(len(data))
	last := 0
	for _, rec := range records {
		if col >= len(rec.fields) {
			continue // short row: nothing to drop
		}
		f := rec.fields[col]
		// Cut the field with one adjacent comma: the one before it, or, for
		// the first field, the one after it.
		cutStart, cutEnd := f.start, f.end
		switch {
		case col > 0:
			cutStart = rec.fields[col-1].end
		case len(rec.fields) > 1:
			cutEnd = rec.fields[1].start
		}
		out.Write(data[last:cutStart])
		last = cutEnd
	}
	out.Write(data[last:])
	return out.Bytes()
}

type span struct{ start, end int }

type record struct{ fields []span }

// split finds the field spans of every record. A field span covers the raw
// bytes between separators, quotes included; the separators (',' and the
// "\n" or "\r\n" ending a record) are outside every span.
func split(data []byte) []record {
	var records []record
	var cur record
	start := 0
	inQuotes := false
	for i := 0; i < len(data); i++ {
		c := data[i]
		if inQuotes {
			if c == '"' {
				if i+1 < len(data) && data[i+1] == '"' {
					i++
				} else {
					inQuotes = false
				}
			}
			continue
		}
		switch c {
		case '"':
			inQuotes = true
		case ',':
			cur.fields = append(cur.fields, span{start, i})
			start = i + 1
		case '\n':
			end := i
			if end > start && data[end-1] == '\r' {
				end--
			}
			cur.fields = append(cur.fields, span{start, end})
			records = append(records, cur)
			cur = record{}
			start = i + 1
		}
	}
	if start < len(data) || len(cur.fields) > 0 {
		end := len(data)
		if end > start && data[end-1] == '\r' {
			end--
		}
		cur.fields = append(cur.fields, span{start, end})
		records = append(records, cur)
	}
	return records
}

func unquote(field []byte) string {
	s := string(field)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = strings.ReplaceAll(s[1:len(s)-1], `""`, `"`)
	}
	return s
}
