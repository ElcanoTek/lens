// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package tabular

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// tokenize splits CSV text into records the way pandas' C tokenizer does
// with its defaults: '"' quotes a field only when it opens it ("" inside a
// quoted field is a literal quote, text after a closing quote is appended),
// quotes elsewhere are literal, "\n", "\r\n" and a lone "\r" all end a
// record, and lines that are empty or hold only spaces/tabs are skipped
// (skip_blank_lines). A quoted field still open at the end of the input is
// closed there; pandas raised "EOF inside string" instead.
func tokenize(text string, delim byte) [][]string {
	var (
		records [][]string
		record  []string
		field   strings.Builder
	)
	const (
		startRecord = iota
		startField
		inField
		inQuoted
		quoteInQuoted
	)
	state := startRecord
	endField := func() {
		record = append(record, field.String())
		field.Reset()
	}
	endRecord := func() {
		endField()
		records = append(records, record)
		record = nil
		state = startRecord
	}
	n := len(text)
	for i := 0; i < n; i++ {
		c := text[i]
		isNL := c == '\n' || c == '\r'
		switch state {
		case startRecord:
			// Blank or whitespace-only line: skip it entirely.
			j := i
			for j < n && (text[j] == ' ' || text[j] == '\t') && text[j] != delim {
				j++
			}
			if j == n {
				i = n
				continue
			}
			if text[j] == '\n' || text[j] == '\r' {
				i = skipLF(text, j)
				continue
			}
			state = startField
			i-- // reprocess c as the start of the first field
		case startField:
			switch {
			case c == '"':
				state = inQuoted
			case c == delim:
				endField()
			case isNL:
				i = skipLF(text, i)
				endRecord()
			default:
				field.WriteByte(c)
				state = inField
			}
		case inField:
			switch {
			case c == delim:
				endField()
				state = startField
			case isNL:
				i = skipLF(text, i)
				endRecord()
			default:
				field.WriteByte(c)
			}
		case inQuoted:
			if c == '"' {
				state = quoteInQuoted
			} else {
				field.WriteByte(c)
			}
		case quoteInQuoted:
			switch {
			case c == '"':
				field.WriteByte('"')
				state = inQuoted
			case c == delim:
				endField()
				state = startField
			case isNL:
				i = skipLF(text, i)
				endRecord()
			default:
				field.WriteByte(c)
				state = inField
			}
		}
	}
	if state != startRecord {
		endRecord()
	}
	return records
}

// decodeText strips a UTF-8 BOM and, when the bytes are not valid UTF-8,
// replaces each invalid byte with U+FFFD. It returns a description of the
// first invalid byte ("" when the input was valid) for the caller's warning.
func decodeText(raw []byte) (text, problem string) {
	if utf8.Valid(raw) {
		return strings.TrimPrefix(string(raw), "\ufeff"), ""
	}
	var b strings.Builder
	b.Grow(len(raw) + 16)
	for i := 0; i < len(raw); {
		r, size := utf8.DecodeRune(raw[i:])
		if r == utf8.RuneError && size <= 1 {
			if problem == "" {
				problem = fmt.Sprintf("'utf-8' codec can't decode byte 0x%02x in position %d: invalid start byte", raw[i], i)
			}
			b.WriteRune(utf8.RuneError)
			i++
			continue
		}
		b.Write(raw[i : i+size])
		i += size
	}
	return strings.TrimPrefix(b.String(), "\ufeff"), problem
}

// sniffCandidates returns the delimiters worth re-reading a one-column CSV
// with, given its header cell: European Excel exports use ';' and some tools
// '\t', and either way the whole header line lands in one cell. The more
// frequent one is tried first; on a tie, tab (Python's csv.Sniffer prefers
// tab over semicolon).
func sniffCandidates(header string) []byte {
	semi, tab := strings.Count(header, ";"), strings.Count(header, "\t")
	switch {
	case semi == 0 && tab == 0:
		return nil
	case semi == 0:
		return []byte{'\t'}
	case tab == 0:
		return []byte{';'}
	case semi > tab:
		return []byte{';', '\t'}
	default:
		return []byte{'\t', ';'}
	}
}

// parseCSV parses decoded CSV text, re-reading with a sniffed delimiter
// when a comma parse yields a single header cell holding ';' or a tab. It
// reports the delimiter used.
func parseCSV(text string) (*Frame, byte) {
	records := tokenize(text, ',')
	if len(records) > 0 && len(records[0]) == 1 {
		for _, d := range sniffCandidates(records[0][0]) {
			alt := tokenize(text, d)
			if len(alt) > 0 && len(alt[0]) > 1 {
				return fromRecords(alt), d
			}
		}
	}
	return fromRecords(records), ','
}

// skipLF returns the index of the last byte of the line break at i: i+1
// for "\r\n", else i.
func skipLF(text string, i int) int {
	if text[i] == '\r' && i+1 < len(text) && text[i+1] == '\n' {
		return i + 1
	}
	return i
}
