// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package detect

import (
	"fmt"
	"log/slog"

	"github.com/ElcanoTek/lens/internal/item"
	"github.com/ElcanoTek/lens/internal/logx"
	"github.com/ElcanoTek/lens/internal/tabular"
)

// orchLog carries the lines orchestration.py logged while loading input.
var orchLog = logx.New("orchestration")

// HeaderlessWebsiteRatio is the share of website-like values that makes a
// one-column file's "header" a lost first domain.
const HeaderlessWebsiteRatio = 0.8

// headerlessCandidate returns the stripped header of a one-column frame
// that might be a lost first data row, and that column's stripped non-blank
// values. ok is false when the frame is not such a candidate: more than one
// column, a blank or "Unnamed: N" header (never data: pandas made that name
// up for an empty header cell), or a recognised header name.
func headerlessCandidate(f *tabular.Frame) (header string, values []string, ok bool) {
	if f.NumColumns() != 1 {
		return "", nil, false
	}
	raw := f.Columns()[0]
	header = tabular.Strip(raw)
	if header == "" || tabular.IsArtifactName(raw) || IsCandidateColumn(header) {
		return "", nil, false
	}
	present := f.Present(0)
	values = make([]string, 0, len(present))
	for _, v := range present {
		if s := tabular.Strip(v); s != "" {
			values = append(values, s)
		}
	}
	return header, values, true
}

func domainFrame(values []string) *tabular.Frame {
	rows := make([][]string, len(values))
	for i, v := range values {
		rows[i] = []string{v}
	}
	return tabular.New([]string{"Domain"}, rows)
}

// NormalizeHeaderless fixes up a headerless single-column list for the
// pipeline. The loader reads the first line as the header, so a file that
// goes straight into identifiers loses its first entry. The header is
// treated as data when at least 80% of the values are website-like (the
// Python rule) or when the header itself looks processable (the dashboard
// preview's rule, so a run never processes fewer rows than the preview
// counted — e.g. a headerless list of App Store ids). The result is one
// "Domain" column with the header prepended and duplicates removed; any
// other frame is returned unchanged.
func NormalizeHeaderless(f *tabular.Frame) *tabular.Frame {
	header, values, ok := headerlessCandidate(f)
	if !ok {
		return f
	}
	isData := LooksProcessable(header)
	if !isData && len(values) > 0 {
		websites := 0
		for _, v := range values {
			if ContentType(v) == item.Website {
				websites++
			}
		}
		isData = float64(websites)/float64(len(values)) >= HeaderlessWebsiteRatio
	}
	if !isData {
		return f
	}
	seen := make(map[string]bool, len(values)+1)
	deduped := make([]string, 0, len(values)+1)
	for _, v := range append([]string{header}, values...) {
		if !seen[v] {
			seen[v] = true
			deduped = append(deduped, v)
		}
	}
	orchLog.Info(fmt.Sprintf("Detected headerless single-column input; treating all %d rows as domain values", len(deduped)))
	return domainFrame(deduped)
}

// NormalizeHeaderlessBreakdown is the dashboard preview's variant: when the
// single header cell itself looks like a domain / app id / package, it is a
// lost first data row and is recovered. Every row is kept — the breakdown
// reports what is in the file, so nothing is de-duplicated. (A real label
// like "identifier" is not processable, so headed files are left alone.)
func NormalizeHeaderlessBreakdown(f *tabular.Frame) *tabular.Frame {
	header, values, ok := headerlessCandidate(f)
	if !ok || !LooksProcessable(header) {
		return f
	}
	return domainFrame(append([]string{header}, values...))
}

// LoadInput loads an input file for the pipeline: tabular.Load (logging
// as the orchestrator did) followed by NormalizeHeaderless.
func LoadInput(path string) (*tabular.Frame, error) {
	f, err := tabular.Load(path, orchLog)
	if err != nil {
		return nil, err
	}
	return NormalizeHeaderless(f), nil
}

// inputError carries operator-facing messages kept verbatim from Python.
type inputError string

func (e inputError) Error() string { return string(e) }

// ErrNoRecognizableColumn: neither detection nor the legacy "Domain"
// column found the identifiers.
const ErrNoRecognizableColumn = inputError("Input file must contain a recognizable column (pageURL, domain, url, etc.) or a 'Domain' column")

// WorkList is the website/app work list built from an input frame.
type WorkList struct {
	Items []item.WorkItem
	// InputColumn is the column the identifiers came from.
	InputColumn string
	// TypeColumn is the explicit per-row type column honoured, or "".
	TypeColumn string
	// Hinted is how many items took their type from TypeColumn.
	Hinted int
}

// Has reports whether any item is of type ct.
func (w *WorkList) Has(ct item.ContentType) bool {
	for _, it := range w.Items {
		if it.Type == ct {
			return true
		}
	}
	return false
}

// Count returns how many items are of type ct.
func (w *WorkList) Count(ct item.ContentType) int {
	n := 0
	for _, it := range w.Items {
		if it.Type == ct {
			n++
		}
	}
	return n
}

