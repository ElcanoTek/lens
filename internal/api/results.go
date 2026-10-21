// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package api

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/ElcanoTek/lens/internal/jobs"
)

// summaryKey identifies one version of an output file: a file still being
// written (a running job) is re-summarised as it grows, while paging
// through a finished one reads the summary once.
type summaryKey struct {
	path  string
	mtime int64
	size  int64
}

type summary struct {
	fieldnames []string
	totalRows  int
	quality    *counts
	types      *counts
	topIAB     *counts
}

// counts is a tally that remembers first-seen order and marshals in it, as
// the Python release's dicts did (top_iab_tier_1 is ordered by count).
type counts struct {
	keys []string
	n    map[string]int
}

func newCounts() *counts { return &counts{n: map[string]int{}} }

func (c *counts) add(key string, by int) {
	if _, ok := c.n[key]; !ok {
		c.keys = append(c.keys, key)
	}
	c.n[key] += by
}

// MarshalJSON writes the tally as an object in key order.
func (c *counts) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range c.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		key, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		b.Write(key)
		b.WriteByte(':')
		b.WriteString(strconv.Itoa(c.n[k]))
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

const summaryCacheMax = 32

// csvRows streams an output CSV as header-keyed rows, like Python's
// csv.DictReader over a file opened with errors="replace": lenient quoting,
// ragged rows allowed, invalid UTF-8 replaced. fn returns false to stop.
func csvRows(path string, header func([]string), fn func(map[string]string) bool) error {
	f, err := os.Open(path) //nolint:gosec // path built from a validated job filename
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	reader := csv.NewReader(f)
	reader.FieldsPerRecord = -1
	reader.LazyQuotes = true
	clean := func(rec []string) []string {
		out := make([]string, len(rec))
		for i, v := range rec {
			out[i] = strings.ToValidUTF8(v, "�")
		}
		return out
	}
	first, err := reader.Read()
	if errors.Is(err, io.EOF) {
		header(nil)
		return nil
	}
	if err != nil {
		return err
	}
	fields := clean(first)
	header(fields)
	for {
		rec, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			// A malformed tail (a running job mid-write) ends the scan
			// rather than failing the whole call.
			return nil
		}
		rec = clean(rec)
		row := make(map[string]string, len(fields))
		for i, name := range fields {
			if i < len(rec) {
				row[name] = rec[i]
			}
		}
		if !fn(row) {
			return nil
		}
	}
}

func (a *API) summarise(path string) (*summary, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	key := summaryKey{path: path, mtime: st.ModTime().UnixNano(), size: st.Size()}
	a.summaryMu.Lock()
	cached := a.summaries[key]
	a.summaryMu.Unlock()
	if cached != nil {
		return cached, nil
	}
	s := &summary{fieldnames: []string{}, quality: newCounts(), types: newCounts()}
	iab := newCounts()
	count := func(c *counts, v string) {
		v = strings.TrimSpace(v)
		if v == "" {
			v = "(blank)"
		}
		c.add(v, 1)
	}
	err = csvRows(path, func(h []string) {
		if h != nil {
			s.fieldnames = h
		}
	}, func(row map[string]string) bool {
		s.totalRows++
		count(s.quality, row["Quality"])
		count(s.types, row["Type"])
		count(iab, row["IAB Tier 1"])
		return true
	})
	if err != nil {
		return nil, err
	}
	// Top 15 by count, ties in first-seen order (a stable sort, as Python's).
	names := slices.Clone(iab.keys)
	sort.SliceStable(names, func(i, j int) bool { return iab.n[names[i]] > iab.n[names[j]] })
	s.topIAB = newCounts()
	for _, k := range names[:min(15, len(names))] {
		s.topIAB.add(k, iab.n[k])
	}

	a.summaryMu.Lock()
	defer a.summaryMu.Unlock()
	if _, ok := a.summaries[key]; !ok {
		if len(a.summaryOrder) >= summaryCacheMax {
			delete(a.summaries, a.summaryOrder[0])
			a.summaryOrder = a.summaryOrder[1:]
		}
		a.summaries[key] = s
		a.summaryOrder = append(a.summaryOrder, key)
	}
	return s, nil
}

func truncateCell(v string) string {
	r := []rune(v)
	if len(r) <= PreviewCellChars {
		return v
	}
	return string(r[:PreviewCellChars]) + "…"
}

