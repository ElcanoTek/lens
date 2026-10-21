// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite" // database/sql driver "sqlite"
)

// Session policy. Application sessions are deliberately short. Expiry costs
// the user only a redirect: the code handoff signs them back in silently
// while the 30-day central Auth session is live. The short limit bounds a
// stolen Lens cookie and forces a daily re-check with Auth that the account
// is still enabled. One day absolute, 12 hours idle is the Elcano convention
// for every application session (owner decision 2026-09-15; see Auth's
// docs/AUTH_V2_IMPLEMENTATION.md "Application session conventions").
const (
	DefaultIdleSeconds     = 12 * 60 * 60
	DefaultAbsoluteSeconds = 24 * 60 * 60
	// SessionTouchSeconds is how often a validated session rewrites
	// last_seen_at / idle_expires_at. Every request reads the session; only a
	// request more than this long after the previous touch writes. The idle
	// limit therefore behaves as "12 hours minus at most one minute", never
	// longer, and a page's burst of requests costs one SQLite write instead of
	// one per request. One minute is the convention for every Elcano service
	// with its own sessions; keep it a constant, not a setting.
	SessionTouchSeconds = 60
	// LoginTransactionSeconds bounds a browser login (the lens_ui cookie).
	LoginTransactionSeconds = 10 * 60
	// RevocationEventRetentionSeconds: replay protection only needs to
	// outlive a token's acceptance window (exp plus skew, minutes). Keep a
	// week for forensics.
	RevocationEventRetentionSeconds = 7 * 24 * 60 * 60
	// DefaultAccessDBPath is LENS_ACCESS_DB's default, outside the release
	// tree so deploys and rollbacks cannot delete it.
	DefaultAccessDBPath = "/var/lib/lens/access.db"
)

// accessSchema is byte-for-byte the Python release's schema; deployed hosts
// keep their databases.
const accessSchema = `
CREATE TABLE IF NOT EXISTS access_entries (
    email TEXT PRIMARY KEY COLLATE NOCASE,
    enabled INTEGER NOT NULL DEFAULT 1 CHECK(enabled IN (0, 1)),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
    token_hash TEXT PRIMARY KEY,
    subject TEXT NOT NULL,
    email TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL,
    idle_expires_at INTEGER NOT NULL,
    absolute_expires_at INTEGER NOT NULL,
    revoked_at INTEGER
);
CREATE INDEX IF NOT EXISTS sessions_subject_idx ON sessions(subject);
CREATE TABLE IF NOT EXISTS revocation_events (
    event_id TEXT PRIMARY KEY,
    issuer TEXT NOT NULL,
    subject TEXT NOT NULL,
    issued_at INTEGER NOT NULL,
    received_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS access_provisioning_state (
    issuer TEXT NOT NULL,
    subject TEXT NOT NULL,
    email TEXT NOT NULL,
    version INTEGER NOT NULL,
    allowed INTEGER NOT NULL CHECK(allowed IN (0, 1)),
    event_id TEXT NOT NULL,
    issued_at INTEGER NOT NULL,
    received_at INTEGER NOT NULL,
    PRIMARY KEY (issuer, subject)
);
`

// CentralIdentity is a validated Lens session's owner.
type CentralIdentity struct {
	Subject string
	Email   string
}

// Store is the Lens-scoped authorization and session database: the local
// access allowlist, hashed sessions, back-channel replay records and Auth's
// provisioning versions. Safe for concurrent use; other processes (`lens
// access`) may write the same file.
type Store struct {
	path            string
	idleSeconds     int64
	absoluteSeconds int64
	db              *sql.DB
	now             func() time.Time
}

