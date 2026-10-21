// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/lens/internal/jobs"
)

func TestSummaryIsCachedPerFileVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x_output.csv")
	write := func(body string) {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("Domain,Quality,IAB Tier 1\na.com,Premium,News\nb.com,,Tech\nc.com,Premium,Tech\nd.com,Standard,News\n")
	a := New(Deps{Manager: jobs.NewManager(dir)})
	first, err := a.summarise(path)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := a.summarise(path)
	if first != again {
		t.Fatal("paging re-read the whole file")
	}
	raw, _ := json.Marshal(first.quality)
	if string(raw) != `{"Premium":2,"(blank)":1,"Standard":1}` {
		t.Fatalf("quality = %s", raw)
	}
	// Ties keep first-seen order, as Python's stable sort did.
	raw, _ = json.Marshal(first.topIAB)
	if string(raw) != `{"News":2,"Tech":2}` {
		t.Fatalf("top = %s", raw)
	}
	// A growing file (a running job) is re-summarised.
	write("Domain,Quality,IAB Tier 1\na.com,Premium,News\nb.com,,Tech\nc.com,Premium,Tech\nd.com,Standard,News\ne.com,Premium,News\n")
	later := time.Now().Add(time.Second)
	_ = os.Chtimes(path, later, later)
	grown, _ := a.summarise(path)
	if grown == first || grown.totalRows != 5 {
		t.Fatalf("grown = %+v", grown)
	}
}

func TestCellsAreTruncatedForPreview(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x_output.csv")
	long := strings.Repeat("é", PreviewCellChars+5)
	if err := os.WriteFile(path, []byte("Domain,Notes\na.com,"+long+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := New(Deps{Manager: jobs.NewManager(dir)})
	out, err := a.readOutput(path, 0, 5, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	cell := out["rows"].([]map[string]string)[0]["Notes"]
	if cell != strings.Repeat("é", PreviewCellChars)+"…" {
		t.Fatalf("cell has %d runes", len([]rune(cell)))
	}
}

func TestETA(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	a := New(Deps{Manager: jobs.NewManager(t.TempDir()), Now: func() time.Time { return now }})
	job := &jobs.Job{Status: "running", StartedAt: now.Add(-100 * time.Second).Format("2006-01-02T15:04:05.000000+00:00")}
	// 10 of 50 attempted in 100s → 40 left at 10s each, plus 2 retries.
	got := a.etaSeconds(job, map[string]any{"total_domains": 50.0, "processed": 8, "retrying": 2})
	if got != 400+int(2*jobs.DeepRetrySecondsPerItem) {
		t.Fatalf("eta = %v", got)
	}
	job.StartedAt = now.Add(-3 * time.Second).Format(time.RFC3339)
	if got := a.etaSeconds(job, map[string]any{"total_domains": 50.0, "processed": 8}); got != nil {
		t.Fatalf("too early: %v", got)
	}
	job.Status = "queued"
	if got := a.etaSeconds(job, map[string]any{"total_domains": 50.0, "processed": 8}); got != nil {
		t.Fatalf("queued: %v", got)
	}
}

func TestArgumentHelpers(t *testing.T) {
	if pyRepr("it's") != `"it's"` || pyRepr("a'b\"") != `'a\'b"'` || pyRepr("x") != "'x'" {
		t.Fatal("pyRepr")
	}
	if pyList([]string{"output", "log"}) != "['output', 'log']" {
		t.Fatal("pyList")
	}
	for _, tc := range []struct {
		in   any
		want int
		ok   bool
	}{
		{nil, 7, true}, {"", 7, true}, {" 5 ", 5, true}, {5.9, 5, true}, {"abc", 0, false},
		{true, 0, false}, {"-1", 0, false}, {[]any{}, 0, false}, {101.0, 0, false},
	} {
		n, err := boundedInt(tc.in, "limit", 7, 0, 100)
		if (err == nil) != tc.ok || (tc.ok && n != tc.want) {
			t.Errorf("boundedInt(%v) = %d, %v", tc.in, n, err)
		}
	}
	for _, tc := range []struct {
		in   any
		want bool
		ok   bool
	}{
		{nil, true, true}, {false, false, true}, {"off", false, true}, {"ON", true, true}, {"maybe", false, false}, {1.0, false, false},
	} {
		b, err := optionalBool(tc.in, "x", true)
		if (err == nil) != tc.ok || (tc.ok && b != tc.want) {
			t.Errorf("optionalBool(%v) = %v, %v", tc.in, b, err)
		}
	}
}

func TestInputNamesAreSanitised(t *testing.T) {
	now := time.Date(2026, 10, 15, 14, 30, 0, 0, time.UTC)
	a := New(Deps{Manager: jobs.NewManager(t.TempDir()), Now: func() time.Time { return now }})
	if got := a.inputName(""); got != "api-list 20261015-143000.csv" {
		t.Fatal(got)
	}
	if got := a.inputName("Q4/review: ünï"); got != "Q4_review_ _n 20261015-143000.csv" {
		t.Fatal(got)
	}
	if got := a.inputName("..."); got != "api-list 20261015-143000.csv" {
		t.Fatal(got)
	}
}
