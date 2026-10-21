// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package progress

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ElcanoTek/lens/internal/cost"
)

func TestMarkCountsAndResume(t *testing.T) {
	path := filepath.Join(t.TempDir(), "progress.json")
	tr := Open(path)
	tr.SetTotal(3)
	secs := 1.5
	if err := tr.Mark("a.com", StatusSuccess, map[string]any{"quality": "Premium"}, "", &secs, "direct"); err != nil {
		t.Fatal(err)
	}
	if err := tr.Mark("b.com", StatusRetryPending, nil, "HTTP status 403", nil, "direct"); err != nil {
		t.Fatal(err)
	}
	if err := tr.Mark("c.com", StatusError, nil, "timeout", nil, "direct"); err != nil {
		t.Fatal(err)
	}
	s := tr.Summary()
	if s.Processed != 2 || s.Retrying != 1 || s.Successful != 1 || s.Errors != 1 || s.Remaining != 1 {
		t.Fatalf("summary = %+v", s)
	}

	// A later pass rescues c.com: the error count moves to success.
	if err := tr.Mark("c.com", StatusSuccess, nil, "", nil, "deep"); err != nil {
		t.Fatal(err)
	}
	re := Open(path)
	if !re.IsProcessed("a.com") || !re.IsProcessed("c.com") || re.IsProcessed("b.com") {
		t.Fatal("resume lost state")
	}
	if got := re.Summary(); got.Successful != 2 || got.Errors != 0 || got.Retrying != 1 {
		t.Fatalf("resumed summary = %+v", got)
	}
	if msg, mode := re.FailureDetails("b.com"); msg != "HTTP status 403" || mode != "direct" {
		t.Fatalf("failure details = %q %q", msg, mode)
	}
	n, err := re.FinalizePendingRetries()
	if err != nil || n != 1 {
		t.Fatalf("finalize = %d, %v", n, err)
	}
	if got := re.Summary(); got.Errors != 1 || got.Processed != 3 || got.CompletionPercent != 100 {
		t.Fatalf("finalized summary = %+v", got)
	}
}

func TestKeepsUnknownKeysAndCost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "progress.json")
	legacy := `{"processed_domains": {"x.com": {"status": "error", "timestamp": "2026-01-02T03:04:05.123456", "processing_time": null, "error_message": "boom"}},
	 "last_processed_index": -1, "total_domains": 1, "successful_count": 0, "error_count": 1,
	 "start_time": "2026-01-02T03:04:05.123456", "last_update": null, "session_info": {},
	 "custom_categories": {"categories": [{"name": "Kids", "type": "boolean", "question": "For kids?"}]},
	 "cost": {"usd": 0.25, "calls": 3, "unpriced_calls": 1, "by_model": {"m": 0.25}}}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	tr := Open(path)
	meter := cost.Restore(decode(t, tr.Extra("cost")))
	if meter.Calls() != 3 || meter.USD() != 0.25 {
		t.Fatalf("restored meter = %v %v", meter.Calls(), meter.USD())
	}
	tr.Cost = meter
	c := 0.5
	meter.Add(&c, "m")
	n, err := tr.ResetFailed()
	if err != nil || n != 1 {
		t.Fatalf("reset = %d %v", n, err)
	}
	var doc map[string]any
	raw, _ := os.ReadFile(path)
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["custom_categories"]; !ok {
		t.Fatal("custom_categories dropped on save")
	}
	if doc["cost"].(map[string]any)["usd"].(float64) != 0.75 {
		t.Fatalf("cost not saved: %v", doc["cost"])
	}
	if len(doc["processed_domains"].(map[string]any)) != 0 || doc["error_count"].(float64) != 0 {
		t.Fatalf("reset not saved: %v", doc)
	}
}

func decode(t *testing.T, raw json.RawMessage) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestCorruptFileStartsFresh(t *testing.T) {
	path := filepath.Join(t.TempDir(), "progress.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s := Open(path).Summary(); s.Total != 0 || s.Processed != 0 {
		t.Fatalf("summary = %+v", s)
	}
}
