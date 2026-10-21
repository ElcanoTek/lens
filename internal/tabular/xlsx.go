// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package tabular

import (
	"archive/zip"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Limits that keep a hostile workbook (a zip bomb, or one cell at XFD1048576
// whose gap-filling would allocate billions of cells) from taking the
// process down. They are far above anything a real upload needs: Excel
// itself stops at 1,048,576 rows and 16,384 columns. Variables only so
// tests can lower them.
var (
	maxXLSXUncompressed int64 = 200 << 20 // total bytes inflated across all parts
	maxXLSXRows               = 1 << 20
	maxXLSXColumns            = 1 << 14
	maxXLSXCells              = 50_000_000 // cells materialised, gap fill included
)

// ErrTooLarge reports a workbook over one of the safety limits.
var ErrTooLarge = errors.New("spreadsheet exceeds the size limits")

// budget caps the bytes inflated across every zip entry read.
type budget struct{ left int64 }

type budgetReader struct {
	r io.Reader
	b *budget
}

func (br *budgetReader) Read(p []byte) (int, error) {
	if br.b.left <= 0 {
		// Only an error if there is actually more data.
		var one [1]byte
		if n, _ := br.r.Read(one[:]); n > 0 {
			return 0, fmt.Errorf("%w: more than %d MB uncompressed", ErrTooLarge, maxXLSXUncompressed>>20)
		}
		return 0, io.EOF
	}
	if int64(len(p)) > br.b.left {
		p = p[:br.b.left]
	}
	n, err := br.r.Read(p)
	br.b.left -= int64(n)
	return n, err
}

type xlsxReader struct {
	files map[string]*zip.File
	b     *budget
}

// partKey normalises a part name for lookup: OPC part names are
// case-insensitive, and some generators write backslashes.
func partKey(name string) string {
	return strings.ToLower(strings.TrimPrefix(strings.ReplaceAll(name, `\`, "/"), "/"))
}

func (x *xlsxReader) has(name string) bool {
	_, ok := x.files[partKey(name)]
	return ok
}

func (x *xlsxReader) open(name string) (io.ReadCloser, error) {
	f, ok := x.files[partKey(name)]
	if !ok {
		return nil, fmt.Errorf("xlsx: missing part %s", name)
	}
	if f.UncompressedSize64 > uint64(x.b.left) { //nolint:gosec // left is never negative here
		return nil, fmt.Errorf("%w: part %s is %d bytes uncompressed", ErrTooLarge, name, f.UncompressedSize64)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("xlsx: %s: %w", name, err)
	}
	return struct {
		io.Reader
		io.Closer
	}{&budgetReader{r: rc, b: x.b}, rc}, nil
}

// decode streams the XML part name through fn, one raw token at a time.
// Namespace prefixes are not resolved; callers match local names, which
// also makes Strict OOXML (different namespace URIs) work.
func (x *xlsxReader) decode(name string, fn func(tok xml.Token) error) error {
	rc, err := x.open(name)
	if err != nil {
		return err
	}
	defer rc.Close()
	d := xml.NewDecoder(rc)
	for {
		tok, err := d.RawToken()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("xlsx: %s: %w", name, err)
		}
		if err := fn(tok); err != nil {
			return err
		}
	}
}

func attr(se xml.StartElement, local string) (string, bool) {
	for _, a := range se.Attr {
		if a.Name.Local == local {
			return a.Value, true
		}
	}
	return "", false
}

// relID is the r:id attribute (any prefix; never the plain "id").
func relID(se xml.StartElement) string {
	for _, a := range se.Attr {
		if a.Name.Local == "id" && a.Name.Space != "" {
			return a.Value
		}
	}
	return ""
}

type rel struct{ typ, target string }

// rels reads a .rels part into id -> relationship, with targets resolved
// against base (the directory of the part the relationships belong to).
func (x *xlsxReader) rels(name, base string) map[string]rel {
	out := map[string]rel{}
	if !x.has(name) {
		return out
	}
	_ = x.decode(name, func(tok xml.Token) error {
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "Relationship" {
			return nil
		}
		id, _ := attr(se, "Id")
		typ, _ := attr(se, "Type")
		target, _ := attr(se, "Target")
		if mode, _ := attr(se, "TargetMode"); strings.EqualFold(mode, "External") {
			return nil
		}
		if strings.HasPrefix(target, "/") {
			target = strings.TrimPrefix(path.Clean(target), "/")
		} else {
			target = path.Clean(path.Join(base, target))
		}
		out[id] = rel{typ: typ, target: target}
		return nil
	})
	return out
}

func relsPath(part string) string {
	dir, file := path.Split(part)
	return dir + "_rels/" + file + ".rels"
}

// workbookInfo is what the reader needs from the workbook part.
type workbookInfo struct {
	sheet         string // first worksheet's part name
	sharedStrings string
	styles        string
	date1904      bool
}

func (x *xlsxReader) workbook() (workbookInfo, error) {
	wbPath := "xl/workbook.xml"
	for _, r := range x.rels("_rels/.rels", "") {
		if strings.HasSuffix(r.typ, "/officeDocument") {
			wbPath = r.target
			break
		}
	}
	info := workbookInfo{}
	firstID := ""
	err := x.decode(wbPath, func(tok xml.Token) error {
		se, ok := tok.(xml.StartElement)
		if !ok {
			return nil
		}
		switch se.Name.Local {
		case "workbookPr":
			if v, ok := attr(se, "date1904"); ok {
				info.date1904 = v == "1" || strings.EqualFold(v, "true")
			}
		case "sheet":
			if firstID == "" {
				firstID = relID(se)
			}
		}
		return nil
	})
	if err != nil {
		return info, err
	}
	base := path.Dir(wbPath)
	rels := x.rels(relsPath(wbPath), base)
	if r, ok := rels[firstID]; ok {
		info.sheet = r.target
	}
	for _, r := range rels {
		switch {
		case strings.HasSuffix(r.typ, "/sharedStrings"):
			info.sharedStrings = r.target
		case strings.HasSuffix(r.typ, "/styles"):
			info.styles = r.target
		}
	}
	if info.sheet == "" {
		// No usable relationship: fall back to the conventional name.
		info.sheet = path.Join(base, "worksheets/sheet1.xml")
		if !x.has(info.sheet) {
			return info, errors.New("xlsx: workbook has no worksheet")
		}
	}
	if info.sharedStrings == "" {
		info.sharedStrings = path.Join(base, "sharedStrings.xml")
	}
	if info.styles == "" {
		info.styles = path.Join(base, "styles.xml")
	}
	return info, nil
}

// escapedChar is OOXML's _xHHHH_ escape for characters XML cannot carry.
var escapedChar = regexp.MustCompile(`_x([0-9A-Fa-f]{4})_`)

// unescapeOOXML decodes _xHHHH_ escapes, as openpyxl does when it reads
// strings.
func unescapeOOXML(s string) string {
	if !strings.Contains(s, "_x") {
		return s
	}
	return escapedChar.ReplaceAllStringFunc(s, func(m string) string {
		var r rune
		for _, h := range []byte(m[2:6]) {
			var d byte
			switch {
			case h >= '0' && h <= '9':
				d = h - '0'
			case h >= 'a' && h <= 'f':
				d = h - 'a' + 10
			default: // 'A'..'F'; the regexp admits nothing else
				d = h - 'A' + 10
			}
			r = r<<4 | rune(d)
		}
		return string(r)
	})
}

// stringItem collects the visible text of an <si> or <is> element: plain
// <t> or rich-text runs (<r><t>), but not phonetic guides (<rPh>).
type stringItem struct {
	b       strings.Builder
	inT     bool
	phonDep int
}

func (s *stringItem) token(tok xml.Token) {
	switch t := tok.(type) {
	case xml.StartElement:
		switch t.Name.Local {
		case "rPh", "phoneticPr":
			s.phonDep++
		case "t":
			s.inT = s.phonDep == 0
		}
	case xml.EndElement:
		switch t.Name.Local {
		case "rPh", "phoneticPr":
			s.phonDep--
		case "t":
			s.inT = false
		}
	case xml.CharData:
		if s.inT {
			s.b.Write(t)
		}
	}
}

func (s *stringItem) text() string { return unescapeOOXML(s.b.String()) }

func (x *xlsxReader) sharedStrings(name string) ([]string, error) {
	if !x.has(name) {
		return nil, nil
	}
	var (
		out []string
		cur *stringItem
	)
	err := x.decode(name, func(tok xml.Token) error {
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "si" {
			cur = &stringItem{}
			return nil
		}
		if ee, ok := tok.(xml.EndElement); ok && ee.Name.Local == "si" && cur != nil {
			out = append(out, cur.text())
			cur = nil
			return nil
		}
		if cur != nil {
			cur.token(tok)
		}
		return nil
	})
	return out, err
}

// builtinDateFormats are the built-in number formats that display dates or
// times (ECMA-376 18.8.30, plus the CJK date ids Excel uses).
var builtinDateFormats = map[int]bool{
	14: true, 15: true, 16: true, 17: true, 18: true, 19: true, 20: true, 21: true, 22: true,
	27: true, 28: true, 29: true, 30: true, 31: true, 32: true, 33: true, 34: true, 35: true, 36: true,
	45: true, 46: true, 47: true, 50: true, 51: true, 52: true, 53: true, 54: true, 55: true,
	56: true, 57: true, 58: true,
}

// Format-code noise openpyxl ignores when it looks for date tokens: quoted
// literals, escaped/padding/fill characters, and bracketed sections other
// than the elapsed-time [h]/[m]/[s] forms (colours, conditions, locales).
var (
	formatLiterals = regexp.MustCompile(`"[^"]*"|\\.|_.|\*.`)
	formatBrackets = regexp.MustCompile(`\[[^\]]*\]`)
	elapsedBracket = regexp.MustCompile(`^\[(?:h+|m+|s+)\]$`)
)

func isDateFormat(code string) bool {
	code, _, _ = strings.Cut(code, ";")
	code = formatLiterals.ReplaceAllString(code, "")
	code = formatBrackets.ReplaceAllStringFunc(code, func(m string) string {
		if elapsedBracket.MatchString(strings.ToLower(m)) {
			return m
		}
		return ""
	})
	return strings.ContainsAny(code, "dmyhsDMYHS")
}

// styles returns, per cell style index (the s attribute), whether the
// style's number format shows a date.
func (x *xlsxReader) styles(name string) []bool {
	if !x.has(name) {
		return nil
	}
	custom := map[int]bool{}
	var xfs []bool
	inCellXfs := false
	_ = x.decode(name, func(tok xml.Token) error {
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "numFmt":
				id, _ := attr(t, "numFmtId")
				code, _ := attr(t, "formatCode")
				if n, err := strconv.Atoi(id); err == nil {
					custom[n] = isDateFormat(code)
				}
			case "cellXfs":
				inCellXfs = true
			case "xf":
				if inCellXfs {
					id, _ := attr(t, "numFmtId")
					n, _ := strconv.Atoi(id)
					date, ok := custom[n]
					if !ok {
						date = builtinDateFormats[n]
					}
					xfs = append(xfs, date)
				}
			}
		case xml.EndElement:
			if t.Name.Local == "cellXfs" {
				inCellXfs = false
			}
		}
		return nil
	})
	return xfs
}

// columnIndex parses the letters of a cell reference ("AB12" -> 27).
func columnIndex(ref string) (int, bool) {
	col, n := 0, 0
	for ; n < len(ref); n++ {
		c := ref[n] | 0x20 // ASCII lower-case
		if c < 'a' || c > 'z' {
			break
		}
		col = col*26 + int(c-'a'+1)
		if col > maxXLSXColumns {
			return 0, false
		}
	}
	if n == 0 {
		return 0, false
	}
	return col - 1, true
}

// FormatNumber renders a spreadsheet number the way the cell reads as
// text: whole numbers without a decimal part (Excel stores 512001 as a
// float, and "512001.0" would send an App Store id down the website path),
// anything else in Python's repr form ("1.5", "1e-05", "1.5e+16").
// Integer literals in the file never get here; they are kept verbatim, as
// openpyxl read them as ints.
func FormatNumber(f float64) string {
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
	if f == math.Trunc(f) && math.Abs(f) < 1e16 {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	exp := 0
	if f != 0 {
		exp = int(math.Floor(math.Log10(math.Abs(f))))
	}
	if exp >= -4 && exp < 16 {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return strconv.FormatFloat(f, 'e', -1, 64)
}

// excelTime converts a date serial to a time, with Excel's 1900 leap-year
// bug (serial 60 is the fictitious 1900-02-29) handled like openpyxl.
func excelTime(serial float64, date1904 bool) time.Time {
	epoch := time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC)
	if date1904 {
		epoch = time.Date(1904, 1, 1, 0, 0, 0, 0, time.UTC)
	} else if serial < 60 {
		epoch = epoch.AddDate(0, 0, 1)
	}
	// openpyxl rounds the fraction of a day to the millisecond.
	days := math.Floor(serial)
	ms := math.Round((serial - days) * 86400e3)
	return epoch.AddDate(0, 0, int(days)).Add(time.Duration(ms) * time.Millisecond)
}

// formatDate renders a date cell as str() of the datetime openpyxl/pandas
// produced: "2024-01-02 03:04:05", a bare time for serials below one day
// (in 1900 and 1904 workbooks alike).
func formatDate(serial float64, date1904 bool) string {
	if serial < 0 || serial > 2958465 { // past 9999-12-31: not a real date
		return FormatNumber(serial)
	}
	t := excelTime(serial, date1904)
	layout := "2006-01-02 15:04:05"
	// openpyxl's from_excel: any serial in [0, 1) is a bare time, whatever
	// the workbook's epoch, unless its fraction rounds up to a whole day.
	if serial < 1 && math.Round(serial*86400e3) < 86400e3 {
		layout = "15:04:05"
	}
	s := t.Format(layout)
	if us := t.Nanosecond() / 1000; us != 0 {
		s += fmt.Sprintf(".%06d", us)
	}
	return s
}

var integerLiteral = regexp.MustCompile(`^[+-]?[0-9]+$`)

// sheetCell is the state of the <c> element being read.
type sheetCell struct {
	col   int
	typ   string
	style int
	v     strings.Builder
	inV   bool
	is    *stringItem
}

func (x *xlsxReader) sheet(info workbookInfo, shared []string, dateStyles []bool) ([][]string, error) {
	var (
		records [][]string
		row     []string
		rowSeen bool
		cell    *sheetCell
		nextCol int
		cells   int
	)
	value := func(c *sheetCell) string {
		raw := c.v.String()
		switch c.typ {
		case "s":
			i, err := strconv.Atoi(strings.TrimSpace(raw))
			if err != nil || i < 0 || i >= len(shared) {
				return ""
			}
			return shared[i]
		case "inlineStr":
			if c.is != nil {
				return c.is.text()
			}
			return unescapeOOXML(raw)
		case "str", "e":
			return unescapeOOXML(raw)
		case "b":
			switch strings.TrimSpace(raw) {
			case "1", "true":
				return "True"
			case "0", "false":
				return "False"
			}
			return raw
		case "d":
			if t, err := time.Parse("2006-01-02T15:04:05", strings.TrimSpace(raw)); err == nil {
				return t.Format("2006-01-02 15:04:05")
			}
			return raw
		}
		if raw == "" {
			return ""
		}
		raw = strings.TrimSpace(raw)
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return raw
		}
		if c.style >= 0 && c.style < len(dateStyles) && dateStyles[c.style] {
			return formatDate(f, info.date1904)
		}
		if integerLiteral.MatchString(raw) {
			return strings.TrimPrefix(raw, "+")
		}
		return FormatNumber(f)
	}
	endRow := func() error {
		// Trailing empty cells carry no data (pandas trims them too).
		for len(row) > 0 && row[len(row)-1] == "" {
			row = row[:len(row)-1]
		}
		if len(row) > 0 {
			if len(records) >= maxXLSXRows {
				return fmt.Errorf("%w: more than %d rows", ErrTooLarge, maxXLSXRows)
			}
			records = append(records, row)
		}
		row, rowSeen, nextCol = nil, false, 0
		return nil
	}
	err := x.decode(info.sheet, func(tok xml.Token) error {
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "row":
				rowSeen = true
			case "c":
				cell = &sheetCell{col: nextCol, style: -1}
				if ref, ok := attr(t, "r"); ok {
					if col, ok := columnIndex(ref); ok {
						cell.col = col
					} else {
						return fmt.Errorf("xlsx: bad cell reference %q", ref)
					}
				}
				cell.typ, _ = attr(t, "t")
				if s, ok := attr(t, "s"); ok {
					cell.style, _ = strconv.Atoi(s)
				}
			case "v":
				if cell != nil {
					cell.inV = true
				}
			case "is":
				if cell != nil {
					cell.is = &stringItem{}
				}
			default:
				if cell != nil && cell.is != nil {
					cell.is.token(t)
				}
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "v":
				if cell != nil {
					cell.inV = false
				}
			case "c":
				if cell == nil {
					return nil
				}
				if cell.col >= maxXLSXColumns {
					return fmt.Errorf("%w: more than %d columns", ErrTooLarge, maxXLSXColumns)
				}
				if v := value(cell); v != "" {
					if grow := cell.col + 1 - len(row); grow > 0 {
						cells += grow
						if cells > maxXLSXCells {
							return fmt.Errorf("%w: more than %d cells", ErrTooLarge, maxXLSXCells)
						}
						row = append(row, make([]string, grow)...)
					}
					row[cell.col] = v
				}
				nextCol = cell.col + 1
				cell = nil
			case "row":
				return endRow()
			default:
				if cell != nil && cell.is != nil {
					cell.is.token(t)
				}
			}
		case xml.CharData:
			if cell != nil {
				switch {
				case cell.inV:
					cell.v.Write(t)
				case cell.is != nil:
					cell.is.token(t)
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if rowSeen {
		if err := endRow(); err != nil {
			return nil, err
		}
	}
	return records, nil
}

// readXLSX parses the first worksheet of a workbook. Rows with no values
// are skipped (so a blank first row does not become an all-"Unnamed"
// header) and the first remaining row is the header.
func readXLSX(zr *zip.Reader) (*Frame, error) {
	x := &xlsxReader{files: make(map[string]*zip.File, len(zr.File)), b: &budget{left: maxXLSXUncompressed}}
	for _, f := range zr.File {
		x.files[partKey(f.Name)] = f
	}
	info, err := x.workbook()
	if err != nil {
		return nil, err
	}
	shared, err := x.sharedStrings(info.sharedStrings)
	if err != nil {
		return nil, err
	}
	dateStyles := x.styles(info.styles)
	records, err := x.sheet(info, shared, dateStyles)
	if err != nil {
		return nil, err
	}
	return fromRecords(records), nil
}