// OpenStore opens (creating and migrating if needed) the access database.
func OpenStore(ctx context.Context, path string, idleSeconds, absoluteSeconds int64) (*Store, error) {
	if idleSeconds <= 0 || absoluteSeconds <= 0 || idleSeconds > absoluteSeconds {
		return nil, errors.New("Invalid session timeouts") //nolint:staticcheck // message kept verbatim from the Python release
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// Create the file owner-only before SQLite opens it: no window in which
	// the session database is world-readable.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // G304: operator-configured LENS_ACCESS_DB
	if err != nil {
		return nil, err
	}
	_ = f.Close()
	dsn, err := sqliteDSN(path, "_pragma=busy_timeout(5000)", "_txlock=immediate")
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if _, err := db.ExecContext(ctx, "PRAGMA journal_mode = WAL"); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := db.ExecContext(ctx, accessSchema); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{path: path, idleSeconds: idleSeconds, absoluteSeconds: absoluteSeconds, db: db, now: time.Now}, nil
}

// AccessDBPath is LENS_ACCESS_DB or the deployed default.
func AccessDBPath(getenv func(string) (string, bool)) string {
	if v, ok := getenv("LENS_ACCESS_DB"); ok {
		return v
	}
	return DefaultAccessDBPath
}

func envSeconds(getenv func(string) (string, bool), name string, fallback int64) (int64, error) {
	raw, ok := getenv(name)
	if !ok {
		return fallback, nil
	}
	value, err := strconv.ParseInt(strings.ReplaceAll(strings.TrimSpace(raw), "_", ""), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer number of seconds", name)
	}
	return value, nil
}

// OpenStoreFromEnv opens LENS_ACCESS_DB with LENS_SESSION_IDLE_SECONDS and
// LENS_SESSION_ABSOLUTE_SECONDS.
func OpenStoreFromEnv(ctx context.Context, getenv func(string) (string, bool)) (*Store, error) {
	idle, err := envSeconds(getenv, "LENS_SESSION_IDLE_SECONDS", DefaultIdleSeconds)
	if err != nil {
		return nil, err
	}
	absolute, err := envSeconds(getenv, "LENS_SESSION_ABSOLUTE_SECONDS", DefaultAbsoluteSeconds)
	if err != nil {
		return nil, err
	}
	return OpenStore(ctx, AccessDBPath(getenv), idle, absolute)
}

func sqliteDSN(path string, params ...string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	// A SQLite URI, so a path containing '?' or '#' cannot be misread.
	dsn := "file:" + (&url.URL{Path: abs}).EscapedPath()
	if len(params) > 0 {
		dsn += "?" + strings.Join(params, "&")
	}
	return dsn, nil
}

// Path is the database file.
func (s *Store) Path() string { return s.path }

// IdleSeconds is the idle timeout.
func (s *Store) IdleSeconds() int64 { return s.idleSeconds }

// AbsoluteSeconds is the absolute session lifetime.
func (s *Store) AbsoluteSeconds() int64 { return s.absoluteSeconds }

// SetClock replaces the store's clock (tests).
func (s *Store) SetClock(now func() time.Time) { s.now = now }

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) timestamp() int64 { return s.now().Unix() }

// tokenHash is the stored form of a session token. Tokens are ASCII; anything
// else is not a token this store issued.
func tokenHash(token string) (string, bool) {
	for i := 0; i < len(token); i++ {
		if token[i] >= utf8.RuneSelf {
			return "", false
		}
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:]), true
}

// CSRFTokenForSession derives the CSRF token for a session token
// (HMAC-SHA256 keyed by the session token, hex). "" for a non-ASCII token.
func CSRFTokenForSession(token string) string {
	if _, ok := tokenHash(token); !ok || token == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write([]byte("lens-central-csrf-v1"))
	return hex.EncodeToString(mac.Sum(nil))
}

func withTx(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	// _txlock=immediate makes this BEGIN IMMEDIATE, as the Python did.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// GrantAccess enables email on the local allowlist.
func (s *Store) GrantAccess(ctx context.Context, email string) error {
	normalized, err := NormalizeEmail(email)
	if err != nil {
		return err
	}
	now := s.timestamp()
	_, err = s.db.ExecContext(ctx, `INSERT INTO access_entries(email, enabled, created_at, updated_at)
                VALUES(?, 1, ?, ?) ON CONFLICT(email) DO UPDATE
                SET enabled = 1, updated_at = excluded.updated_at`, normalized, now, now)
	return err
}

// RevokeAccess disables email and ends its sessions; false when it was not
// currently allowed.
func (s *Store) RevokeAccess(ctx context.Context, email string) (bool, error) {
	normalized, err := NormalizeEmail(email)
	if err != nil {
		return false, err
	}
	now := s.timestamp()
	var changed int64
	err = withTx(ctx, s.db, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			"UPDATE access_entries SET enabled = 0, updated_at = ? WHERE email = ? AND enabled = 1", now, normalized)
		if err != nil {
			return err
		}
		if changed, err = res.RowsAffected(); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			"UPDATE sessions SET revoked_at = ? WHERE email = ? AND revoked_at IS NULL", now, normalized)
		return err
	})
	return changed > 0, err
}

