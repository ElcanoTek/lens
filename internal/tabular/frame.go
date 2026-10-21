// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package tabular loads the input files Lens is given (.csv and .xlsx) into
// a small string table, standing in for the pandas DataFrames the Python
// release read them into.
//
// Every cell is kept as the exact text in the file. pandas inferred column
// types, which turned a numeric column with a blank cell into floats
// ("512001.0"), dropped leading zeros and shifted values into an implicit
// index when rows were longer than the header; reading text avoids all of
// that. What pandas did that the detection heuristics depend on is kept:
// the first non-blank line is the header, blank header cells are named
// "Unnamed: N", duplicate names become "X", "X.1", ..., the strings pandas
// reads as NaN count as missing (IsNA), a semicolon/tab export is re-read
// with its real delimiter, and empty artifact columns are dropped.
package tabular

import (
	"fmt"
	"strings"
	"unicode"
)

// Frame is an ordered table of text cells with named columns. Rows may be
// stored shorter than the header; the missing trailing cells read as "".
// A Frame is immutable once built and safe for concurrent reads.
type Frame struct {
	columns []string
	rows    [][]string
}

// New builds a Frame from column names and rows. Rows longer than columns
// are truncated; shorter rows read as padded with "". The slices are copied.
func New(columns []string, rows [][]string) *Frame {
	f := &Frame{columns: append([]string(nil), columns...), rows: make([][]string, 0, len(rows))}
	for _, r := range rows {
		if len(r) > len(columns) {
			r = r[:len(columns)]
		}
		f.rows = append(f.rows, append([]string(nil), r...))
	}
	return f
}

// Columns returns the column names, in file order.
func (f *Frame) Columns() []string { return append([]string(nil), f.columns...) }

// NumColumns is the number of columns.
func (f *Frame) NumColumns() int { return len(f.columns) }

// NumRows is the number of data rows (the header is not a row).
func (f *Frame) NumRows() int { return len(f.rows) }

// Empty reports what pandas' DataFrame.empty did: no rows or no columns.
func (f *Frame) Empty() bool { return len(f.rows) == 0 || len(f.columns) == 0 }

// Index returns the position of the first column called name, or -1.
func (f *Frame) Index(name string) int {
	for i, c := range f.columns {
		if c == name {
			return i
		}
	}
	return -1
}

// Has reports whether a column called name exists.
func (f *Frame) Has(name string) bool { return f.Index(name) >= 0 }

// Cell returns the raw text at row r, column c ("" past the row's end).
func (f *Frame) Cell(r, c int) string {
	row := f.rows[r]
	if c < len(row) {
		return row[c]
	}
	return ""
}

// Missing reports whether the cell is what pandas would have read as NaN.
func (f *Frame) Missing(r, c int) bool { return IsNA(f.Cell(r, c)) }

// Row returns row r padded to the full width (a copy).
func (f *Frame) Row(r int) []string {
	out := make([]string, len(f.columns))
	copy(out, f.rows[r])
	return out
}

// Column returns every cell of column c (a copy), missing ones included.
func (f *Frame) Column(c int) []string {
	out := make([]string, len(f.rows))
	for r := range f.rows {
		out[r] = f.Cell(r, c)
	}
	return out
}

// Present returns the non-missing cells of column c in row order:
// pandas' df[col].dropna().astype(str).
func (f *Frame) Present(c int) []string {
	out := make([]string, 0, len(f.rows))
	for r := range f.rows {
		if v := f.Cell(r, c); !IsNA(v) {
			out = append(out, v)
		}
	}
	return out
}

// RowMap returns row r as column name -> text for the non-missing cells,
// like {k: str(v) for k, v in row.items() if pd.notna(v)}. With duplicate
// names (impossible after Load, which disambiguates) the last cell wins.
func (f *Frame) RowMap(r int) map[string]string {
	m := make(map[string]string, len(f.columns))
	for c, name := range f.columns {
		if v := f.Cell(r, c); !IsNA(v) {
			m[name] = v
		}
	}
	return m
}

// Select returns a Frame with only the given columns, in that order.
func (f *Frame) Select(cols []int) *Frame {
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = f.columns[c]
	}
	rows := make([][]string, len(f.rows))
	for r := range f.rows {
		row := make([]string, len(cols))
		for i, c := range cols {
			row[i] = f.Cell(r, c)
		}
		rows[r] = row
	}
	return &Frame{columns: names, rows: rows}
}

// SelectRows returns a Frame with only the given rows, in that order.
func (f *Frame) SelectRows(rows []int) *Frame {
	out := &Frame{columns: f.columns, rows: make([][]string, len(rows))}
	for i, r := range rows {
		out.rows[i] = f.rows[r]
	}
	return out
}

