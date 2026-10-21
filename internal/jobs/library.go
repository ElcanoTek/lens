// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package jobs

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ComputeBreakdown counts websites/iOS/Android/CTV rows in an input file
// with the same detection the pipeline runs (keys websites, ios, android,
// ctv, total). It is set at startup to detect.Breakdown; nil means no
// breakdowns.
var ComputeBreakdown func(path string) (map[string]int, error)

// Breakdown returns the content breakdown for an input file, cached in a
// sidecar keyed by mtime+size so the parse runs once per upload.
func (d Dirs) Breakdown(path string) map[string]int {
	st, err := os.Stat(path)
	if err != nil {
		return map[string]int{}
	}
	cacheDir := filepath.Join(d.Inputs, BreakdownCacheDirname)
	cacheFile := filepath.Join(cacheDir, filepath.Base(path)+".json")
	type entry struct {
		Mtime     int64          `json:"mtime"`
		Size      int64          `json:"size"`
		Breakdown map[string]int `json:"breakdown"`
	}
	if raw, err := os.ReadFile(cacheFile); err == nil { //nolint:gosec // cache path derived from a library filename
		var e entry
		if json.Unmarshal(raw, &e) == nil && e.Mtime == st.ModTime().Unix() && e.Size == st.Size() {
			if e.Breakdown == nil {
				return map[string]int{}
			}
			return e.Breakdown
		}
	}
	breakdown := map[string]int{}
	if ComputeBreakdown != nil {
		if b, err := ComputeBreakdown(path); err == nil && b != nil {
			breakdown = b
		}
	}
	if err := os.MkdirAll(cacheDir, 0o755); err == nil { //nolint:gosec // cache beside the library
		if raw, err := json.Marshal(entry{Mtime: st.ModTime().Unix(), Size: st.Size(), Breakdown: breakdown}); err == nil {
			_ = os.WriteFile(cacheFile, raw, 0o644) //nolint:gosec // cache is not sensitive
		}
	}
	return breakdown
}

// Chip is one non-zero content type for the composition bar.
type Chip struct {
	Key   string  `json:"key"`
	Seg   string  `json:"seg"`
	Label string  `json:"label"`
	Count int     `json:"count"`
	Pct   float64 `json:"pct"`
}

var breakdownTypes = []struct{ key, seg, label string }{
	{"websites", "seg-site", "Websites"},
	{"ios", "seg-ios", "iOS"},
	{"android", "seg-android", "Android"},
	{"ctv", "seg-ctv", "CTV"},
}

// Chips lists non-zero types in display order; Seg picks the colour and Pct
// is the share of the total.
func Chips(b map[string]int) []Chip {
	total := b["total"]
	chips := []Chip{}
	for _, t := range breakdownTypes {
		n := b[t.key]
		if n == 0 {
			continue
		}
		pct := 0.0
		if total > 0 {
			// Python's round(x, 2): correctly rounded from the exact binary
			// value, which strconv's fixed formatting also does (x*100
			// rounding in float arithmetic does not, e.g. for 1.005).
			pct, _ = strconv.ParseFloat(strconv.FormatFloat(float64(n)/float64(total)*100, 'f', 2, 64), 64)
		}
		chips = append(chips, Chip{Key: t.key, Seg: t.seg, Label: t.label, Count: n, Pct: pct})
	}
	return chips
}

// BreakdownLabel is the plain-text summary, e.g. "318 Websites · 64 iOS".
func BreakdownLabel(b map[string]int) string {
	chips := Chips(b)
	parts := make([]string, 0, len(chips))
	for _, c := range chips {
		parts = append(parts, groupThousands(c.Count)+" "+c.Label)
	}
	return strings.Join(parts, " · ")
}

func groupThousands(n int) string {
	s := fmt.Sprint(n)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// InputFile is one library entry.
type InputFile struct {
	Name           string         `json:"name"`
	Size           string         `json:"size"`
	SizeBytes      int64          `json:"-"`
	Uploaded       string         `json:"uploaded"`
	ModTime        time.Time      `json:"-"`
	Breakdown      map[string]int `json:"breakdown"`
	BreakdownTotal int            `json:"breakdown_total"`
	BreakdownLabel string         `json:"breakdown_label"`
	BreakdownChips []Chip         `json:"breakdown_chips"`
}

// FormatSize renders a byte count like the library does.
func FormatSize(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%d KB", n/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
}

// ListInputFiles returns the library, newest first.
func (d Dirs) ListInputFiles(now time.Time) []InputFile {
	entries, err := os.ReadDir(d.Inputs)
	if err != nil {
		return []InputFile{}
	}
	files := []InputFile{}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		mtime := info.ModTime().UTC()
		var uploaded string
		// timedelta.days floors, so a file stamped slightly in the future
		// (clock skew) is not "today".
		days := int(math.Floor(now.UTC().Sub(mtime).Hours() / 24))
		switch days {
		case 0:
			uploaded = "today " + mtime.Format("15:04")
		case 1:
			uploaded = "yesterday " + mtime.Format("15:04")
		default:
			uploaded = fmt.Sprintf("%s %d", mtime.Format("Jan"), mtime.Day())
		}
		b := d.Breakdown(filepath.Join(d.Inputs, e.Name()))
		files = append(files, InputFile{
			Name: e.Name(), Size: FormatSize(info.Size()), SizeBytes: info.Size(), Uploaded: uploaded, ModTime: mtime,
			Breakdown: b, BreakdownTotal: b["total"], BreakdownLabel: BreakdownLabel(b), BreakdownChips: Chips(b),
		})
	}
	sort.SliceStable(files, func(a, b int) bool { return files[a].ModTime.After(files[b].ModTime) })
	return files
}

// ListOutputFiles returns output filenames (never _jobs.json), sorted.
func (d Dirs) ListOutputFiles() []string {
	entries, err := os.ReadDir(d.Outputs)
	if err != nil {
		return []string{}
	}
	out := []string{}
	for _, e := range entries {
		if e.Type().IsRegular() && e.Name() != JobsStateFilename {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}
