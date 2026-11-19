// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package jobs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestReadProgress_StreamAndCaches(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "20261118-abc_progress.json")
	doc := map[string]any{
		"total_domains":    5,
		"successful_count": 2,
		"error_count":      1,
		"start_time":       "2026-11-18T10:00:00",
		"last_update":      "2026-11-18T10:05:00",
		"processed_domains": map[string]any{
			"a.com": map[string]any{
				"status": "success",
				"result": map[string]any{"Domain": "a.com", "Justification": "nested payload skipped"},
			},
			"b.com": map[string]any{"status": "error", "error_message": "timeout"},
			"c.com": map[string]any{"status": "retry_pending"},
			"d.com": nil,
		},
		"cost": map[string]any{"usd": 1.25, "calls": 10, "unpriced_calls": 0},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	assertSummary := func(label string, p map[string]any) {
		t.Helper()
		if p["processed"] != 3 || p["retrying"] != 1 || p["remaining"] != 2 || p["completion_percentage"] != 60.0 {
			t.Fatalf("%s: unexpected counts %#v", label, p)
		}
		if p["cost_text"] != "$1.25" || p["cost_calls"] != 10 || p["cost_unpriced_calls"] != 0 {
			t.Fatalf("%s: unexpected cost %#v", label, p)
		}
		if p["start_time"] != "2026-11-18T10:00:00" || p["last_update"] != "2026-11-18T10:05:00" {
			t.Fatalf("%s: unexpected timestamps %#v", label, p)
		}
	}

	// 1. Cold parse populates in-memory cache and on-disk sidecar.
	p1 := ReadProgress(path)
	assertSummary("cold", p1)
	if _, err := os.Stat(progressSidecarPath(path)); err != nil {
		t.Fatalf("expected sidecar cache file: %v", err)
	}

	// Mutating the returned map (like dashboardProgress deleting cost_*) must not affect cache.
	delete(p1, "cost_text")
	p1["processed"] = 999

	// 2. Warm in-memory cache hit.
	p2 := ReadProgress(path)
	assertSummary("mem-hit", p2)

	// 3. Simulated process restart: clear in-memory cache, verify on-disk sidecar hit preserves types.
	progressMemMu.Lock()
	delete(progressMem, filepath.Clean(path))
	progressMemMu.Unlock()

	p3 := ReadProgress(path)
	assertSummary("disk-hit", p3)

	// 4. Updating the file invalidates both caches cleanly.
	doc["successful_count"] = 3
	doc["processed_domains"].(map[string]any)["c.com"] = map[string]any{"status": "success"}
	raw2, _ := json.Marshal(doc)
	if err := os.WriteFile(path, raw2, 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(path, future, future)

	p4 := ReadProgress(path)
	if p4["processed"] != 4 || p4["retrying"] != 0 || p4["remaining"] != 1 {
		t.Fatalf("after update: unexpected counts %#v", p4)
	}

	// 5. RemoveProgressCache cleans up sidecar and memory entry.
	RemoveProgressCache(path)
	if _, err := os.Stat(progressSidecarPath(path)); !os.IsNotExist(err) {
		t.Fatalf("expected sidecar removed, got err=%v", err)
	}
}

func TestReadProgress_LegacySummaryAndCorrupt(t *testing.T) {
	dir := t.TempDir()

	// Legacy "summary" wrapper shape.
	legacyPath := filepath.Join(dir, "legacy_progress.json")
	legacyRaw := []byte(`{"summary":{"total_domains":10,"processed":7},"cost":{"usd":0.5,"calls":3}}`)
	if err := os.WriteFile(legacyPath, legacyRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	p := ReadProgress(legacyPath)
	if p["total_domains"] != 10.0 || p["processed"] != 7.0 || p["cost_text"] != "$0.500" {
		t.Fatalf("legacy = %#v", p)
	}

	// Truncated / corrupt JSON returns empty map and does not write a sidecar.
	corruptPath := filepath.Join(dir, "corrupt_progress.json")
	if err := os.WriteFile(corruptPath, []byte(`{"total_domains": 10, "processed_domains": {`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ReadProgress(corruptPath); len(got) != 0 {
		t.Fatalf("corrupt = %#v, want empty", got)
	}
	if _, err := os.Stat(progressSidecarPath(corruptPath)); !os.IsNotExist(err) {
		t.Fatalf("corrupt file must not write sidecar")
	}

	// Trailing garbage rejected.
	trailingPath := filepath.Join(dir, "trailing_progress.json")
	if err := os.WriteFile(trailingPath, []byte(`{"total_domains": 2} trailing`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ReadProgress(trailingPath); len(got) != 0 {
		t.Fatalf("trailing = %#v, want empty", got)
	}
}

func TestReadProgress_Concurrent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "conc_progress.json")
	raw := []byte(`{"total_domains":2,"successful_count":2,"processed_domains":{"a.com":{"status":"success"},"b.com":{"status":"success"}}}`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := ReadProgress(path)
			if p["processed"] != 2 || p["remaining"] != 0 {
				t.Errorf("concurrent read got %#v", p)
			}
		}()
	}
	wg.Wait()
}