func (a *API) readOutput(path string, offset, limit int, columns []string, quality string) (Body, error) {
	s, err := a.summarise(path)
	if err != nil {
		return nil, err
	}
	var unknown []string
	for _, c := range columns {
		if !slices.Contains(s.fieldnames, c) {
			unknown = append(unknown, c)
		}
	}
	if len(unknown) > 0 {
		return nil, invalidArg("Unknown column(s) %s; available: %s", pyList(unknown), pyList(s.fieldnames))
	}
	matching := s.totalRows
	if quality != "" {
		matching = 0
		for _, q := range s.quality.keys {
			if strings.EqualFold(q, quality) {
				matching += s.quality.n[q]
			}
		}
	}
	picked := columns
	if len(picked) == 0 {
		picked = s.fieldnames
	}
	rows := []map[string]string{}
	if limit > 0 && offset < matching {
		seen := 0
		err := csvRows(path, func([]string) {}, func(row map[string]string) bool {
			if quality != "" && !strings.EqualFold(strings.TrimSpace(row["Quality"]), quality) {
				return true
			}
			seen++
			if seen <= offset {
				return true
			}
			out := make(map[string]string, len(picked))
			for _, c := range picked {
				out[c] = truncateCell(row[c])
			}
			rows = append(rows, out)
			return len(rows) < limit // the page is full; the summary covers the rest
		})
		if err != nil {
			return nil, err
		}
	}
	return Body{
		"columns":       picked,
		"total_rows":    s.totalRows,
		"matching_rows": matching,
		"summary": Body{
			"quality":        s.quality,
			"type":           s.types,
			"top_iab_tier_1": s.topIAB,
		},
		"rows": rows,
	}, nil
}

// ResultsQuery are get_job_results' paging and filter arguments, loosely
// typed (see boundedInt); Columns is nil, a []string or a decoded JSON list.
type ResultsQuery struct {
	Offset, Limit, Columns, Quality any
}

// JobResults is get_job_results: a whole-file summary plus a page of rows.
// It works while the job runs (rows written so far).
func (a *API) JobResults(r *http.Request, jobID any, q ResultsQuery) (Body, error) {
	job, err := a.getJobOr404(jobID)
	if err != nil {
		return nil, err
	}
	start, err := boundedInt(q.Offset, "offset", 0, 0, 10_000_000)
	if err != nil {
		return nil, err
	}
	count, err := boundedInt(q.Limit, "limit", DefaultResultRows, 0, MaxResultRows)
	if err != nil {
		return nil, err
	}
	var columns []string
	switch c := q.Columns.(type) {
	case nil:
	case []string:
		columns = c
	case []any:
		for _, v := range c {
			s, ok := v.(string)
			if !ok {
				return nil, invalidArg("columns must be a list of column names")
			}
			columns = append(columns, s)
		}
	default:
		return nil, invalidArg("columns must be a list of column names")
	}
	quality := ""
	if q.Quality != nil {
		s, ok := q.Quality.(string)
		if !ok {
			return nil, invalidArg("quality must be a string such as 'Premium'")
		}
		quality = s
	}
	path := a.filePath(job, "output")
	if !isFile(path) {
		if job.Status == jobs.StatusQueued {
			return nil, apiErr(http.StatusConflict, "not_started", "Job %s is still queued; there are no results yet.", job.ID)
		}
		if jobs.IsTerminal(job.Status) {
			return nil, apiErr(http.StatusNotFound, "output_missing", "Job %s is %s and has no output CSV on the Lens host.", job.ID, job.Status)
		}
		return nil, apiErr(http.StatusConflict, "no_results_yet", "Job %s is %s and has no output CSV yet; try again shortly.", job.ID, job.Status)
	}
	result, err := a.readOutput(path, start, count, columns, quality)
	if err != nil {
		return nil, err
	}
	result["job"] = a.jobView(job, r, false)
	result["offset"] = start
	result["complete"] = job.Status == jobs.StatusCompleted
	switch {
	case job.Status != jobs.StatusCompleted:
		result["note"] = "Job is " + job.Status + "; these are the rows written so far."
	case result["matching_rows"].(int) > start+len(result["rows"].([]map[string]string)):
		result["note"] = "More rows remain: page with offset, or use get_download_link(file='output') for the whole CSV."
	}
	return result, nil
}
