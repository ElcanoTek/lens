// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package cache provides a persistent cross-job SQLite cache for scraped
// website/app content and standard LLM classifications so re-scraped sites
// and apps are served from cache instead of re-hitting websites, app stores,
// or OpenRouter.
package cache

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // Pure-Go SQLite driver (keeps CGO_ENABLED=0).
)

// ContentType constants for cache keys.
const (
	TypeWebsite = "website"
	TypeIOS     = "ios"
	TypeAndroid = "android"
)

// maxCachedContentBytes bounds stored source text so cache.db stays compact
// across tens of thousands of sites while keeping enough text for custom
// category decision calls.
const maxCachedContentBytes = 64 * 1024

// AppMeta holds cached mobile-app store metadata.
type AppMeta struct {
	AppName       string   `json:"app_name,omitempty"`
	Developer     string   `json:"developer,omitempty"`
	PrimaryGenre  string   `json:"primary_genre,omitempty"`
	Category      string   `json:"category,omitempty"`
	Description   string   `json:"description,omitempty"`
	Rating        *float64 `json:"rating,omitempty"`
	RatingCount   *int     `json:"rating_count,omitempty"`
	Downloads     string   `json:"downloads,omitempty"`
	ContentForLLM string   `json:"content_for_llm,omitempty"`
}

// Classification holds cached standard classification fields.
type Classification struct {
	Quality          string `json:"quality,omitempty"`
	Justification    string `json:"justification,omitempty"`
	Tier1            string `json:"tier1,omitempty"`
	Tier2            string `json:"tier2,omitempty"`
	Tier3            string `json:"tier3,omitempty"`
	Description      string `json:"description,omitempty"`
	Language         string `json:"language,omitempty"`
	PoliticalLeaning string `json:"political_leaning,omitempty"`
	AudienceSize     string `json:"audience_size,omitempty"`
	Source           string `json:"source,omitempty"`
}

// Entry is one cached website or mobile app result.
type Entry struct {
	ContentType    string
	Identifier     string
	LLMModel       string
	ScrapeMode     string
	BotProtection  string
	Title          string
	Content        string
	ContentLength  int
	AppMeta        AppMeta
	Classification Classification
	ScrapedAt      string
	UpdatedAt      time.Time
}

// Store is a concurrent-safe SQLite result cache shared across jobs.
type Store struct {
	db  *sql.DB
	ttl time.Duration
	now func() time.Time
}

const schemaSQL = `
CREATE TABLE IF NOT EXISTS result_cache (
	content_type        TEXT NOT NULL,
	identifier          TEXT NOT NULL,
	llm_model           TEXT NOT NULL,
	scrape_mode         TEXT NOT NULL,
	bot_protection      TEXT NOT NULL,
	title               TEXT NOT NULL,
	content             TEXT NOT NULL,
	content_length      INTEGER NOT NULL,
	app_meta_json       TEXT NOT NULL,
	classification_json TEXT NOT NULL,
	scraped_at          TEXT NOT NULL,
	updated_at          INTEGER NOT NULL,
	PRIMARY KEY (content_type, identifier)
);
`

// Open opens or creates the SQLite cache at path with the given TTL.
// A non-positive ttl disables expiration.
func Open(path string, ttl time.Duration) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("cache path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // managed-files is operator-readable
		return nil, fmt.Errorf("create cache dir: %w", err)
	}
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open cache db: %w", err)
	}
	// Single connection keeps SQLite writes serialized in-process without SQLITE_BUSY.
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(context.Background(), schemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init cache schema: %w", err)
	}
	return &Store{db: db, ttl: ttl, now: time.Now}, nil
}

// Close closes the underlying SQLite database.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func normalizeID(id string) string {
	return strings.ToLower(strings.TrimSpace(id))
}

// Get looks up a fresh cached entry for (contentType, identifier).
func (s *Store) Get(ctx context.Context, contentType, identifier string) (*Entry, bool) {
	if s == nil || s.db == nil {
		return nil, false
	}
	norm := normalizeID(identifier)
	if norm == "" {
		return nil, false
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT llm_model, scrape_mode, bot_protection, title, content, content_length,
		       app_meta_json, classification_json, scraped_at, updated_at
		FROM result_cache
		WHERE content_type = ? AND identifier = ?`,
		contentType, norm,
	)
	var (
		e         Entry
		appJSON   string
		clsJSON   string
		updatedAt int64
	)
	e.ContentType = contentType
	e.Identifier = norm
	if err := row.Scan(
		&e.LLMModel, &e.ScrapeMode, &e.BotProtection, &e.Title, &e.Content, &e.ContentLength,
		&appJSON, &clsJSON, &e.ScrapedAt, &updatedAt,
	); err != nil {
		return nil, false
	}
	e.UpdatedAt = time.Unix(updatedAt, 0).UTC()
	if s.ttl > 0 && s.now().Sub(e.UpdatedAt) > s.ttl {
		return nil, false
	}
	if appJSON != "" {
		if err := json.Unmarshal([]byte(appJSON), &e.AppMeta); err != nil {
			return nil, false
		}
	}
	if clsJSON != "" {
		if err := json.Unmarshal([]byte(clsJSON), &e.Classification); err != nil {
			return nil, false
		}
	}
	return &e, true
}

// HasFresh reports whether a fresh cached entry exists for (contentType, identifier).
func (s *Store) HasFresh(ctx context.Context, contentType, identifier string) bool {
	if s == nil || s.db == nil {
		return false
	}
	norm := normalizeID(identifier)
	if norm == "" {
		return false
	}
	var updatedAt int64
	err := s.db.QueryRowContext(ctx,
		`SELECT updated_at FROM result_cache WHERE content_type = ? AND identifier = ?`,
		contentType, norm,
	).Scan(&updatedAt)
	if err != nil {
		return false
	}
	if s.ttl > 0 && s.now().Sub(time.Unix(updatedAt, 0).UTC()) > s.ttl {
		return false
	}
	return true
}

// Put stores or updates a cached entry.
func (s *Store) Put(ctx context.Context, e Entry) error {
	if s == nil || s.db == nil {
		return nil
	}
	norm := normalizeID(e.Identifier)
	if norm == "" {
		return nil
	}
	content := truncateBytes(e.Content, maxCachedContentBytes)
	appJSON, err := json.Marshal(e.AppMeta)
	if err != nil {
		return err
	}
	clsJSON, err := json.Marshal(e.Classification)
	if err != nil {
		return err
	}
	updatedAt := e.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = s.now().UTC()
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO result_cache (
			content_type, identifier, llm_model, scrape_mode, bot_protection,
			title, content, content_length, app_meta_json, classification_json,
			scraped_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(content_type, identifier) DO UPDATE SET
			llm_model = excluded.llm_model,
			scrape_mode = excluded.scrape_mode,
			bot_protection = excluded.bot_protection,
			title = excluded.title,
			content = excluded.content,
			content_length = excluded.content_length,
			app_meta_json = excluded.app_meta_json,
			classification_json = excluded.classification_json,
			scraped_at = excluded.scraped_at,
			updated_at = excluded.updated_at`,
		e.ContentType, norm, e.LLMModel, e.ScrapeMode, e.BotProtection,
		e.Title, content, e.ContentLength, string(appJSON), string(clsJSON),
		e.ScrapedAt, updatedAt.Unix(),
	)
	return err
}

func truncateBytes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	// Back off to a valid UTF-8 boundary.
	n := maxBytes
	for n > 0 && (s[n]&0xc0) == 0x80 {
		n--
	}
	return s[:n]
}
