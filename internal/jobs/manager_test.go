// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package jobs

import (
	"context"
	"encoding/json"
	"errors"
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
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"+script), 0o700); err != nil {
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

// Run pops a job off the queue before runJob takes the lock; a Cancel in
// that window used to mark the job cancelled and runJob then ran it anyway.
func TestCancelBetweenDequeueAndStartDoesNotRun(t *testing.T) {
	home, exe := fakeRunner(t, `touch "$(dirname "$0")/ran"; exit 0`)
	m := NewManager(home)
	m.Executable = exe
	_ = m.Dirs.Ensure()
	job, _ := m.Create(NewJob{InputFile: "a.csv", Mode: "auto"})
	m.mu.Lock() // what Run does
	m.queue = m.queue[1:]
	m.running[job.ID] = nil
	m.mu.Unlock()
	if err := m.Cancel(job.ID); err != nil {
		t.Fatal(err)
	}
	m.runJob(job.ID)
	if j, _ := m.Get(job.ID); j.Status != StatusCancelled {
		t.Fatalf("status = %q, want cancelled", j.Status)
	}
	if _, err := os.Stat(filepath.Join(home, "ran")); err == nil {
		t.Fatal("a cancelled job was started")
	}
}

// A Cancel while the job is marked running but its process is not started
// yet is accepted (it used to be refused as "not cancellable") and the
// process is never started.
func TestCancelWhileStartingIsHonoured(t *testing.T) {
	home, exe := fakeRunner(t, `touch "$(dirname "$0")/ran"; exit 0`)
	m := NewManager(home)
	m.Executable = exe
	_ = m.Dirs.Ensure()
	job, _ := m.Create(NewJob{InputFile: "a.csv", Mode: "auto", CustomCategories: json.RawMessage(`{"a":1}`)})
	var cancelErr error
	// FormatCategories runs in runJob after the job is marked running and
	// before the process starts.
	m.FormatCategories = func(raw json.RawMessage) ([]byte, error) {
		cancelErr = m.Cancel(job.ID)
		return raw, nil
	}
	m.mu.Lock()
	m.queue = m.queue[1:]
	m.running[job.ID] = nil
	m.mu.Unlock()
	m.runJob(job.ID)
	if cancelErr != nil {
		t.Fatalf("cancel while starting: %v", cancelErr)
	}
	if j, _ := m.Get(job.ID); j.Status != StatusCancelled || j.FinishedAt == "" {
		t.Fatalf("job = %+v, want cancelled", j)
	}
	if _, err := os.Stat(filepath.Join(home, "ran")); err == nil {
		t.Fatal("a cancelled job was started")
	}
}

// breakState makes every _jobs.json write fail, even as root: the state
// path becomes a non-empty directory, so the rename over it is refused.
func breakState(t *testing.T, m *Manager) func() {
	t.Helper()
	_ = os.Remove(m.Dirs.StatePath())
	if err := os.MkdirAll(filepath.Join(m.Dirs.StatePath(), "blocker"), 0o755); err != nil {
		t.Fatal(err)
	}
	return func() { _ = os.RemoveAll(m.Dirs.StatePath()) }
}

func TestUnsavedTransitionsAreRefused(t *testing.T) {
	home, exe := fakeRunner(t, `exit 0`)
	m := NewManager(home)
	m.Executable = exe
	if err := m.Dirs.Ensure(); err != nil {
		t.Fatal(err)
	}
	queued, err := m.Create(NewJob{InputFile: "a.csv", Mode: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	restore := breakState(t, m)

	if _, err := m.Create(NewJob{InputFile: "b.csv", Mode: "auto"}); !errors.Is(err, ErrNotSaved) {
		t.Fatalf("Create with unwritable state = %v, want ErrNotSaved", err)
	}
	if n := len(m.List()); n != 1 || m.QueueLength() != 1 {
		t.Fatalf("unsaved job kept in memory: %d jobs, %d queued", n, m.QueueLength())
	}
	if err := m.Cancel(queued.ID); !errors.Is(err, ErrNotSaved) {
		t.Fatalf("Cancel with unwritable state = %v, want ErrNotSaved", err)
	}
	if j, _ := m.Get(queued.ID); j.Status != StatusQueued || m.QueuePosition(queued.ID) != 1 {
		t.Fatalf("unsaved cancel applied: %+v", j)
	}

	// A start that can't be recorded never runs.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)
	done := waitFor(t, m, queued.ID, StatusFailed)
	if done.ReturnCode != nil || !strings.Contains(done.Error, "could not save job state") {
		t.Fatalf("unsaved start = %+v", done)
	}

	restore()
	if err := os.WriteFile(filepath.Join(home, "managed-files", "outputs", queued.ID+".log"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.jobs[queued.ID].LogFile = queued.ID + ".log"
	m.mu.Unlock()
	restore = breakState(t, m)
	defer restore()
	if err := m.Delete(queued.ID); !errors.Is(err, ErrNotSaved) {
		t.Fatalf("Delete with unwritable state = %v, want ErrNotSaved", err)
	}
	if _, ok := m.Get(queued.ID); !ok || !exists(filepath.Join(home, "managed-files", "outputs", queued.ID+".log")) {
		t.Fatal("unsaved delete removed the job or its files")
	}
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }
