// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package jobs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakeRunner(t *testing.T, script string) (string, string) {
	t.Helper()
	home := t.TempDir()
	exe := filepath.Join(home, "fake-lens")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"+script), 0o700); err != nil { //nolint:gosec // test helper must be executable
		t.Fatal(err)
	}
	return home, exe
}

func waitFor(t *testing.T, m *Manager, id string, want string) Job {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if j, _ := m.Get(id); j.Status == want {
			return j
		}
		time.Sleep(20 * time.Millisecond)
	}
	j, _ := m.Get(id)
	t.Fatalf("job %s status %q, want %q", id, j.Status, want)
	return j
}

func TestRunCompletesAndPersists(t *testing.T) {
	home, exe := fakeRunner(t, `echo "$@" > "$(dirname "$0")/argv"; exit 0`)
	m := NewManager(home)
	m.Executable = exe
	if err := m.Dirs.Ensure(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)
	job, err := m.Create(NewJob{InputFile: "sites.csv", Mode: "auto", ResearchFallback: false, DeepRetry: true, LLMModel: "x/y"})
	if err != nil {
		t.Fatal(err)
	}
	done := waitFor(t, m, job.ID, StatusCompleted)
	if done.ReturnCode == nil || *done.ReturnCode != 0 || done.OutputFile != job.ID+"_output.csv" {
		t.Fatalf("job = %+v", done)
	}
	argv, _ := os.ReadFile(filepath.Join(home, "argv"))
	for _, want := range []string{"run --input-csv", "--scrape-mode auto", "--llm-model x/y", "--research-fallback off"} {
		if !strings.Contains(string(argv), want) {
			t.Errorf("argv %q missing %q", argv, want)
		}
	}
	// Reload: state survives, nulls where Python wrote null.
	raw, _ := os.ReadFile(m.Dirs.StatePath())
	if !strings.Contains(string(raw), `"name": null`) {
		t.Errorf("expected null name in %s", raw)
	}
	m2 := NewManager(home)
	m2.Load()
	if j, ok := m2.Get(job.ID); !ok || j.Status != StatusCompleted {
		t.Fatalf("reloaded = %+v", j)
	}
}

func TestFailureCancelAndInterrupted(t *testing.T) {
	home, exe := fakeRunner(t, `trap 'exit 130' INT; sleep 30 & wait; exit 3`)
	m := NewManager(home)
	m.Executable = exe
	_ = m.Dirs.Ensure()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)
	running, _ := m.Create(NewJob{InputFile: "a.csv", Mode: "auto"})
	queued, _ := m.Create(NewJob{InputFile: "b.csv", Mode: "ctv"})
	waitFor(t, m, running.ID, StatusRunning)
	time.Sleep(100 * time.Millisecond) // let the process start
	if err := m.Cancel(queued.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.Cancel(running.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, running.ID, StatusCancelled)
	if err := m.Cancel(running.ID); err == nil {
		t.Fatal("cancelling a finished job should fail")
	}
	if err := m.Delete(running.ID); err != nil {
		t.Fatal(err)
	}

	// A job persisted as running is failed on load.
	state := `{"jobs": [{"id": "20260101000000-abcdef", "input_file": "x.csv", "mode": "auto", "status": "running", "created_at": "2026"},
	 {"id": "../evil", "input_file": "x.csv", "mode": "auto", "status": "queued"}]}`
	home2 := t.TempDir()
	m2 := NewManager(home2)
	_ = m2.Dirs.Ensure()
	_ = os.WriteFile(m2.Dirs.StatePath(), []byte(state), 0o600)
	m2.Load()
	j, ok := m2.Get("20260101000000-abcdef")
	if !ok || j.Status != StatusFailed || j.Error != "Interrupted by server restart" || !j.ResearchFallback {
		t.Fatalf("loaded = %+v", j)
	}
	if len(m2.List()) != 1 {
		t.Fatal("unsafe job id was loaded")
	}
}

func TestFilenamesAndStore(t *testing.T) {
	for in, want := range map[string]string{"Q1 list (2).csv": "Q1 list (2).csv", "café&co.csv": "caf__co.csv"} {
		if got, err := SafeFilename(in); err != nil || got != want {
			t.Errorf("SafeFilename(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"../x.csv", `a\b.csv`, "", "___"} {
		if _, err := SafeFilename(bad); err == nil {
			t.Errorf("SafeFilename(%q) accepted", bad)
		}
	}
	if _, err := ValidateInputFilename("x.txt"); err == nil {
		t.Error("txt accepted")
	}
	dir := t.TempDir()
	a, _ := StoreUniqueInput(dir, "l.csv", []byte("a"))
	b, _ := StoreUniqueInput(dir, "l.csv", []byte("b"))
	c, _ := StoreUniqueInput(dir, "l.csv", []byte("a"))
	if a != "l.csv" || b != "l (2).csv" || c != "l.csv" {
		t.Fatalf("store = %q %q %q", a, b, c)
	}
	if VerifyInputPayload("x.xlsx", []byte("nope")) == nil || VerifyInputPayload("x.csv", []byte("a\x00b")) == nil {
		t.Fatal("bad payloads accepted")
	}
}

func TestReadProgress(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.json")
	doc := map[string]any{
		"total_domains": 4, "successful_count": 1, "error_count": 1,
		"processed_domains": map[string]any{"a": map[string]any{"status": "success"}, "b": map[string]any{"status": "error"}, "c": map[string]any{"status": "retry_pending"}},
		"cost":              map[string]any{"usd": 0.0123, "calls": 5, "unpriced_calls": 1},
	}
	raw, _ := json.Marshal(doc)
	_ = os.WriteFile(path, raw, 0o600)
	p := ReadProgress(path)
	if p["processed"] != 2 || p["retrying"] != 1 || p["remaining"] != 2 || p["completion_percentage"] != 50.0 {
		t.Fatalf("progress = %v", p)
	}
	if p["cost_text"] != "$0.012+" {
		t.Fatalf("cost_text = %v", p["cost_text"])
	}
	if FormatCost(1234.5) != "$1,234.50" || FormatCost(0.0004) != "<$0.001" {
		t.Fatal("FormatCost")
	}
}
