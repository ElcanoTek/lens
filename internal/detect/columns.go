// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package detect

import (
	"errors"
	"fmt"
	"strings"

	"github.com/ElcanoTek/lens/internal/item"
	"github.com/ElcanoTek/lens/internal/tabular"
)

// CandidateColumns are the header names that hold the identifiers, in
// priority order.
var CandidateColumns = []string{
	"pageURL", "pageurl", "page_url", "domain", "Domain", "url", "URL", "Url", "website",
	"Website", "site", "Site", "app_id", "appId", "app", "bundle_id", "bundleId", "package",
	"package_name",
}

// IsCandidateColumn reports whether name (case-insensitively, after
// trimming) is one of CandidateColumns: a real header, not a lost data row.
func IsCandidateColumn(name string) bool {
	n := strings.ToLower(tabular.Strip(name))
	for _, c := range CandidateColumns {
		if strings.ToLower(c) == n {
			return true
		}
	}
	return false
}

// lowerIndex mirrors a Python {col.lower(): col} dict comprehension: keys in
// first-seen order, and a later column whose lowered name repeats an
// earlier one replaces it as the value.
type lowerIndex struct {
	keys []string
	m    map[string]string
}

func newLowerIndex(columns []string, strip bool) lowerIndex {
	ix := lowerIndex{m: make(map[string]string, len(columns))}
	for _, col := range columns {
		k := strings.ToLower(col)
		if strip {
			k = tabular.Strip(k)
		}
		if _, seen := ix.m[k]; !seen {
			ix.keys = append(ix.keys, k)
		}
		ix.m[k] = col
	}
	return ix
}

func (ix lowerIndex) get(k string) (string, bool) {
	v, ok := ix.m[k]
	return v, ok
}

// InputColumn auto-detects the column holding the identifiers by name:
// an exact candidate name, then a case-insensitive one, then any column
// whose name contains url/domain/site/app/package. It returns "" when none
// matches.
func InputColumn(f *tabular.Frame) string {
	columns := f.Columns()
	for _, candidate := range CandidateColumns {
		if f.Has(candidate) {
			log.Info(fmt.Sprintf("Detected input column: %s (exact match)", candidate))
			return candidate
		}
	}
	ix := newLowerIndex(columns, false)
	for _, candidate := range CandidateColumns {
		if col, ok := ix.get(strings.ToLower(candidate)); ok {
			log.Info(fmt.Sprintf("Detected input column: %s (case-insensitive match)", col))
			return col
		}
	}
	for _, col := range columns {
		lower := strings.ToLower(col)
		for _, term := range []string{"url", "domain", "site", "app", "package"} {
			if strings.Contains(lower, term) {
				log.Info(fmt.Sprintf("Detected input column: %s (partial match)", col))
				return col
			}
		}
	}
	log.Warn(fmt.Sprintf("Could not auto-detect input column from: %s", pyList(columns)))
	return ""
}