// BuildWorkList turns a loaded (and NormalizeHeaderless'd) frame into the
// website/app work list, as SiteAnalysisOrchestrator._load_input_data did
// after its CTV checks: detect the identifier column (falling back to a
// "Domain" column), drop missing cells, trim, drop duplicates (first row
// wins), then type every identifier — from an explicit per-row type column
// when the file carries a trustworthy one (e.g. a re-uploaded results export
// with a Type column), else by value.
//
// Unlike the Python release, identifiers go through SpreadsheetCell, so a
// whole-number id exported as "512001.0" is the App Store id 512001 rather
// than a website, and cells that are blank after trimming are skipped
// instead of becoming empty work items.
func BuildWorkList(f *tabular.Frame) (*WorkList, error) {
	var column string
	if a, err := Analyze(f); err == nil {
		column = a.InputColumn
	} else {
		// Fallback to the legacy "Domain" column behaviour.
		if !f.Has("Domain") {
			return nil, ErrNoRecognizableColumn
		}
		column = "Domain"
	}
	orchLog.Info(fmt.Sprintf("Using input column: %s", column))
	col := f.Index(column)

	seen := make(map[string]bool, f.NumRows())
	rows := make([]int, 0, f.NumRows())
	values := make([]string, 0, f.NumRows())
	for r := range f.NumRows() {
		raw := f.Cell(r, col)
		if tabular.IsNA(raw) {
			continue
		}
		v := SpreadsheetCell(raw)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		rows = append(rows, r)
		values = append(values, v)
	}
	deduped := f.SelectRows(rows)

	w := &WorkList{InputColumn: column, Items: make([]item.WorkItem, 0, len(values))}
	w.TypeColumn = TypeColumn(deduped, column, TypeColumnMinRatio)
	hintCol := -1
	if w.TypeColumn != "" {
		hintCol = deduped.Index(w.TypeColumn)
	}
	for i, v := range values {
		ct, hinted := item.ContentType(""), false
		if hintCol >= 0 {
			ct, hinted = ParseContentTypeHint(deduped.Cell(i, hintCol))
		}
		if hinted {
			w.Hinted++
		} else {
			ct = ContentType(v)
		}
		w.Items = append(w.Items, item.WorkItem{Identifier: v, Type: ct, OriginalValue: v})
	}
	if w.TypeColumn != "" {
		orchLog.Info(fmt.Sprintf("Applied explicit '%s' column to %d of %d rows", w.TypeColumn, w.Hinted, len(w.Items)))
	}
	orchLog.Info(fmt.Sprintf("Content type breakdown: %d iOS apps, %d Android apps, %d websites",
		w.Count(item.IOSApp), w.Count(item.AndroidApp), w.Count(item.Website)))
	return w, nil
}

// Breakdown counts how many websites, iOS apps, Android apps or CTV
// channels the file at path holds, with the detection the pipeline runs, for
// the dashboard's composition bar. Keys: "websites", "ios", "android" and
// "total" for app/website lists; "ctv" and "total" for CTV lists. A file
// with nothing processable yields an empty map. Errors are load errors
// (unreadable file, wrong suffix, malformed workbook).
func Breakdown(path string) (map[string]int, error) {
	f, err := tabular.Load(path, slog.New(slog.DiscardHandler))
	if err != nil {
		return nil, err
	}
	return BreakdownFrame(f), nil
}

// BreakdownFrame is Breakdown on an already loaded frame.
func BreakdownFrame(f *tabular.Frame) map[string]int {
	f = NormalizeHeaderlessBreakdown(f)
	if f.Empty() {
		return map[string]int{}
	}

	// CTV is recognised by its column headers, not per row, so the whole
	// file is CTV when it matches. Blank cells are not channels.
	if IsCTVInputFile(f) {
		cols := DetectCTVColumns(f)
		column := cols.BundleID
		if column == "" {
			column = cols.AppName
		}
		c := 0
		if column != "" {
			c = f.Index(column)
		}
		total := 0
		for r := range f.NumRows() {
			if SpreadsheetCell(f.Cell(r, c)) != "" {
				total++
			}
		}
		if total == 0 {
			return map[string]int{}
		}
		return map[string]int{"ctv": total, "total": total}
	}

	column := InputColumn(f)
	if column == "" {
		column = InputColumnByContent(f, ContentMinRatio, ContentSampleSize)
	}
	if column == "" {
		return map[string]int{}
	}
	// Honour an explicit type column exactly like the pipeline, so the
	// preview matches how the file will be processed.
	typeColumn := TypeColumn(f, column, TypeColumnMinRatio)
	col, hintCol := f.Index(column), -1
	if typeColumn != "" {
		hintCol = f.Index(typeColumn)
	}
	counts := map[item.ContentType]int{}
	total := 0
	for r := range f.NumRows() {
		v := SpreadsheetCell(f.Cell(r, col))
		if v == "" {
			continue
		}
		ct, ok := item.ContentType(""), false
		if hintCol >= 0 {
			ct, ok = ParseContentTypeHint(f.Cell(r, hintCol))
		}
		if !ok {
			ct = ContentType(v)
		}
		switch ct {
		case item.Website, item.IOSApp, item.AndroidApp:
			counts[ct]++
			total++
		}
	}
	if total == 0 {
		return map[string]int{}
	}
	return map[string]int{
		"websites": counts[item.Website],
		"ios":      counts[item.IOSApp],
		"android":  counts[item.AndroidApp],
		"total":    total,
	}
}