// ListAccess returns every enabled email, sorted.
func (s *Store) ListAccess(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT email FROM access_entries WHERE enabled = 1 ORDER BY email")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var email string
		if err := rows.Scan(&email); err != nil {
			return nil, err
		}
		out = append(out, email)
	}
	return out, rows.Err()
}

// IsAllowed reports whether email is enabled; a malformed email is not.
func (s *Store) IsAllowed(ctx context.Context, email string) (bool, error) {
	normalized, err := NormalizeEmail(email)
	if err != nil {
		return false, nil
	}
	var enabled int64
	err = s.db.QueryRowContext(ctx, "SELECT enabled FROM access_entries WHERE email = ?", normalized).Scan(&enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil && enabled != 0, err
}

// CreateSession issues a session for an allowed identity and returns its raw
// token (only the hash is stored). An email that is not on the allowlist is
// a KindAccessDenied *CentralAuthError.
func (s *Store) CreateSession(ctx context.Context, subject, email string) (string, error) {
	normalized, err := NormalizeEmail(email)
	if err != nil {
		return "", err
	}
	if subject == "" || utf8.RuneCountInString(subject) > 255 {
		return "", newError(KindGeneric, "The authenticated subject was invalid", nil)
	}
	now := s.timestamp()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	hash, _ := tokenHash(token)
	absolute := now + s.absoluteSeconds
	idle := min(now+s.idleSeconds, absolute)
	err = withTx(ctx, s.db, func(tx *sql.Tx) error {
		// Nothing else deletes session rows: validation only marks them
		// revoked and expiry is enforced by comparison, so without this every
		// login since the first deploy would stay on disk. A revoked or
		// absolutely-expired row can never validate again, so a new login is
		// a safe, cheap moment to drop them (same as Explorer).
		if _, err := tx.ExecContext(ctx,
			"DELETE FROM sessions WHERE revoked_at IS NOT NULL OR absolute_expires_at <= ?", now); err != nil {
			return err
		}
		var one int
		err := tx.QueryRowContext(ctx,
			"SELECT 1 FROM access_entries WHERE email = ? AND enabled = 1", normalized).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return newError(KindAccessDenied, "Email is not on this Lens access list", nil)
		}
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO sessions(token_hash, subject, email, created_at, last_seen_at,
                idle_expires_at, absolute_expires_at, revoked_at) VALUES(?, ?, ?, ?, ?, ?, ?, NULL)`,
			hash, subject, normalized, now, now, idle, absolute)
		return err
	})
	if err != nil {
		return "", err
	}
	return token, nil
}

// GetIdentity validates a session token: live, not revoked, within both
// timeouts and still on the allowlist. A failing row is marked revoked. A
// valid one is touched at most once per SessionTouchSeconds.
func (s *Store) GetIdentity(ctx context.Context, token string) (*CentralIdentity, error) {
	if token == "" {
		return nil, nil
	}
	hash, ok := tokenHash(token)
	if !ok {
		return nil, nil
	}
	now := s.timestamp()
	// Plain read first: most requests fall inside the touch interval and must
	// not take the write lock.
	var (
		identity                       CentralIdentity
		lastSeen, idleExp, absoluteExp int64
		enabled                        int64
	)
	err := s.db.QueryRowContext(ctx, `SELECT s.subject, s.email, s.last_seen_at, s.idle_expires_at,
                          s.absolute_expires_at, a.enabled
                FROM sessions s JOIN access_entries a ON a.email = s.email
                WHERE s.token_hash = ? AND s.revoked_at IS NULL`, hash).
		Scan(&identity.Subject, &identity.Email, &lastSeen, &idleExp, &absoluteExp, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if enabled == 0 || now >= idleExp || now >= absoluteExp {
		_, err := s.db.ExecContext(ctx,
			"UPDATE sessions SET revoked_at = ? WHERE token_hash = ? AND revoked_at IS NULL", now, hash)
		return nil, err
	}
	if now-lastSeen >= SessionTouchSeconds {
		nextIdle := min(now+s.idleSeconds, absoluteExp)
		if _, err := s.db.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ?, idle_expires_at = ?
                    WHERE token_hash = ? AND revoked_at IS NULL`, now, nextIdle, hash); err != nil {
			return nil, err
		}
	}
	return &identity, nil
}

