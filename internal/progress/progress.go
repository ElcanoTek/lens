// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package progress persists per-item run state so a run can be killed and
// resumed: every item's outcome is written to the progress JSON file as soon
// as it is known (atomic write + rename), and items already marked success
// are skipped on the next run.
//
// The file format is the one the Python release wrote; the dashboard, the
// API and `lens manage` all read it, and deployed boxes keep old files.
package progress

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ElcanoTek/lens/internal/cost"
	"github.com/ElcanoTek/lens/internal/logx"
)

var logger = logx.New("progress_tracker")

// Item statuses. RetryPending records a fast-crawl failure a later pass
// will retry: it is not counted as processed, so progress bars don't read
// 100% while a retry queue is outstanding.
const (
	StatusSuccess      = "success"
	StatusError        = "error"
	StatusRetryPending = "retry_pending"
)

// timeLayout matches Python's datetime.now().isoformat() (local, naive).
const timeLayout = "2006-01-02T15:04:05.000000"

// Entry is one item's recorded outcome.
type Entry struct {
	Status         string   `json:"status"`
	Timestamp      string   `json:"timestamp"`
	ProcessingTime *float64 `json:"processing_time"`
	Result         any      `json:"result,omitempty"`
	ErrorMessage   string   `json:"error_message,omitempty"`
	ScrapeMode     string   `json:"scrape_mode,omitempty"`
}

// Tracker owns one progress file. It is safe for concurrent use.
type Tracker struct {
	path string

	mu        sync.Mutex
	saveMu    sync.Mutex
	processed map[string]*Entry
	// order is processed's keys in first-recorded order: the Python dict
	// kept it, the file is written in it, and `lens manage progress` lists
	// "recent activity" by it.
	order      []string
	lastIndex  int
	total      int
	successful int
	errors     int
	startTime  string
	lastUpdate string
	// extra keeps every other top-level key (session_info, cost,
	// custom_categories, ...) so a save never drops what it didn't write.
	extra map[string]json.RawMessage

	// Cost, when set, is snapshotted into the "cost" key on every save.
	Cost *cost.Meter
}

// Open loads path if it exists; a missing or unreadable file starts fresh.
func Open(path string) *Tracker {
	t := &Tracker{
		path:      path,
		processed: map[string]*Entry{},
		lastIndex: -1,
		extra:     map[string]json.RawMessage{"session_info": json.RawMessage("{}")},
	}
	t.load()
	return t
}

// Path is the progress file this tracker writes.
func (t *Tracker) Path() string { return t.path }

func (t *Tracker) load() {
	raw, err := os.ReadFile(t.path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			logger.Warn(fmt.Sprintf("Could not load progress file %s: %v", t.path, err))
			logger.Info("Starting with fresh progress tracking")
		}
		return
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil || doc == nil {
		logger.Warn(fmt.Sprintf("Could not load progress file %s: %v", t.path, err))
		logger.Info("Starting with fresh progress tracking")
		return
	}
	for key, value := range doc {
		switch key {
		case "processed_domains":
			var entries map[string]*Entry
			if json.Unmarshal(value, &entries) == nil && entries != nil {
				for _, k := range objectKeys(value) {
					if e := entries[k]; e != nil {
						if _, dup := t.processed[k]; !dup {
							t.order = append(t.order, k)
						}
						t.processed[k] = e
					}
				}
			}
		case "last_processed_index":
			_ = json.Unmarshal(value, &t.lastIndex)
		case "total_domains":
			t.total = jsonInt(value)
		case "successful_count":
			t.successful = jsonInt(value)
		case "error_count":
			t.errors = jsonInt(value)
		case "start_time":
			_ = json.Unmarshal(value, &t.startTime)
		case "last_update":
			_ = json.Unmarshal(value, &t.lastUpdate)
		default:
			t.extra[key] = value
		}
	}
	logger.Info(fmt.Sprintf("Loaded progress: %s", t.Summary().String()))
}

// objectKeys returns a JSON object's keys in document order.
func objectKeys(raw json.RawMessage) []string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		key, _ := tok.(string)
		var skip json.RawMessage
		if dec.Decode(&skip) != nil {
			break
		}
		keys = append(keys, key)
	}
	return keys
}

// orderedEntries marshals processed_domains in first-recorded order.
type orderedEntries struct {
	order   []string
	entries map[string]*Entry
}

