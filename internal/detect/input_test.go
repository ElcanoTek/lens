// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package detect

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ElcanoTek/lens/internal/item"
	"github.com/ElcanoTek/lens/internal/logx"
	"github.com/ElcanoTek/lens/internal/tabular"
)

func TestMain(m *testing.M) {
	logx.SetOutput(io.Discard, slog.LevelError+1)
	os.Exit(m.Run())
}

// expected is what testdata/inputs/expected.json records per input file,
// produced by running the Python release's own functions
// (orchestration._load_input_dataframe + _normalize_headerless_input_dataframe
// + InputDetector + the _load_input_data loop, web_service._compute_breakdown,
// parse_ctv_work_items and parse_ctv_input) on the same files.
type expected struct {
	Work struct {
		Exception  string     `json:"exception"`
		Column     string     `json:"column"`
		CTVFile    bool       `json:"ctv_file"`
		Items      [][]string `json:"items"`
		TypeColumn string     `json:"type_column"`
	} `json:"work"`
	Breakdown    map[string]any `json:"breakdown"`
	CTVWorkItems any            `json:"ctv_work_items"`
	CTVInput     [][]string     `json:"ctv_input"`
}

func workPairs(w *WorkList) [][]string {
	out := make([][]string, 0, len(w.Items))
	for _, it := range w.Items {
		out = append(out, []string{it.Identifier, string(it.Type)})
	}
	return out
}

func ctvRows(items []item.CTVWorkItem, full bool) [][]string {
	out := make([][]string, 0, len(items))
	for _, it := range items {
		row := []string{it.AppName, it.BundleID, it.SSP, it.Publisher}
		if full {
			row = append(row, it.Platform, it.URL)
		}
		out = append(out, row)
	}
	return out
}

func toInts(m map[string]any) map[string]int {
	out := map[string]int{}
	for k, v := range m {
		if f, ok := v.(float64); ok {
			out[k] = int(f)
		}
	}
	return out
}

func toRows(v any) [][]string {
	list, _ := v.([]any)
	out := make([][]string, 0, len(list))
	for _, r := range list {
		cells, _ := r.([]any)
		row := make([]string, 0, len(cells))
		for _, c := range cells {
			row = append(row, c.(string))
		}
		out = append(out, row)
	}
	return out
}

// deliberate lists where the Go loader intentionally differs from Python.
var deliberate = map[string]struct {
	column string
	items  [][]string
	note   string
}{
	// Python lost the first row of a headerless list that was not ≥80%
	// websites (the preview counted 3, the run processed 2).
	"headerless_mixed.csv": {"Domain", [][]string{{"284882215", "ios_app"}, {"example.com", "website"}, {"com.example.app", "android_app"}}, "header recovered"},
	// pandas raised "Expected 1 fields in line 5, saw 2" on the "," row.
	"numeric_ids_blank.csv": {"app_id", [][]string{{"333903271", "ios_app"}, {"389801252", "ios_app"}}, "ragged row tolerated"},
}

func TestInputsMatchPython(t *testing.T) {
	raw, err := os.ReadFile("testdata/inputs/expected.json")
	if err != nil {
		t.Fatal(err)
	}
	var all map[string]expected
	if err := json.Unmarshal(raw, &all); err != nil {
		t.Fatal(err)
	}
	if len(all) < 20 {
		t.Fatalf("only %d fixtures", len(all))
	}
	for name, want := range all {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join("testdata", "inputs", name)
			f, err := LoadInput(path)
			if err != nil {
				t.Fatal(err)
			}
			if IsCTVInputFile(f) != want.Work.CTVFile && want.Work.Exception == "" {
				t.Errorf("IsCTVInputFile = %v", !want.Work.CTVFile)
			}
			w, err := BuildWorkList(f)
			if err != nil {
				t.Fatal(err)
			}
			wantCol, wantItems, wantType := want.Work.Column, want.Work.Items, want.Work.TypeColumn
			if d, ok := deliberate[name]; ok {
				wantCol, wantItems = d.column, d.items
			}
			if w.InputColumn != wantCol || w.TypeColumn != wantType {
				t.Errorf("columns = %q/%q, want %q/%q", w.InputColumn, w.TypeColumn, wantCol, wantType)
			}
			if got := workPairs(w); !reflect.DeepEqual(got, wantItems) {
				t.Errorf("items =\n%q\nwant\n%q", got, wantItems)
			}

			bd, err := Breakdown(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, failed := want.Breakdown["exception"]; !failed {
				if wantBD := toInts(want.Breakdown); !reflect.DeepEqual(bd, wantBD) {
					t.Errorf("breakdown = %v, want %v", bd, wantBD)
				}
			}

			if _, ok := deliberate[name]; ok {
				return
			}
			if got, wantRows := ctvRows(ParseCTVWorkItems(f), true), toRows(want.CTVWorkItems); !reflect.DeepEqual(got, wantRows) {
				t.Errorf("ParseCTVWorkItems =\n%q\nwant\n%q", got, wantRows)
			}
			if got := ctvRows(ParseCTVInput(f), false); !reflect.DeepEqual(got, want.CTVInput) && (len(got) > 0 || len(want.CTVInput) > 0) {
				t.Errorf("ParseCTVInput =\n%q\nwant\n%q", got, want.CTVInput)
			}
		})
	}
}

