// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package report renders run progress as plain lines on any terminal (no
// cursor tricks, so the output reads the same in a TTY, a pipe and the
// dashboard's captured log) and optionally writes a JSONL attempt log.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ElcanoTek/lens/internal/progress"
)

// ShortenError returns the first sentence of the first line of message,
// truncated to maxLength runes with an ellipsis.
func ShortenError(message string, maxLength int) string {
	first, _, _ := strings.Cut(message, "\n")
	first = strings.TrimSpace(first)
	if first == "" {
		return "Unknown error"
	}
	if i := strings.Index(first, ". "); i != -1 {
		first = first[:i+1]
	}
	if utf8.RuneCountInString(first) <= maxLength {
		return first
	}
	runes := []rune(first)
	return strings.TrimRight(string(runes[:maxLength-1]), " \t") + "…"
}

// Shorten is ShortenError with the default 80-rune limit.
func Shorten(message string) string { return ShortenError(message, 80) }

// FormatDuration renders seconds as compact "1h2m3s" / "0m42s".
func FormatDuration(seconds float64) string {
	total := int(math.Max(0, math.Round(seconds)))
	minutes, sec := total/60, total%60
	hours, minutes := minutes/60, minutes%60
	var b strings.Builder
	if hours > 0 {
		fmt.Fprintf(&b, "%dh", hours)
	}
	fmt.Fprintf(&b, "%dm%ds", minutes, sec)
	return b.String()
}

// Options configures a Reporter.
type Options struct {
	Total     int // 0 = unknown
	Completed int
	Successes int
	Failures  int
	Quiet     bool
	Verbose   bool
	JSONLPath string
	Out       io.Writer // default os.Stdout
}

// Reporter prints per-item lines and running metrics. Safe for concurrent use.
type Reporter struct {
	mu        sync.Mutex
	out       io.Writer
	quiet     bool
	verbose   bool
	total     int
	completed int
	successes int
	failures  int
	jsonlPath string
	jsonl     *os.File
	lastLine  string
	// outcomes holds the latest outcome per item. Auto mode re-attempts
	// failures in later passes; counters track items, not attempts, or the
	// progress line would overshoot its total.
	outcomes map[string]bool
	now      func() time.Time
}

// New returns a reporter; call Start before logging.
func New(o Options) *Reporter {
	out := o.Out
	if out == nil {
		out = os.Stdout
	}
	return &Reporter{
		out: out, quiet: o.Quiet, verbose: o.Verbose, total: o.Total,
		completed: o.Completed, successes: o.Successes, failures: o.Failures,
		jsonlPath: o.JSONLPath, outcomes: map[string]bool{}, now: time.Now,
	}
}

// Start opens the JSONL log (if any) and prints the starting metrics, so a
// resumed run shows existing progress.
func (r *Reporter) Start() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.jsonlPath != "" {
		if err := os.MkdirAll(filepath.Dir(r.jsonlPath), 0o755); err != nil { //nolint:gosec // operator-chosen log location
			return err
		}
		f, err := os.OpenFile(r.jsonlPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644) //nolint:gosec // operator-chosen path
		if err != nil {
			return err
		}
		r.jsonl = f
	}
	r.emit(r.metrics())
	return nil
}

// Stop closes the JSONL log.
func (r *Reporter) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.jsonl != nil {
		_ = r.jsonl.Close()
		r.jsonl = nil
	}
}

// Attempt is one item outcome to report.
type Attempt struct {
	Domain     string
	OK         bool
	StatusCode any // int, string or nil
	Error      string
	Elapsed    time.Duration
	SizeKB     *float64
	Retries    int
	Cache      string
}