func (o orderedEntries) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, id := range o.order {
		if i > 0 {
			b.WriteByte(',')
		}
		key, err := json.Marshal(id)
		if err != nil {
			return nil, err
		}
		val, err := json.Marshal(o.entries[id])
		if err != nil {
			return nil, err
		}
		b.Write(key)
		b.WriteByte(':')
		b.Write(val)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func jsonInt(raw json.RawMessage) int {
	var f float64
	if json.Unmarshal(raw, &f) != nil || math.IsNaN(f) {
		return 0
	}
	return int(f)
}

// Extra returns a raw top-level value the tracker does not interpret
// (e.g. "custom_categories", "cost"), or nil.
func (t *Tracker) Extra(key string) json.RawMessage {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.extra[key]
}

// SetExtra stores a top-level value; it is written on the next save.
func (t *Tracker) SetExtra(key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.extra[key] = raw
	return nil
}

// HasProcessedEntries reports whether any item has been recorded.
func (t *Tracker) HasProcessedEntries() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.processed) > 0
}

// Save writes the file atomically (temp file + rename).
func (t *Tracker) Save() error {
	t.saveMu.Lock()
	defer t.saveMu.Unlock()

	t.mu.Lock()
	t.lastUpdate = time.Now().Format(timeLayout)
	if t.Cost != nil {
		if raw, err := json.Marshal(t.Cost.Snapshot()); err == nil {
			t.extra["cost"] = raw
		}
	}
	doc := map[string]any{
		"processed_domains":    orderedEntries{t.order, t.processed},
		"last_processed_index": t.lastIndex,
		"total_domains":        t.total,
		"successful_count":     t.successful,
		"error_count":          t.errors,
		"start_time":           nullable(t.startTime),
		"last_update":          t.lastUpdate,
	}
	for k, v := range t.extra {
		doc[k] = v
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	t.mu.Unlock()
	if err != nil {
		return err
	}

	tmp := t.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil { //nolint:gosec // progress files are read by the dashboard process
		logger.Error(fmt.Sprintf("Could not save progress file %s: %v", t.path, err))
		return err
	}
	if err := os.Rename(tmp, t.path); err != nil {
		logger.Error(fmt.Sprintf("Could not save progress file %s: %v", t.path, err))
		return err
	}
	return nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Mark records an item's outcome and saves immediately (crash recovery).
func (t *Tracker) Mark(id, status string, result any, errorMessage string, processingTime *float64, scrapeMode string) error {
	t.mu.Lock()
	previous, ok := t.processed[id]
	if !ok {
		t.order = append(t.order, id)
	} else {
		switch {
		case previous.Status == StatusSuccess && t.successful > 0:
			t.successful--
		case previous.Status == StatusError && t.errors > 0:
			t.errors--
		}
	}
	t.processed[id] = &Entry{
		Status:         status,
		Timestamp:      time.Now().Format(timeLayout),
		ProcessingTime: processingTime,
		Result:         result,
		ErrorMessage:   errorMessage,
		ScrapeMode:     scrapeMode,
	}
	switch status {
	case StatusSuccess:
		t.successful++
	case StatusError:
		t.errors++
	}
	t.mu.Unlock()
	return t.Save()
}

// IsProcessed reports whether id already finished successfully.
func (t *Tracker) IsProcessed(id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.processed[id]
	return ok && e.Status == StatusSuccess
}

func (t *Tracker) withStatus(status string) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []string
	for id, e := range t.processed {
		if e.Status == status {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// Processed lists every recorded id.
func (t *Tracker) Processed() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, 0, len(t.processed))
	for id := range t.processed {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Successful lists ids that finished successfully.
func (t *Tracker) Successful() []string { return t.withStatus(StatusSuccess) }

// Failed lists ids whose last outcome was a terminal error.
func (t *Tracker) Failed() []string { return t.withStatus(StatusError) }

// RetryPending lists ids waiting for a later auto-mode pass.
func (t *Tracker) RetryPending() []string { return t.withStatus(StatusRetryPending) }

// Error is the last recorded error message for id.
func (t *Tracker) Error(id string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e, ok := t.processed[id]; ok {
		return e.ErrorMessage
	}
	return ""
}

// FailureDetails returns the last failure's message and the crawler rung
// (direct, deep, research) that produced it; mode is "" for entries written
// before modes were tracked.
func (t *Tracker) FailureDetails(id string) (message, mode string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e, ok := t.processed[id]; ok {
		return e.ErrorMessage, e.ScrapeMode
	}
	return "", ""
}

// FinalizePendingRetries turns leftover retry_pending entries into errors,
// for when a retry pass was skipped or interrupted, so progress can reach
// 100% and the entries read as the failures they are.
func (t *Tracker) FinalizePendingRetries() (int, error) {
	t.mu.Lock()
	n := 0
	for _, e := range t.processed {
		if e.Status == StatusRetryPending {
			e.Status = StatusError
			t.errors++
			n++
		}
	}
	t.mu.Unlock()
	if n == 0 {
		return 0, nil
	}
	logger.Info(fmt.Sprintf("Finalized %d unretried failures as errors", n))
	return n, t.Save()
}

// SetTotal sets the item count and stamps the start time on first use.
func (t *Tracker) SetTotal(total int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.total = total
	if t.startTime == "" {
		t.startTime = time.Now().Format(timeLayout)
	}
}

// ResetFailed forgets failed items so they are retried, and saves.
func (t *Tracker) ResetFailed() (int, error) {
	t.mu.Lock()
	n := 0
	kept := t.order[:0]
	for _, id := range t.order {
		if t.processed[id].Status == StatusError {
			delete(t.processed, id)
			t.errors--
			n++
			continue
		}
		kept = append(kept, id)
	}
	t.order = kept
	t.mu.Unlock()
	if err := t.Save(); err != nil {
		return n, err
	}
	logger.Info(fmt.Sprintf("Reset %d failed domains for retry", n))
	return n, nil
}

// Summary is the progress roll-up the reporter and `lens manage` show.
type Summary struct {
	Total              int
	Processed          int
	Successful         int
	Errors             int
	Retrying           int
	Remaining          int
	CompletionPercent  float64
	Elapsed            time.Duration
	HasElapsed         bool
	EstimatedRemaining time.Duration
	HasEstimate        bool
}

// Summary computes the current roll-up.
func (t *Tracker) Summary() Summary {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := Summary{Total: t.total, Successful: t.successful, Errors: t.errors}
	for _, e := range t.processed {
		if e.Status == StatusRetryPending {
			s.Retrying++
		}
	}
	// retry_pending items are still in flight: a queued pass will redo them.
	s.Processed = len(t.processed) - s.Retrying
	s.Remaining = max(0, s.Total-s.Processed)
	if s.Total > 0 {
		s.CompletionPercent = float64(s.Processed) / float64(s.Total) * 100
	}
	if start, ok := ParseTime(t.startTime); ok {
		s.Elapsed = time.Since(start)
		s.HasElapsed = true
		if s.Processed > 0 {
			per := s.Elapsed.Seconds() / float64(s.Processed)
			s.EstimatedRemaining = time.Duration(int64(per*float64(s.Remaining))) * time.Second
			s.HasEstimate = true
		}
	}
	return s
}

// ParseTime reads the timestamps progress files carry (Python isoformat,
// with or without fractional seconds or an offset).
func ParseTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	if ts, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return ts, true
	}
	for _, layout := range []string{"2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05.999999999"} {
		if ts, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return ts, true
		}
	}
	return time.Time{}, false
}

// String renders the summary on one line (for the log).
func (s Summary) String() string {
	return fmt.Sprintf("total=%d processed=%d successful=%d errors=%d retrying=%d remaining=%d completion=%.1f%%",
		s.Total, s.Processed, s.Successful, s.Errors, s.Retrying, s.Remaining, s.CompletionPercent)
}

// Print writes the human summary block the CLI ends with.
func (s Summary) Print(w io.Writer) {
	fmt.Fprintln(w, "\n📊 Progress Summary:")
	fmt.Fprintf(w, "   Total domains: %d\n", s.Total)
	fmt.Fprintf(w, "   Processed: %d\n", s.Processed)
	fmt.Fprintf(w, "   Successful: %d\n", s.Successful)
	fmt.Fprintf(w, "   Errors: %d\n", s.Errors)
	fmt.Fprintf(w, "   Remaining: %d\n", s.Remaining)
	fmt.Fprintf(w, "   Completion: %.1f%%\n", s.CompletionPercent)
	if s.HasElapsed {
		fmt.Fprintf(w, "   Elapsed time: %s\n", s.Elapsed.Round(time.Second))
	}
	if s.HasEstimate {
		fmt.Fprintf(w, "   Estimated remaining: %s\n", s.EstimatedRemaining)
	}
	fmt.Fprintln(w)
}

// EnsureDir creates the progress file's parent directory.
func (t *Tracker) EnsureDir() error {
	return os.MkdirAll(filepath.Dir(t.path), 0o755) //nolint:gosec // managed-files is world-readable by design
}