// ---- tests/test_input_robustness.py ----

func analyzeFile(t *testing.T, path string) *Analysis {
	t.Helper()
	f, err := LoadInput(path)
	if err != nil {
		t.Fatal(err)
	}
	a, err := Analyze(f)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func writeFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func values(a *Analysis) []string {
	out := make([]string, 0, len(a.Items))
	for _, tv := range a.Items {
		out = append(out, tv.Value)
	}
	return out
}

func TestBOMCRLFCSVDetectsDomainColumn(t *testing.T) {
	a := analyzeFile(t, writeFile(t, "rmi.csv", []byte("\xef\xbb\xbfDomain\r\nfoxnews.com\r\nfoxbusiness.com\r\n")))
	if a.InputColumn != "Domain" || len(a.Items) != 2 {
		t.Errorf("%+v", a)
	}
}

func TestHeaderlessXLSXRecoversHeaderDomain(t *testing.T) {
	path := writeXLSX(t, "allowlist.xlsx", [][]any{{"harborlight-news.com"}, {"cedarpost.com"}, {"01net-example.com"}})
	a := analyzeFile(t, path)
	if a.InputColumn != "Domain" {
		t.Errorf("column = %q", a.InputColumn)
	}
	if v := values(a); len(v) != 3 || v[0] != "harborlight-news.com" {
		t.Errorf("values = %q", v)
	}
}

func TestSemicolonDelimitedCSVIsSniffed(t *testing.T) {
	a := analyzeFile(t, writeFile(t, "euro.csv", []byte("Domain;Tier;Notes\nlemonde.fr;Premium;bon\nspiegel.de;Standard;gut\n")))
	if a.InputColumn != "Domain" || !reflect.DeepEqual(values(a), []string{"lemonde.fr", "spiegel.de"}) {
		t.Errorf("%+v", a)
	}
}

func TestTrailingEmptyColumnsDropped(t *testing.T) {
	a := analyzeFile(t, writeFile(t, "trailing.csv", []byte("Domain,,,\nnytimes.com,,,\nwsj.com,,,\n")))
	if a.InputColumn != "Domain" || len(a.Items) != 2 {
		t.Errorf("%+v", a)
	}
}

func TestContentFallbackFindsDomainColumn(t *testing.T) {
	a := analyzeFile(t, writeFile(t, "ranked.csv", []byte("Position,Address\n1,nytimes.com\n2,wsj.com\n3,cnn.com\n")))
	if a.InputColumn != "Address" || len(a.Items) != 3 {
		t.Errorf("%+v", a)
	}
}

// ---- breakdown (tests/test_input_robustness.py, web_service) ----

func breakdownOf(t *testing.T, name string, data []byte) map[string]int {
	t.Helper()
	bd, err := Breakdown(writeFile(t, name, data))
	if err != nil {
		t.Fatal(err)
	}
	return bd
}

func TestBreakdownCountsMixedFile(t *testing.T) {
	got := breakdownOf(t, "mixed.csv", []byte("identifier\nexample.com\nanother-site.org\nhttps://third.io\n284882215\ncom.example.app\n"))
	if want := map[string]int{"websites": 3, "ios": 1, "android": 1, "total": 5}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v", got)
	}
}

func TestBreakdownAppBundleColumnIsCTVAndIgnoresBlankCells(t *testing.T) {
	path := writeXLSX(t, "App Bundle list.xlsx", [][]any{{"App Bundle"}, {54092}, {nil}, {"B091RFCS5V"}, {"com.plexapp.ctv"}})
	got, err := Breakdown(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]int{"ctv": 3, "total": 3}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v", got)
	}
}

func TestBreakdownNumericSheetWithBlankCellStillCounts(t *testing.T) {
	path := writeXLSX(t, "ids.xlsx", [][]any{{"Domain"}, {333903271}, {nil}, {"example.com"}})
	got, err := Breakdown(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]int{"websites": 1, "ios": 1, "android": 0, "total": 2}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v", got)
	}
}

