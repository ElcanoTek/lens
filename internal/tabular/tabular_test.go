// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package tabular

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// grid returns every row padded to the frame width.
func grid(f *Frame) [][]string {
	out := make([][]string, f.NumRows())
	for r := range out {
		out[r] = f.Row(r)
	}
	return out
}

func mustParseCSV(t *testing.T, s string) *Frame {
	t.Helper()
	f, err := ParseCSV([]byte(s), "test.csv", nil)
	if err != nil {
		t.Fatalf("ParseCSV(%q): %v", s, err)
	}
	return f
}

func TestParseCSVMatchesPandas(t *testing.T) {
	// Expectations recorded from pandas 3.0 read_csv(dtype=str) on the same
	// input, except where noted.
	cases := []struct {
		name string
		in   string
		cols []string
		rows [][]string
	}{
		{"bom crlf", "\ufeffDomain\r\nfoxnews.com\r\nfoxbusiness.com\r\n", []string{"Domain"}, [][]string{{"foxnews.com"}, {"foxbusiness.com"}}},
		{"whitespace-only line skipped", "a\n  \nb\n", []string{"a"}, [][]string{{"b"}}},
		{"blank lines before header", "\n\na,b\n1,2\n", []string{"a", "b"}, [][]string{{"1", "2"}}},
		{"row of empties kept", "a,b\n,\n1,2\n", []string{"a", "b"}, [][]string{{"", ""}, {"1", "2"}}},
		{"blank header cells", ",x, \n1,2,3\n", []string{"Unnamed: 0", "x", " "}, [][]string{{"1", "2", "3"}}},
		{"short row padded", "a,b\n1\n", []string{"a", "b"}, [][]string{{"1", ""}}},
		{"quotes", "a,b\n\"x\"\"y\",z\"q\n\"ab\"cd,e\n", []string{"a", "b"}, [][]string{{`x"y`, `z"q`}, {"abcd", "e"}}},
		{"quote not at field start is literal", "a\n \"\" \n\"\"\nx\n", []string{"a"}, [][]string{{` "" `}, {""}, {"x"}}},
		{"lone CR line endings", "a\rb\rc\r", []string{"a"}, [][]string{{"b"}, {"c"}}},
		{"multiline quoted", "a\n\"multi\nline\"\nz\n", []string{"a"}, [][]string{{"multi\nline"}, {"z"}}},
		{"semicolon sniffed", "Domain;Tier;Notes\nlemonde.fr;Premium;bon\nspiegel.de;Standard;gut\n", []string{"Domain", "Tier", "Notes"}, [][]string{{"lemonde.fr", "Premium", "bon"}, {"spiegel.de", "Standard", "gut"}}},
		{"tab sniffed", "a\tb\n1\t2\n", []string{"a", "b"}, [][]string{{"1", "2"}}},
		{"semicolon in one-column comma file without semicolon split stays", "Notes\na;b\n", []string{"Notes"}, [][]string{{"a;b"}}},
		{"numbers stay text", "a,b\n512001,\n,00123\n", []string{"a", "b"}, [][]string{{"512001", ""}, {"", "00123"}}},
		// pandas raised "Expected 2 fields"; we widen instead.
		{"long row widens", "a,b\n1,2\n3,4,5\n", []string{"a", "b", "Unnamed: 2"}, [][]string{{"1", "2", ""}, {"3", "4", "5"}}},
		// pandas moved "foo.com" into an implicit index; we keep it.
		{"trailing comma rows", "Domain\nfoo.com,\nbar.com,\n", []string{"Domain", "Unnamed: 1"}, [][]string{{"foo.com", ""}, {"bar.com", ""}}},
		// pandas raised "EOF inside string"; we close the field.
		{"unterminated quote", "a\n1\n\"unterminated\n", []string{"a"}, [][]string{{"1"}, {"unterminated\n"}}},
		{"no trailing newline", "a,b\n1,2", []string{"a", "b"}, [][]string{{"1", "2"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := mustParseCSV(t, tc.in)
			if !reflect.DeepEqual(f.Columns(), tc.cols) {
				t.Errorf("columns = %q, want %q", f.Columns(), tc.cols)
			}
			if got := grid(f); !reflect.DeepEqual(got, tc.rows) {
				t.Errorf("rows = %q, want %q", got, tc.rows)
			}
		})
	}
}

func TestHeaderNamesMatchPandas(t *testing.T) {
	// Recorded from pandas 3.0 (read_csv and read_excel agree).
	cases := map[string][]string{
		"a,,a,a.1,a":       {"a", "Unnamed: 1", "a.2", "a.1", "a.3"},
		"x,x,x":            {"x", "x.1", "x.2"},
		",Unnamed: 0,":     {"Unnamed: 0.1", "Unnamed: 0", "Unnamed: 2"},
		"a.1,a,a":          {"a.1", "a", "a.2"},
		"b,b.1,b,b":        {"b", "b.1", "b.2", "b.3"},
		"Unnamed: 1,,":     {"Unnamed: 1", "Unnamed: 1.1", "Unnamed: 2"},
		"a,a,a.1.1,a.1":    {"a", "a.2", "a.1.1", "a.1"},
		"Domain,domain":    {"Domain", "domain"},
		"Domain,Domain, x": {"Domain", "Domain.1", " x"},
	}
	for in, want := range cases {
		if got := headerNames(strings.Split(in, ",")); !reflect.DeepEqual(got, want) {
			t.Errorf("headerNames(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsNA(t *testing.T) {
	for _, s := range []string{"", "NA", "N/A", "n/a", "null", "NULL", "None", "nan", "NaN", "-nan", "-NaN", "#N/A", "#N/A N/A", "#NA", "<NA>", "1.#IND", "1.#QNAN", "-1.#IND", "-1.#QNAN"} {
		if !IsNA(s) {
			t.Errorf("IsNA(%q) = false", s)
		}
	}
	for _, s := range []string{" ", "na", "none", "Null", " NA", "foo", "0", "nat"} {
		if IsNA(s) {
			t.Errorf("IsNA(%q) = true", s)
		}
	}
}

func TestInvalidUTF8IsReplacedWithWarning(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	f, err := ParseCSV([]byte("\xef\xbb\xbfDomain\ncaf\xe9.com\nok.com\n"), "latin1.csv", log)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.Columns(), []string{"Domain"}) {
		t.Errorf("columns = %q", f.Columns())
	}
	if got := f.Cell(0, 0); got != "caf�.com" {
		t.Errorf("cell = %q", got)
	}
	out := buf.String()
	if !strings.Contains(out, "Failed to decode latin1.csv as UTF-8") || !strings.Contains(out, "Retrying with replacement characters") {
		t.Errorf("missing warning: %s", out)
	}
	if !strings.Contains(out, "with replacement characters for invalid bytes") {
		t.Errorf("missing info line: %s", out)
	}
}

func TestEmptyCSVIsAnError(t *testing.T) {
	for _, in := range []string{"", "\n\n", "  \n"} {
		if _, err := ParseCSV([]byte(in), "e.csv", nil); err == nil {
			t.Errorf("ParseCSV(%q) succeeded", in)
		}
	}
}

func TestDropEmptyColumns(t *testing.T) {
	f := mustParseCSV(t, "Domain,,,Notes,\nnytimes.com,,NA,,x\nwsj.com,, ,,\n")
	got, dropped := DropEmptyColumns(f)
	if dropped != 2 {
		t.Errorf("dropped %d", dropped)
	}
	// Unnamed: 1 (blank) and Unnamed: 2 (only NA / whitespace) go; the named
	// but empty Notes column and the Unnamed column with a value stay.
	if want := []string{"Domain", "Notes", "Unnamed: 4"}; !reflect.DeepEqual(got.Columns(), want) {
		t.Errorf("columns = %q, want %q", got.Columns(), want)
	}
	if want := [][]string{{"nytimes.com", "", "x"}, {"wsj.com", "", ""}}; !reflect.DeepEqual(grid(got), want) {
		t.Errorf("rows = %q", grid(got))
	}
	same, n := DropEmptyColumns(got)
	if n != 0 || same != got {
		t.Error("second pass changed the frame")
	}
}

func writeFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadCSV(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	p := writeFile(t, "trailing.CSV", []byte("Domain,,,\nnytimes.com,,,\nwsj.com,,,\n"))
	f, err := Load(p, log)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.Columns(), []string{"Domain"}) || f.NumRows() != 2 {
		t.Errorf("got %q %q", f.Columns(), grid(f))
	}
	if !strings.Contains(buf.String(), "Dropped 3 empty spreadsheet columns") || !strings.Contains(buf.String(), "Loaded 2 rows from") {
		t.Errorf("log: %s", buf.String())
	}
	buf.Reset()
	p = writeFile(t, "euro.csv", []byte("Domain;Tier\nlemonde.fr;Premium\n"))
	if _, err := Load(p, log); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "with sniffed delimiter (2 columns)") {
		t.Errorf("log: %s", buf.String())
	}
}

func TestLoadRejectsOtherSuffixes(t *testing.T) {
	for _, name := range []string{"list.txt", "list.xls", "list"} {
		_, err := Load(writeFile(t, name, []byte("Domain\na.com\n")), nil)
		if !errors.Is(err, ErrUnsupported) || err.Error() != "Input file must be a .csv or .xlsx file" {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.csv"), nil); err == nil {
		t.Error("missing file loaded")
	}
}

// ---- XLSX ----

// workbook assembles a minimal .xlsx in memory. parts maps part names to
// XML; the defaults (content types, root rels, workbook, workbook rels)
// are filled in unless overridden.
func workbook(t *testing.T, parts map[string]string) []byte {
	t.Helper()
	defaults := map[string]string{
		"[Content_Types].xml": `<?xml version="1.0" encoding="UTF-8"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>`,
		"_rels/.rels": `<?xml version="1.0" encoding="UTF-8"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
			`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/></Relationships>`,
		"xl/workbook.xml": `<?xml version="1.0" encoding="UTF-8"?><workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">` +
			`<sheets><sheet name="Sheet1" sheetId="1" r:id="rId1"/></sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<?xml version="1.0" encoding="UTF-8"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
			`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/>` +
			`<Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/sharedStrings" Target="sharedStrings.xml"/>` +
			`<Relationship Id="rId3" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/></Relationships>`,
	}
	for k, v := range parts {
		defaults[k] = v
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range defaults {
		if body == "" {
			continue // "" removes a default part
		}
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sheetXML(rows string) string {
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>` + rows + `</sheetData></worksheet>`
}

func sstXML(items ...string) string {
	return `<?xml version="1.0" encoding="UTF-8"?><sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">` + strings.Join(items, "") + `</sst>`
}

func mustParseXLSX(t *testing.T, data []byte) *Frame {
	t.Helper()
	f, err := ParseXLSX(data)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestXLSXCellTypes(t *testing.T) {
	data := workbook(t, map[string]string{
		"xl/sharedStrings.xml": sstXML(
			`<si><t>Domain</t></si>`,
			`<si><r><rPr><b/></rPr><t>rich</t></r><r><t xml:space="preserve"> text</t></r></si>`,
			`<si><t>漢字</t><rPh sb="0" eb="2"><t>かんじ</t></rPh></si>`,
			`<si><t>line_x000D_break &amp; more</t></si>`,
			`<si><t>Value</t></si>`,
		),
		"xl/styles.xml": `<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">` +
			`<numFmts count="2"><numFmt numFmtId="164" formatCode="yyyy\-mm\-dd"/><numFmt numFmtId="165" formatCode="&quot;day&quot;0.00;[Red]0"/></numFmts>` +
			`<cellXfs count="4"><xf numFmtId="0"/><xf numFmtId="14"/><xf numFmtId="164"/><xf numFmtId="165"/></cellXfs></styleSheet>`,
		"xl/worksheets/sheet1.xml": sheetXML(
			`<row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1" t="s"><v>4</v></c></row>` +
				`<row r="2"><c r="A2" t="s"><v>1</v></c><c r="B2"><v>512001</v></c></row>` +
				`<row r="3"><c r="A3" t="s"><v>2</v></c><c r="B3"><v>512001.0</v></c></row>` +
				`<row r="4"><c r="A4" t="s"><v>3</v></c><c r="B4"><v>0.25</v></c></row>` +
				`<row r="5"><c r="A5" t="inlineStr"><is><t>inline</t></is></c><c r="B5"><v>1E-5</v></c></row>` +
				`<row r="6"><c r="A6" t="inlineStr"><is><r><t>in</t></r><r><t>run</t></r></is></c><c r="B6"><v>1.5E+16</v></c></row>` +
				`<row r="7"><c r="A7" t="b"><v>1</v></c><c r="B7" t="b"><v>0</v></c></row>` +
				`<row r="8"><c r="A8" t="e"><v>#N/A</v></c><c r="B8" t="e"><v>#DIV/0!</v></c></row>` +
				`<row r="9"><c r="A9" t="str"><f>A1&amp;"x"</f><v>Domainx</v></c><c r="B9"><f>1+1</f><v>2</v></c></row>` +
				`<row r="10"><c r="A10" s="1"><v>45293.127835648</v></c><c r="B10" s="2"><v>45291</v></c></row>` +
				`<row r="11"><c r="A11" s="3"><v>7</v></c><c r="B11" s="1"><v>0.5</v></c></row>` +
				`<row r="12"><c r="A12"><v>-3</v></c><c r="B12"><v>100000000000000000000</v></c><c r="C12"><v>1E+20</v></c></row>`),
	})
	f := mustParseXLSX(t, data)
	if want := []string{"Domain", "Value", "Unnamed: 2"}; !reflect.DeepEqual(f.Columns(), want) {
		t.Errorf("columns = %q", f.Columns())
	}
	want := [][]string{
		{"rich text", "512001"},
		{"漢字", "512001"},
		{"line\rbreak & more", "0.25"},
		{"inline", "1e-05"},
		{"inrun", "1.5e+16"},
		{"True", "False"},
		{"#N/A", "#DIV/0!"},
		{"Domainx", "2"},
		{"2024-01-02 03:04:05", "2023-12-31 00:00:00"},
		{"7", "12:00:00"},
		// Integer literals stay verbatim; a float literal gets Python's repr.
		{"-3", "100000000000000000000", "1e+20"},
	}
	for i := range want {
		want[i] = append(want[i], make([]string, 3-len(want[i]))...)
	}
	if got := grid(f); !reflect.DeepEqual(got, want) {
		t.Errorf("rows:\n got %q\nwant %q", got, want)
	}
	if !f.Missing(6, 0) || f.Missing(6, 1) {
		t.Error("#N/A should read as missing, #DIV/0! should not")
	}
}

func TestXLSXGapsBlankRowsAndRefs(t *testing.T) {
	data := workbook(t, map[string]string{
		"xl/sharedStrings.xml": "",
		"xl/worksheets/sheet1.xml": sheetXML(
			`<row r="1"><c r="A1" s="0"/><c r="C1" t="inlineStr"><is><t></t></is></c></row>` + // blank row: skipped
				`<row r="2"><c r="B2" t="inlineStr"><is><t>Domain</t></is></c><c r="D2" t="inlineStr"><is><t>Extra</t></is></c></row>` +
				`<row r="3"><c r="B3" t="inlineStr"><is><t>a.com</t></is></c><c r="F3" s="0"/></row>` +
				`<row r="7"><c t="inlineStr"><is><t>x</t></is></c><c t="inlineStr"><is><t>b.com</t></is></c><c r="E7"><v>5</v></c></row>`),
	})
	f := mustParseXLSX(t, data)
	if want := []string{"Unnamed: 0", "Domain", "Unnamed: 2", "Extra", "Unnamed: 4"}; !reflect.DeepEqual(f.Columns(), want) {
		t.Errorf("columns = %q, want %q", f.Columns(), want)
	}
	if want := [][]string{{"", "a.com", "", "", ""}, {"x", "b.com", "", "", "5"}}; !reflect.DeepEqual(grid(f), want) {
		t.Errorf("rows = %q", grid(f))
	}
}

func TestXLSXFirstSheetByWorkbookOrderAndAbsoluteTargets(t *testing.T) {
	data := workbook(t, map[string]string{
		"_rels/.rels": `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
			`<Relationship Id="rId1" Type="http://purl.oclc.org/ooxml/officeDocument/relationships/officeDocument" Target="/xl/book.xml"/></Relationships>`,
		"xl/workbook.xml":            "",
		"xl/_rels/workbook.xml.rels": "",
		"xl/book.xml": `<x:workbook xmlns:x="http://purl.oclc.org/ooxml/spreadsheetml/main" xmlns:rel="http://purl.oclc.org/ooxml/officeDocument/relationships">` +
			`<x:workbookPr date1904="1"/><x:sheets><x:sheet name="Second" sheetId="2" rel:id="rIdB"/><x:sheet name="First" sheetId="1" rel:id="rIdA"/></x:sheets></x:workbook>`,
		"xl/_rels/book.xml.rels": `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
			`<Relationship Id="rIdA" Type="http://purl.oclc.org/ooxml/officeDocument/relationships/worksheet" Target="/xl/worksheets/sheet1.xml"/>` +
			`<Relationship Id="rIdB" Type="http://purl.oclc.org/ooxml/officeDocument/relationships/worksheet" Target="worksheets/data.xml"/>` +
			`<Relationship Id="rIdS" Type="http://purl.oclc.org/ooxml/officeDocument/relationships/styles" Target="theStyles.xml"/>` +
			`<Relationship Id="rIdX" Type="http://x/hyperlink" Target="https://example.com" TargetMode="External"/></Relationships>`,
		"xl/theStyles.xml":         `<styleSheet><cellXfs><xf numFmtId="0"/><xf numFmtId="15"/></cellXfs></styleSheet>`,
		"xl/worksheets/sheet1.xml": sheetXML(`<row><c t="inlineStr"><is><t>wrong sheet</t></is></c></row>`),
		"xl/worksheets/data.xml":   sheetXML(`<row><c t="inlineStr"><is><t>When</t></is></c></row><row><c s="1"><v>0</v></c></row>`),
	})
	f := mustParseXLSX(t, data)
	if want := []string{"When"}; !reflect.DeepEqual(f.Columns(), want) {
		t.Fatalf("columns = %q", f.Columns())
	}
	if got := f.Cell(0, 0); got != "1904-01-01 00:00:00" {
		t.Errorf("1904 date = %q", got)
	}
}

func TestXLSXRealWorkbooks(t *testing.T) {
	// Written by pandas/openpyxl; expectations from pd.read_excel(dtype=str),
	// except that the all-blank row openpyxl_types.xlsx carries is skipped.
	cases := []struct {
		file string
		cols []string
		rows [][]string
	}{
		{"pandas_written.xlsx", []string{"App Bundle", "Score"}, [][]string{
			{"54092", "1.5"}, {"", "2"}, {"B091RFCS5V", ""}, {"com.plexapp.ctv", "1e-05"},
		}},
		{"openpyxl_types.xlsx", []string{"Domain", "When", "Flag", "N", "Unnamed: 4", "Dup", "Dup.1"}, [][]string{
			{"example.com", "2024-01-02 03:04:05", "True", "512001", "", "x", "y"},
			{"io.example.com", "2023-12-31 00:00:00", "False", "0.25", "", "", ""},
			{"  spaced  ", "", "", "-3", "", "NA", ""},
			{"after-gap.org", "", "", "", "", "", ""},
		}},
	}
	for _, tc := range cases {
		data, err := os.ReadFile(filepath.Join("testdata", tc.file))
		if err != nil {
			t.Fatal(err)
		}
		f := mustParseXLSX(t, data)
		if !reflect.DeepEqual(f.Columns(), tc.cols) {
			t.Errorf("%s: columns = %q", tc.file, f.Columns())
		}
		if got := grid(f); !reflect.DeepEqual(got, tc.rows) {
			t.Errorf("%s: rows =\n%q\nwant\n%q", tc.file, got, tc.rows)
		}
	}
	// Through Load the empty Unnamed: 4 column is dropped.
	f, err := Load(filepath.Join("testdata", "openpyxl_types.xlsx"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Domain", "When", "Flag", "N", "Dup", "Dup.1"}; !reflect.DeepEqual(f.Columns(), want) {
		t.Errorf("Load columns = %q", f.Columns())
	}
}

func TestXLSXLimits(t *testing.T) {
	big := strings.Repeat(`<row><c t="inlineStr"><is><t>example.com</t></is></c></row>`, 2000)
	data := workbook(t, map[string]string{"xl/worksheets/sheet1.xml": sheetXML(big)})

	saved := maxXLSXUncompressed
	maxXLSXUncompressed = 10_000
	_, err := ParseXLSX(data)
	maxXLSXUncompressed = saved
	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("uncompressed cap: err = %v", err)
	}

	savedRows := maxXLSXRows
	maxXLSXRows = 100
	_, err = ParseXLSX(data)
	maxXLSXRows = savedRows
	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("row cap: err = %v", err)
	}

	// One far-right cell per row would need 16384 cells each.
	wide := workbook(t, map[string]string{"xl/worksheets/sheet1.xml": sheetXML(strings.Repeat(`<row><c r="XFD1"><v>1</v></c></row>`, 10))})
	savedCells := maxXLSXCells
	maxXLSXCells = 100_000
	_, err = ParseXLSX(wide)
	maxXLSXCells = savedCells
	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("cell cap: err = %v", err)
	}
	beyond := workbook(t, map[string]string{"xl/worksheets/sheet1.xml": sheetXML(`<row><c r="XFE1"><v>1</v></c></row>`)})
	if _, err := ParseXLSX(beyond); err == nil {
		t.Error("column past XFD accepted")
	}
}

func TestXLSXGarbage(t *testing.T) {
	if _, err := ParseXLSX([]byte("PK\x03\x04fake")); err == nil {
		t.Error("garbage accepted")
	}
	if _, err := Load(writeFile(t, "fake.xlsx", []byte("not a zip")), nil); err == nil {
		t.Error("garbage file accepted")
	}
	noSheet := workbook(t, map[string]string{"xl/_rels/workbook.xml.rels": ""})
	if _, err := ParseXLSX(noSheet); err == nil {
		t.Error("workbook without sheet accepted")
	}
}

func TestFormatNumber(t *testing.T) {
	cases := map[float64]string{
		512001: "512001", 0: "0", -3: "-3", 0.25: "0.25", 1.5: "1.5", 1e-5: "1e-05",
		1234567.5: "1234567.5", 1.5e16: "1.5e+16", 1e20: "1e+20", 1e15: "1000000000000000", 1e16: "1e+16",
		0.0001: "0.0001",
	}
	for in, want := range cases {
		if got := FormatNumber(in); got != want {
			t.Errorf("FormatNumber(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestIsDateFormat(t *testing.T) {
	for code, want := range map[string]bool{
		"yyyy-mm-dd": true, `yyyy\-mm\-dd`: true, "h:mm AM/PM": true, "[h]:mm:ss": true,
		"General": false, "0.00": false, `"day"0.00`: false, "[Red]0.00": false, "#,##0;[Red]-#,##0": false,
		"[$-409]0.00": false, `0.00_);\(0.00\)`: false,
	} {
		if got := isDateFormat(code); got != want {
			t.Errorf("isDateFormat(%q) = %v", code, got)
		}
	}
}

func TestFrameAccessors(t *testing.T) {
	f := New([]string{"a", "b"}, [][]string{{"1"}, {"x", "NA", "dropped"}})
	if f.NumRows() != 2 || f.NumColumns() != 2 || f.Empty() {
		t.Fatal("shape")
	}
	if f.Cell(0, 1) != "" || !f.Missing(0, 1) || !f.Missing(1, 1) {
		t.Error("padding/NA")
	}
	if got := f.Present(1); len(got) != 0 {
		t.Errorf("Present = %q", got)
	}
	if got := f.RowMap(1); !reflect.DeepEqual(got, map[string]string{"a": "x"}) {
		t.Errorf("RowMap = %v", got)
	}
	if f.Index("b") != 1 || f.Has("c") {
		t.Error("Index/Has")
	}
	if !New([]string{"a"}, nil).Empty() || !New(nil, [][]string{{"x"}}).Empty() {
		t.Error("Empty")
	}
	if fmt.Sprint(f.Column(0)) != "[1 x]" {
		t.Errorf("Column = %q", f.Column(0))
	}
}
