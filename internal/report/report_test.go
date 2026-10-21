// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package report

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCountersTrackDomainsNotAttempts(t *testing.T) {
	var out bytes.Buffer
	r := New(Options{Total: 2, Out: &out, Quiet: true})
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	r.Log(Attempt{Domain: "a.com", OK: false, Error: "timeout"})
	r.Log(Attempt{Domain: "a.com", OK: false, Error: "timeout again"})
	r.Log(Attempt{Domain: "b.com", OK: true})
	if c, s, f := r.Counts(); c != 2 || s != 1 || f != 1 {
		t.Fatalf("counts = %d %d %d", c, s, f)
	}
	if !strings.Contains(out.String(), "Progress: 2/2 | Succ: 1 | Fail: 1 | Failure Rate: 50.0%") {
		t.Fatalf("metrics missing:\n%s", out.String())
	}
}

func TestRetryRescueFlipsFailureToSuccess(t *testing.T) {
	r := New(Options{Total: 1, Out: &bytes.Buffer{}})
	r.Log(Attempt{Domain: "a.com", OK: false})
	r.Log(Attempt{Domain: "a.com", OK: true})
	if c, s, f := r.Counts(); c != 1 || s != 1 || f != 0 {
		t.Fatalf("counts = %d %d %d", c, s, f)
	}
}

func TestPlainLinesAndJSONL(t *testing.T) {
	var out bytes.Buffer
	jsonl := filepath.Join(t.TempDir(), "run.jsonl")
	r := New(Options{Out: &out, Verbose: true, JSONLPath: jsonl})
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	size := 12.34
	r.Log(Attempt{Domain: "a.com", OK: true, StatusCode: 200, Elapsed: 1500 * time.Millisecond, SizeKB: &size, Retries: 1})
	r.Log(Attempt{Domain: "b.com", OK: false, Error: "HTTP status 403", Elapsed: time.Second})
	r.Stop()
	text := out.String()
	for _, want := range []string{"✅ Success a.com (200) 1.50s | Size: 12.3KB | Retries: 1", "❌ Failed b.com (HTTP status 403) after 1.00s", "Progress: 2/? |"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	if strings.Contains(text, "\x1b[") {
		t.Error("output carries ANSI escapes")
	}
	raw, err := os.ReadFile(jsonl)
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(string(raw), "\n"); lines != 2 || !strings.Contains(string(raw), `"elapsed_sec":1.5`) {
		t.Fatalf("jsonl = %s", raw)
	}
}

func TestShortenAndDuration(t *testing.T) {
	cases := map[string]string{
		"":                              "Unknown error",
		"Connection refused. Try later": "Connection refused.",
		"first line\nsecond":            "first line",
		strings.Repeat("x", 100):        strings.Repeat("x", 79) + "…",
	}
	for in, want := range cases {
		if got := Shorten(in); got != want {
			t.Errorf("Shorten(%q) = %q, want %q", in, got, want)
		}
	}
	if got := FormatDuration(3723); got != "1h2m3s" {
		t.Errorf("FormatDuration = %q", got)
	}
	if got := FormatDuration(42.4); got != "0m42s" {
		t.Errorf("FormatDuration = %q", got)
	}
}