func TestBreakdownHeaderlessLists(t *testing.T) {
	cases := []struct {
		in   string
		want map[string]int
	}{
		// Every row counts: the first line isn't eaten as a header.
		{"example.com\nfoo.com\nbar.com\n", map[string]int{"websites": 3, "ios": 0, "android": 0, "total": 3}},
		// Detection isn't website-only.
		{"284882215\nexample.com\ncom.example.app\n", map[string]int{"websites": 1, "ios": 1, "android": 1, "total": 3}},
		// A recognised header stays a header.
		{"Domain\nexample.com\n", map[string]int{"websites": 1, "ios": 0, "android": 0, "total": 1}},
		// Beyond Python (which returned {} for a frame with no data rows):
		// a one-line headerless file is one item.
		{"example.com\n", map[string]int{"websites": 1, "ios": 0, "android": 0, "total": 1}},
		// Nothing processable.
		{"notes\nhello\nworld\n", map[string]int{}},
		{"Domain\n", map[string]int{}},
	}
	for _, tc := range cases {
		if got := breakdownOf(t, "list.csv", []byte(tc.in)); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Breakdown(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestBreakdownErrors(t *testing.T) {
	if _, err := Breakdown(writeFile(t, "list.txt", []byte("Domain\na.com\n"))); !errors.Is(err, tabular.ErrUnsupported) {
		t.Errorf("err = %v", err)
	}
	if _, err := Breakdown(writeFile(t, "list.xlsx", []byte("PK\x03\x04fake"))); err == nil {
		t.Error("fake workbook accepted")
	}
}

// ---- orchestrator loading (tests/test_web_service.py, test_auto_mode.py) ----

func workList(t *testing.T, path string) *WorkList {
	t.Helper()
	f, err := LoadInput(path)
	if err != nil {
		t.Fatal(err)
	}
	w, err := BuildWorkList(f)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func identifiers(w *WorkList) []string {
	out := make([]string, 0, len(w.Items))
	for _, it := range w.Items {
		out = append(out, it.Identifier)
	}
	return out
}

func TestOrchestratorLoadsXLSXInput(t *testing.T) {
	path := writeXLSX(t, "input workbook.xlsx", [][]any{{"pageURL"}, {"example.com"}, {"example.com"}, {"github.com"}})
	if got := identifiers(workList(t, path)); !reflect.DeepEqual(got, []string{"example.com", "github.com"}) {
		t.Errorf("got %q", got)
	}
}

func TestOrchestratorLoadsHeaderlessDomainList(t *testing.T) {
	path := writeFile(t, "domains only.csv", []byte("example.com\ngithub.com\nexample.com\n"))
	if got := identifiers(workList(t, path)); !reflect.DeepEqual(got, []string{"example.com", "github.com"}) {
		t.Errorf("got %q", got)
	}
}

func TestAutoModeCTVDetectionAndMixedContent(t *testing.T) {
	ctv := "SSP,Publisher,\"App, Account or Network Name\",Bundle ID\nExampleSSP,Northwind Broadcasting,Example News Network,512001\n"
	f, err := LoadInput(writeFile(t, "ctv.csv", []byte(ctv)))
	if err != nil {
		t.Fatal(err)
	}
	if !IsCTVInputFile(f) {
		t.Error("CTV master file not detected")
	}
	items := ParseCTVInput(f)
	wantItems := []item.CTVWorkItem{{AppName: "Example News Network", BundleID: "512001", SSP: "ExampleSSP", Publisher: "Northwind Broadcasting"}}
	if !reflect.DeepEqual(items, wantItems) {
		t.Errorf("ParseCTVInput = %+v", items)
	}

	w := workList(t, writeFile(t, "mixed.csv", []byte("pageURL\nexample.com\n333903271\ncom.spotify.music\n")))
	if !w.Has(item.Website) || !w.Has(item.IOSApp) || !w.Has(item.AndroidApp) || len(w.Items) != 3 {
		t.Errorf("work list = %+v", w.Items)
	}
}

func TestBuildWorkListDetails(t *testing.T) {
	// Missing, blank-after-trim and NA cells are skipped; duplicates after
	// trimming keep the first row; a float-exported id is an App Store id.
	w := workList(t, writeFile(t, "x.csv", []byte("Domain,Type\n example.com ,\nexample.com,\n  ,\nNA,\n512001.0,\nkik.android,Android\n")))
	want := []item.WorkItem{
		{Identifier: "example.com", Type: item.Website, OriginalValue: "example.com"},
		{Identifier: "512001", Type: item.IOSApp, OriginalValue: "512001"},
		{Identifier: "kik.android", Type: item.AndroidApp, OriginalValue: "kik.android"},
	}
	if !reflect.DeepEqual(w.Items, want) || w.TypeColumn != "Type" || w.Hinted != 1 {
		t.Errorf("work list = %+v (type %q, hinted %d)", w.Items, w.TypeColumn, w.Hinted)
	}
	if w.Count(item.Website) != 1 {
		t.Error("Count")
	}

	// Legacy fallback: an undetectable "Domain" column is still used.
	f := tabular.New([]string{"Domain"}, [][]string{{""}, {"NA"}})
	w, err := BuildWorkList(f)
	if err != nil || w.InputColumn != "Domain" || len(w.Items) != 0 {
		t.Errorf("fallback = %+v, %v", w, err)
	}
	if _, err := BuildWorkList(tabular.New([]string{"Notes"}, [][]string{{"hello"}})); !errors.Is(err, ErrNoRecognizableColumn) {
		t.Errorf("err = %v", err)
	}
}

func TestNormalizeHeaderless(t *testing.T) {
	one := func(header string, vals ...string) *tabular.Frame {
		rows := make([][]string, len(vals))
		for i, v := range vals {
			rows[i] = []string{v}
		}
		return tabular.New([]string{header}, rows)
	}
	col := func(f *tabular.Frame) []string { return append(f.Columns(), f.Column(0)...) }
	cases := []struct {
		in        *tabular.Frame
		pipeline  []string
		breakdown []string
	}{
		// ≥80% websites: the header is a domain; the pipeline de-duplicates.
		{one("example.com", "github.com", "example.com"), []string{"Domain", "example.com", "github.com"}, []string{"Domain", "example.com", "github.com", "example.com"}},
		// A label header over websites: Python's 80% rule still recovers it
		// for the pipeline; the preview (header not processable) does not.
		{one("Sites", "a.com", "b.com"), []string{"Domain", "Sites", "a.com", "b.com"}, []string{"Sites", "a.com", "b.com"}},
		// App ids: both recover the processable header.
		{one("284882215", "389801252"), []string{"Domain", "284882215", "389801252"}, []string{"Domain", "284882215", "389801252"}},
		// Recognised header names and artifacts are never data.
		{one("pageURL", "a.com"), []string{"pageURL", "a.com"}, []string{"pageURL", "a.com"}},
		{one(" domain ", "a.com"), []string{" domain ", "a.com"}, []string{" domain ", "a.com"}},
		{one("Unnamed: 0", "a.com", "b.com"), []string{"Unnamed: 0", "a.com", "b.com"}, []string{"Unnamed: 0", "a.com", "b.com"}},
		// Mostly non-websites under a non-processable header: unchanged.
		{one("rank", "1", "2", "a.com"), []string{"rank", "1", "2", "a.com"}, []string{"rank", "1", "2", "a.com"}},
		// No data rows: a processable header is the only item.
		{one("example.com"), []string{"Domain", "example.com"}, []string{"Domain", "example.com"}},
	}
	for _, tc := range cases {
		if got := col(NormalizeHeaderless(tc.in)); !reflect.DeepEqual(got, tc.pipeline) {
			t.Errorf("NormalizeHeaderless(%q) = %q, want %q", col(tc.in), got, tc.pipeline)
		}
		if got := col(NormalizeHeaderlessBreakdown(tc.in)); !reflect.DeepEqual(got, tc.breakdown) {
			t.Errorf("NormalizeHeaderlessBreakdown(%q) = %q, want %q", col(tc.in), got, tc.breakdown)
		}
	}
	two := tabular.New([]string{"a.com", "b"}, [][]string{{"x.com", "y"}})
	if NormalizeHeaderless(two) != two || NormalizeHeaderlessBreakdown(two) != two {
		t.Error("multi-column frame changed")
	}
}

// writeXLSX writes a one-sheet workbook; string cells become inline strings,
// ints numbers, nil an absent cell.
func writeXLSX(t *testing.T, name string, rows [][]any) string {
	t.Helper()
	var sheet strings.Builder
	sheet.WriteString(`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`)
	for r, row := range rows {
		fmt.Fprintf(&sheet, `<row r="%d">`, r+1)
		for c, v := range row {
			ref := fmt.Sprintf("%c%d", 'A'+c, r+1)
			switch v := v.(type) {
			case nil:
			case string:
				var esc bytes.Buffer
				_ = xml.EscapeText(&esc, []byte(v))
				fmt.Fprintf(&sheet, `<c r="%s" t="inlineStr"><is><t>%s</t></is></c>`, ref, esc.String())
			default:
				fmt.Fprintf(&sheet, `<c r="%s"><v>%v</v></c>`, ref, v)
			}
		}
		sheet.WriteString(`</row>`)
	}
	sheet.WriteString(`</sheetData></worksheet>`)
	parts := map[string]string{
		"_rels/.rels":                `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/></Relationships>`,
		"xl/workbook.xml":            `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets><sheet name="Sheet1" sheetId="1" r:id="rId1"/></sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/></Relationships>`,
		"xl/worksheets/sheet1.xml":   sheet.String(),
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for n, body := range parts {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return writeFile(t, name, buf.Bytes())
}
