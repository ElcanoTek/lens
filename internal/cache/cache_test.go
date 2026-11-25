// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package cache

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestStorePutGetAndExpiry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.db")
	st, err := Open(path, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	st.now = func() time.Time { return now }

	ctx := context.Background()
	if _, ok := st.Get(ctx, TypeWebsite, "example.com"); ok {
		t.Fatal("expected cache miss on empty store")
	}

	rating := 4.5
	count := 1200
	entry := Entry{
		ContentType:   TypeIOS,
		Identifier:    "389801252",
		LLMModel:      "~openai/gpt-luna-latest",
		ScrapeMode:    "ios_api",
		ContentLength: 42,
		AppMeta: AppMeta{
			AppName:       "Harbor Tides",
			Developer:     "Saltmarsh",
			PrimaryGenre:  "Weather",
			Rating:        &rating,
			RatingCount:   &count,
			ContentForLLM: "App Name: Harbor Tides",
		},
		Classification: Classification{
			Quality:       "Premium",
			Justification: "Well-known weather app",
			Tier1:         "News and Politics",
			Language:      "English",
			Source:        "openrouter",
		},
		ScrapedAt: "2026-11-25T12:00:00",
	}
	if err := st.Put(ctx, entry); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, ok := st.Get(ctx, TypeIOS, " 389801252 ")
	if !ok {
		t.Fatal("expected cache hit")
	}
	if got.AppMeta.AppName != "Harbor Tides" || got.Classification.Quality != "Premium" || got.AppMeta.Rating == nil || *got.AppMeta.Rating != 4.5 {
		t.Fatalf("got = %+v", got)
	}
	if !st.HasFresh(ctx, TypeIOS, "389801252") {
		t.Fatal("expected HasFresh=true")
	}

	// Advance clock past TTL: entry expires and is evicted from SQLite.
	st.now = func() time.Time { return now.Add(25 * time.Hour) }
	if _, ok := st.Get(ctx, TypeIOS, "389801252"); ok {
		t.Fatal("expected cache miss after TTL expiry")
	}
	if st.HasFresh(ctx, TypeIOS, "389801252") {
		t.Fatal("expected HasFresh=false after TTL expiry")
	}
	var remaining int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM result_cache`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("expired row was not evicted: count=%d", remaining)
	}

	// Website normalization lowercases scheme/host but preserves path/query case.
	st.now = func() time.Time { return now }
	if err := st.Put(ctx, Entry{
		ContentType: TypeWebsite,
		Identifier:  "HTTPS://Example.COM/News?Sort=Top",
		Title:       "Upper",
	}); err != nil {
		t.Fatal(err)
	}
	if got, ok := st.Get(ctx, TypeWebsite, "https://example.com/News?Sort=Top"); !ok || got.Title != "Upper" {
		t.Fatalf("case-insensitive host lookup failed: ok=%v got=%+v", ok, got)
	}
	if _, ok := st.Get(ctx, TypeWebsite, "https://example.com/news?Sort=Top"); ok {
		t.Fatal("distinct case-sensitive path collided in cache")
	}
}