// naValues is pandas' default STR_NA_VALUES: cell texts read_csv and
// read_excel turn into NaN unless told otherwise. Matching is exact (no
// trimming), as in pandas.
var naValues = map[string]struct{}{
	"": {}, "#N/A": {}, "#N/A N/A": {}, "#NA": {}, "-1.#IND": {}, "-1.#QNAN": {},
	"-NaN": {}, "-nan": {}, "1.#IND": {}, "1.#QNAN": {}, "<NA>": {}, "N/A": {},
	"NA": {}, "NULL": {}, "NaN": {}, "None": {}, "n/a": {}, "nan": {}, "null": {},
}

// IsNA reports whether pandas would have read the cell text s as missing:
// an empty cell or one of its default NA strings ("NA", "N/A", "null",
// "None", "nan", "#N/A", ...). Placeholder rows like "N/A" in a domain list
// therefore never become work items, as before.
func IsNA(s string) bool {
	_, ok := naValues[s]
	return ok
}

// IsSpace is Python's str.isspace() for one rune: Go's unicode.IsSpace plus
// the ASCII separators U+001C..U+001F, which Python also strips.
func IsSpace(c rune) bool { return unicode.IsSpace(c) || (c >= 0x1c && c <= 0x1f) }

// Strip is Python's str.strip().
func Strip(s string) string { return strings.TrimFunc(s, IsSpace) }

// headerNames turns raw header cells into pandas column names, exactly as
// pandas' parsers do: a blank cell becomes "Unnamed: <position>", then
// repeats get ".1", ".2", ... skipping any suffix another header already
// uses ("a,a,a.1" gives a, a.2, a.1). Named columns are numbered before the
// unnamed ones so real names keep their spelling.
func headerNames(fields []string) []string {
	out := make([]string, len(fields))
	var named, unnamed []int
	for i, name := range fields {
		if name == "" {
			out[i] = fmt.Sprintf("Unnamed: %d", i)
			unnamed = append(unnamed, i)
			continue
		}
		out[i] = name
		named = append(named, i)
	}
	// present tracks the names currently in out, which pandas' `col in
	// this_header` consults as the header is rewritten in place.
	present := make(map[string]int, len(out))
	for _, n := range out {
		present[n]++
	}
	counts := make(map[string]int, len(fields))
	for _, i := range append(named, unnamed...) {
		col := out[i]
		base := col
		cur := counts[col]
		for cur > 0 {
			counts[base] = cur + 1
			col = fmt.Sprintf("%s.%d", base, cur)
			if present[col] > 0 {
				cur++
			} else {
				cur = counts[col]
			}
		}
		present[out[i]]--
		present[col]++
		out[i] = col
		counts[col] = cur + 1
	}
	return out
}

// fromRecords builds a Frame from parsed records whose first record is the
// header. A data row longer than the header widens the table with
// "Unnamed: N" columns instead of failing (pandas raised "Expected N fields")
// or silently moving the first values into an index (what pandas did when
// every row had one extra field, e.g. a trailing comma).
func fromRecords(records [][]string) *Frame {
	if len(records) == 0 {
		return &Frame{}
	}
	width := 0
	for _, r := range records {
		width = max(width, len(r))
	}
	header := make([]string, width)
	copy(header, records[0])
	return &Frame{columns: headerNames(header), rows: records[1:]}
}

// IsArtifactName reports whether a column name is a spreadsheet artifact:
// blank, or pandas' "Unnamed: N" for a blank header cell.
func IsArtifactName(name string) bool {
	n := Strip(name)
	return n == "" || strings.HasPrefix(strings.ToLower(n), "unnamed:")
}

// DropEmptyColumns drops spreadsheet artifacts: unnamed/blank columns with
// no values. Excel exports routinely carry trailing ",,," columns (pandas
// names them "Unnamed: N"). Left in place they defeat the headerless-input
// heuristic, which only fires for single-column frames. It returns the
// frame (f itself when nothing was dropped) and how many columns went.
func DropEmptyColumns(f *Frame) (*Frame, int) {
	keep := make([]int, 0, len(f.columns))
	for c, name := range f.columns {
		if IsArtifactName(name) && f.columnBlank(c) {
			continue
		}
		keep = append(keep, c)
	}
	if len(keep) == len(f.columns) {
		return f, 0
	}
	return f.Select(keep), len(f.columns) - len(keep)
}

// columnBlank: every non-missing cell is whitespace.
func (f *Frame) columnBlank(c int) bool {
	for r := range f.rows {
		if v := f.Cell(r, c); !IsNA(v) && Strip(v) != "" {
			return false
		}
	}
	return true
}