// pyList formats names like a Python list repr, for log lines operators
// already know.
func pyList(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = "'" + strings.ReplaceAll(n, "'", `\'`) + "'"
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// Content-scoring defaults (detect_input_column_by_content).
const (
	ContentMinRatio   = 0.6
	ContentSampleSize = 200
)

// InputColumnByContent is the fallback for files with unrecognisable
// headers: it scores every column by how many of its first sampleSize
// non-blank values look processable and returns the best one if at least
// minRatio of them do ("" otherwise).
func InputColumnByContent(f *tabular.Frame, minRatio float64, sampleSize int) string {
	best, bestRatio := -1, 0.0
	for c := range f.NumColumns() {
		present := f.Present(c)
		values := make([]string, 0, min(len(present), sampleSize))
		for _, v := range present {
			if s := tabular.Strip(v); s != "" {
				values = append(values, s)
				if len(values) == sampleSize {
					break
				}
			}
		}
		if len(values) == 0 {
			continue
		}
		hits := 0
		for _, v := range values {
			if LooksProcessable(v) {
				hits++
			}
		}
		if ratio := float64(hits) / float64(len(values)); ratio > bestRatio {
			best, bestRatio = c, ratio
		}
	}
	if best >= 0 && bestRatio >= minRatio {
		col := f.Columns()[best]
		log.Info(fmt.Sprintf("Detected input column by content: %s (%d%% classifiable values)", col, int(bestRatio*100)))
		return col
	}
	return ""
}

// TypedValue is one value and the content type detected for it.
type TypedValue struct {
	Value string
	Type  item.ContentType
}

// BatchStats counts values per content type.
type BatchStats map[item.ContentType]int

// statsOrder is the order Python's stats dict iterated in.
var statsOrder = []item.ContentType{item.IOSApp, item.AndroidApp, item.Website, item.CTVApp}

// AnalyzeBatch detects the type of every value and counts them.
func AnalyzeBatch(values []string) (BatchStats, []TypedValue) {
	stats := BatchStats{item.IOSApp: 0, item.AndroidApp: 0, item.Website: 0, item.CTVApp: 0}
	results := make([]TypedValue, 0, len(values))
	for _, v := range values {
		ct := ContentType(v)
		results = append(results, TypedValue{Value: v, Type: ct})
		stats[ct]++
	}
	log.Info(fmt.Sprintf("Batch analysis: %d iOS apps, %d Android apps, %d websites, %d CTV apps",
		stats[item.IOSApp], stats[item.AndroidApp], stats[item.Website], stats[item.CTVApp]))
	return stats, results
}

// BatchType returns the content type at least 90% of values share, or
// false for a mixed (or empty) batch.
func BatchType(values []string) (item.ContentType, bool) {
	if len(values) == 0 {
		return "", false
	}
	stats, _ := AnalyzeBatch(values)
	total := float64(len(values))
	for _, ct := range statsOrder {
		if share := float64(stats[ct]) / total; share >= 0.9 {
			log.Info(fmt.Sprintf("Batch detected as predominantly %s (%d%%)", ct, int(share*100)))
			return ct, true
		}
	}
	log.Info("Mixed batch detected - will process each item by type")
	return "", false
}

// Analysis is the outcome of InputDetector.analyze(): the identifier column
// and the per-value types of its non-missing cells.
type Analysis struct {
	InputColumn string
	// BatchType is the dominant type, or "" for a mixed batch.
	BatchType item.ContentType
	// Items holds every non-missing cell of InputColumn, as written, with
	// its detected type.
	Items []TypedValue
}

// Errors Analyze reports.
var (
	ErrNoInputColumn = errors.New("could not detect input column")
	ErrNoValues      = errors.New("no valid values in input column")
)

// Analyze finds the input column (by name, then by content) and types its
// values.
func Analyze(f *tabular.Frame) (*Analysis, error) {
	col := InputColumn(f)
	if col == "" {
		col = InputColumnByContent(f, ContentMinRatio, ContentSampleSize)
	}
	if col == "" {
		log.Error("Could not detect input column")
		return nil, ErrNoInputColumn
	}
	values := f.Present(f.Index(col))
	if len(values) == 0 {
		log.Error("No valid values in input column")
		return nil, ErrNoValues
	}
	a := &Analysis{InputColumn: col}
	if ct, ok := BatchType(values); ok {
		a.BatchType = ct
	}
	_, a.Items = AnalyzeBatch(values)
	batch := "mixed"
	if a.BatchType != "" {
		batch = string(a.BatchType)
	}
	log.Info(fmt.Sprintf("Analysis complete: column=%s, batch_type=%s, items=%d", col, batch, len(a.Items)))
	return a, nil
}

// ItemsOfType returns the values detected as ct.
func (a *Analysis) ItemsOfType(ct item.ContentType) []string {
	out := make([]string, 0, len(a.Items))
	for _, tv := range a.Items {
		if tv.Type == ct {
			out = append(out, tv.Value)
		}
	}
	return out
}

// Has reports whether any value was detected as ct.
func (a *Analysis) Has(ct item.ContentType) bool {
	for _, tv := range a.Items {
		if tv.Type == ct {
			return true
		}
	}
	return false
}

// Mixed reports whether no single type dominates.
func (a *Analysis) Mixed() bool { return a.BatchType == "" }

// typeColumnCandidates are the column names that may carry an explicit
// per-row content type. When present and trustworthy, this beats value-based
// guessing (an Android package whose leaf collides with a real TLD). CTV is
// excluded: CTV inputs are routed as a whole file by IsCTVInputFile.
var typeColumnCandidates = map[string]bool{
	"type": true, "content_type": true, "content type": true, "contenttype": true,
	"media_type": true, "media type": true, "platform": true,
}

// contentTypeHints maps a normalised type cell to a content type. Only the
// three types the app/website pipeline can act on per row are mapped;
// anything else (including "ctv") is left to value detection, so a hint
// never produces an unprocessable work item.
var contentTypeHints = map[string]item.ContentType{
	"ios": item.IOSApp, "ios_app": item.IOSApp, "iosapp": item.IOSApp, "iphone": item.IOSApp,
	"ipad": item.IOSApp, "apple": item.IOSApp, "app store": item.IOSApp, "appstore": item.IOSApp,
	"itunes": item.IOSApp,
	"android": item.AndroidApp, "android_app": item.AndroidApp, "androidapp": item.AndroidApp,
	"google play": item.AndroidApp, "googleplay": item.AndroidApp, "play store": item.AndroidApp,
	"playstore": item.AndroidApp, "gplay": item.AndroidApp,
	"website": item.Website, "web": item.Website, "web site": item.Website, "site": item.Website,
	"domain": item.Website, "url": item.Website, "webpage": item.Website,
}

// ParseContentTypeHint parses one type-column cell. It returns false for an
// empty or unrecognised cell; the caller then falls back to value detection.
func ParseContentTypeHint(value string) (item.ContentType, bool) {
	ct, ok := contentTypeHints[strings.ToLower(tabular.Strip(value))]
	return ct, ok
}

// TypeColumnMinRatio is the share of recognised values a type column needs.
const TypeColumnMinRatio = 0.5

// TypeColumn finds a column that holds an explicit per-row content type: its
// name must be a known type-column name AND at least minRatio of its
// non-blank values must parse as a content type. The value check is what
// makes this safe: a generic "Type" column of content genres is rejected
// rather than trusted. The input column itself is never a type column.
// It returns "" when there is none.
func TypeColumn(f *tabular.Frame, inputColumn string, minRatio float64) string {
	inputLower := strings.ToLower(tabular.Strip(inputColumn))
	for c, col := range f.Columns() {
		name := tabular.Strip(strings.ToLower(col))
		if !typeColumnCandidates[name] {
			continue
		}
		if inputColumn != "" && name == inputLower {
			continue
		}
		present := f.Present(c)
		values := make([]string, 0, len(present))
		for _, v := range present {
			if tabular.Strip(v) != "" {
				values = append(values, v)
			}
		}
		if len(values) == 0 {
			continue
		}
		recognized := 0
		for _, v := range values {
			if _, ok := ParseContentTypeHint(v); ok {
				recognized++
			}
		}
		if share := float64(recognized) / float64(len(values)); share >= minRatio {
			log.Info(fmt.Sprintf("Detected explicit type column: %s (%d%% of values recognized)", col, int(share*100)))
			return col
		}
	}
	return ""
}