// RevokeSession ends one session; false when it was not live.
func (s *Store) RevokeSession(ctx context.Context, token string) (bool, error) {
	if token == "" {
		return false, nil
	}
	hash, ok := tokenHash(token)
	if !ok {
		return false, nil
	}
	res, err := s.db.ExecContext(ctx,
		"UPDATE sessions SET revoked_at = ? WHERE token_hash = ? AND revoked_at IS NULL", s.timestamp(), hash)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ConsumeLogoutEvent records a back-channel logout once and ends every
// session of its subject; false for a replay.
func (s *Store) ConsumeLogoutEvent(ctx context.Context, event LogoutEvent) (bool, error) {
	now := s.timestamp()
	var inserted int64
	err := withTx(ctx, s.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM revocation_events WHERE received_at < ?",
			now-RevocationEventRetentionSeconds); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO revocation_events(event_id, issuer, subject, issued_at, received_at)
                VALUES(?, ?, ?, ?, ?)`, event.EventID, event.Issuer, event.Subject, event.IssuedAt, now)
		if err != nil {
			return err
		}
		if inserted, err = res.RowsAffected(); err != nil {
			return err
		}
		if inserted > 0 {
			_, err = tx.ExecContext(ctx,
				"UPDATE sessions SET revoked_at = ? WHERE subject = ? AND revoked_at IS NULL", now, event.Subject)
		}
		return err
	})
	return inserted > 0, err
}

// ApplyAccessProvisioning applies Auth's desired access state for one
// subject if its version is newer than the last one applied; false for an
// older or replayed event. A changed email disables the old address; a
// revoke ends the address's sessions.
func (s *Store) ApplyAccessProvisioning(ctx context.Context, event AccessProvisionEvent) (bool, error) {
	now := s.timestamp()
	applied := false
	allowed := 0
	if event.Allowed {
		allowed = 1
	}
	err := withTx(ctx, s.db, func(tx *sql.Tx) error {
		var (
			oldEmail string
			version  int64
		)
		err := tx.QueryRowContext(ctx,
			"SELECT email, version FROM access_provisioning_state WHERE issuer = ? AND subject = ?",
			event.Issuer, event.Subject).Scan(&oldEmail, &version)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			oldEmail = ""
		case err != nil:
			return err
		case version >= event.Version:
			return nil
		}
		if oldEmail != "" && oldEmail != event.Email {
			if _, err := tx.ExecContext(ctx,
				"UPDATE access_entries SET enabled = 0, updated_at = ? WHERE email = ?", now, oldEmail); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				"UPDATE sessions SET revoked_at = ? WHERE email = ? AND revoked_at IS NULL", now, oldEmail); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO access_entries(email, enabled, created_at, updated_at)
                VALUES(?, ?, ?, ?) ON CONFLICT(email) DO UPDATE
                SET enabled = excluded.enabled, updated_at = excluded.updated_at`,
			event.Email, allowed, now, now); err != nil {
			return err
		}
		if !event.Allowed {
			if _, err := tx.ExecContext(ctx,
				"UPDATE sessions SET revoked_at = ? WHERE email = ? AND revoked_at IS NULL", now, event.Email); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO access_provisioning_state(
                    issuer, subject, email, version, allowed, event_id, issued_at, received_at
                ) VALUES(?, ?, ?, ?, ?, ?, ?, ?)
                ON CONFLICT(issuer, subject) DO UPDATE SET
                    email = excluded.email, version = excluded.version,
                    allowed = excluded.allowed, event_id = excluded.event_id,
                    issued_at = excluded.issued_at, received_at = excluded.received_at`,
			event.Issuer, event.Subject, event.Email, event.Version, allowed, event.EventID, event.IssuedAt, now); err != nil {
			return err
		}
		applied = true
		return nil
	})
	return applied, err
}