// Log records an attempt and refreshes the metrics line.
func (r *Reporter) Log(a Attempt) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	previous, seen := r.outcomes[a.Domain]
	r.outcomes[a.Domain] = a.OK
	switch {
	case !seen:
		r.completed++
		if a.OK {
			r.successes++
		} else {
			r.failures++
		}
	case previous != a.OK:
		// A retry pass changed this item's outcome.
		if a.OK {
			r.successes++
			r.failures--
		} else {
			r.failures++
			r.successes--
		}
	}
	metrics := r.metrics()
	if !r.quiet {
		r.printAttempt(a)
	}
	r.emit(metrics)
	if r.jsonl != nil {
		entry := map[string]any{
			"timestamp":    r.now().Format("2006-01-02T15:04:05.000000"),
			"domain":       a.Domain,
			"ok":           a.OK,
			"status_code":  a.StatusCode,
			"error":        nullable(a.Error),
			"elapsed_sec":  math.Round(a.Elapsed.Seconds()*100) / 100,
			"retries_used": a.Retries,
		}
		if line, err := json.Marshal(entry); err == nil {
			_, _ = r.jsonl.Write(append(line, '\n'))
		}
	}
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Retry prints a retry notice.
func (r *Reporter) Retry(domain string, attempt, total int, delay time.Duration, reason string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	fmt.Fprintf(r.out, "[%s] ⚠️ Retry %d/%d for %s in %.1fs (%s)\n", r.now().Format("15:04:05"), attempt, total, domain, delay.Seconds(), reason)
}

// Summary prints the final block.
func (r *Reporter) Summary(s progress.Summary, duration time.Duration) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	total := s.Total
	attempts := s.Successful + s.Errors
	rate := 0.0
	if attempts > 0 {
		rate = float64(s.Errors) / float64(attempts) * 100
	}
	fmt.Fprintln(r.out, "\n--- Lens run complete ---")
	fmt.Fprintf(r.out, "Total: %d\n", total)
	fmt.Fprintf(r.out, "✅ Success: %d\n", s.Successful)
	fmt.Fprintf(r.out, "❌ Failures: %d\n", s.Errors)
	fmt.Fprintf(r.out, "Failure Rate: %.1f%%\n", rate)
	fmt.Fprintf(r.out, "Duration: %s\n", FormatDuration(duration.Seconds()))
}

// Counts returns completed, successes and failures (tests and summaries).
func (r *Reporter) Counts() (completed, successes, failures int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.completed, r.successes, r.failures
}

func (r *Reporter) metrics() string {
	attempts := r.successes + r.failures
	rate := 0.0
	if attempts > 0 {
		rate = float64(r.failures) / float64(attempts) * 100
	}
	total := "?"
	if r.total > 0 {
		total = fmt.Sprint(r.total)
	}
	return fmt.Sprintf("Progress: %d/%s | Succ: %d | Fail: %d | Failure Rate: %.1f%%", r.completed, total, r.successes, r.failures, rate)
}

func (r *Reporter) emit(line string) {
	if line == r.lastLine {
		return
	}
	fmt.Fprintln(r.out, line)
	r.lastLine = line
}

func (r *Reporter) printAttempt(a Attempt) {
	ts := r.now().Format("15:04:05")
	var extra []string
	if r.verbose {
		if a.SizeKB != nil {
			extra = append(extra, fmt.Sprintf("Size: %.1fKB", *a.SizeKB))
		}
		extra = append(extra, fmt.Sprintf("Retries: %d", a.Retries))
		if a.Cache != "" {
			extra = append(extra, "Cache: "+a.Cache)
		}
	}
	var line string
	if a.OK {
		status := "OK"
		if a.StatusCode != nil {
			status = fmt.Sprint(a.StatusCode)
		}
		line = fmt.Sprintf("[%s] ✅ Success %s (%s) %.2fs", ts, a.Domain, status, a.Elapsed.Seconds())
	} else {
		errText := a.Error
		if errText == "" {
			errText = "Unknown error"
		}
		line = fmt.Sprintf("[%s] ❌ Failed %s (%s) after %.2fs", ts, a.Domain, errText, a.Elapsed.Seconds())
	}
	if len(extra) > 0 {
		line += " | " + strings.Join(extra, " | ")
	}
	fmt.Fprintln(r.out, line)
}
