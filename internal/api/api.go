// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package api is programmatic Lens: the /api/v1 REST routes and the job,
// result and download operations the MCP endpoint (internal/mcp) shares.
//
// Everything here authenticates with a scoped bearer token from
// internal/apitoken and never with a browser session, so the dashboard's
// cookie and CSRF rules and this surface cannot leak into each other.
//
// Jobs created here go through the same jobs.Manager as a browser upload or
// `lens submit`: same queue, same validation, same Runs table. The caller is
// recorded on the job (submitted_by) so a run started by Fleet is
// recognisable in the dashboard.
//
// See docs/API.md for the contract; it is what Fleet depends on, so response
// shapes, error codes and messages here must not drift.
package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ElcanoTek/lens/internal/apitoken"
	"github.com/ElcanoTek/lens/internal/catalog"
	"github.com/ElcanoTek/lens/internal/config"
	"github.com/ElcanoTek/lens/internal/jobs"
	"github.com/ElcanoTek/lens/internal/logx"
)

// Limits. Inline identifier lists are for chat-sized work; bigger lists
// belong in a file (or the dashboard). 10,000 short identifiers is ~300 KB
// of JSON.
const (
	MaxInlineItems    = 10_000
	MaxItemChars      = 512
	MaxResultRows     = 200
	DefaultResultRows = 25
	PreviewCellChars  = 600
	DefaultJobList    = 20
	MaxJobList        = 100
	LogTailLines      = 15
	// MaxFilenameBytes is comfortably under the 255-byte filename limit of
	// common filesystems, leaving room for the " (n)" suffix
	// jobs.StoreUniqueInput may add.
	MaxFilenameBytes = 200
	// MultipartOverheadBytes covers multipart framing (boundaries, part
	// headers, small fields) on top of the file.
	MultipartOverheadBytes = 1024 * 1024
	// maxFormFieldBytes bounds one non-file multipart field (Starlette's
	// max_part_size default).
	maxFormFieldBytes = 1024 * 1024
	maxFormFields     = 16
	maxFormFiles      = 1
)

// FileKinds are a job's downloadable files: "output" is the classified CSV,
// "log" the run log, "input" the list the job was given, "categories" the
// custom-category definitions it ran with.
var FileKinds = []string{"output", "log", "input", "categories"}

// JobStatuses lists every job status, sorted (the MCP list_jobs enum).
var JobStatuses = []string{
	jobs.StatusCancelled, jobs.StatusCancelling, jobs.StatusCompleted,
	jobs.StatusFailed, jobs.StatusQueued, jobs.StatusRunning,
}

// CreateJobArgs are the options create_job accepts, sorted.
var CreateJobArgs = []string{
	"content_base64", "ctv", "custom_categories", "decision_model", "deep_retry", "filename",
	"input_file", "items", "llm_model", "name", "research_fallback", "research_model",
}

var jobIDRe = regexp.MustCompile(`^[0-9]{14}-[0-9a-f]{6}$`)

// Body is a JSON response body.
type Body = map[string]any

// Error is a failure with a stable code and a message written for the
// caller. REST answers with it as {"ok": false, "error", "code"}; MCP
// returns the same body as an isError tool result.
type Error struct {
	Status  int
	Code    string
	Message string
	Details map[string]any
}

func (e *Error) Error() string { return e.Message }

// Body is the JSON error body.
func (e *Error) Body() Body {
	body := Body{"ok": false, "error": e.Message, "code": e.Code}
	if len(e.Details) > 0 {
		body["details"] = e.Details
	}
	return body
}

func apiErr(status int, code, format string, args ...any) *Error {
	msg := format
	if len(args) > 0 {
		msg = fmt.Sprintf(format, args...)
	}
	return &Error{Status: status, Code: code, Message: msg}
}

func invalidArg(format string, args ...any) *Error {
	return apiErr(http.StatusBadRequest, "invalid_argument", format, args...)
}

// Deps are what the API needs from the running service.
type Deps struct {
	// Manager is the dashboard's job queue (required). Its Dirs locate the
	// input library and outputs.
	Manager *jobs.Manager
	// Tokens is the API token store (required).
	Tokens *apitoken.Store
	// Catalog is the OpenRouter model catalog; nil offers only the
	// recommended models.
	Catalog *catalog.Source
	// Config is this instance's loaded config.json (nil: shipped defaults).
	// Option defaults and job settings report its values, because a value a
	// job does not carry is not passed to `lens run`, which then uses it.
	Config *config.Config
	// MaxUploadBytes caps an uploaded input (default jobs.MaxUploadBytes).
	MaxUploadBytes int64
	// Logger defaults to logx.New("lens.web").
	Logger *slog.Logger
	// Now is the clock (default time.Now).
	Now func() time.Time
}

// API serves /api/v1 and implements the operations MCP shares. Safe for
// concurrent use.
type API struct {
	manager   *jobs.Manager
	dirs      jobs.Dirs
	tokens    *apitoken.Store
	catalog   *catalog.Source
	cfg       *config.Config
	maxUpload int64
	log       *slog.Logger
	now       func() time.Time

	summaryMu    sync.Mutex
	summaries    map[summaryKey]*summary
	summaryOrder []summaryKey
}

// New returns the API over deps.
func New(d Deps) *API {
	a := &API{
		manager: d.Manager, dirs: d.Manager.Dirs, tokens: d.Tokens, catalog: d.Catalog, cfg: d.Config,
		maxUpload: d.MaxUploadBytes, log: d.Logger, now: d.Now, summaries: map[summaryKey]*summary{},
	}
	if a.cfg == nil {
		a.cfg = config.Default()
	}
	if a.maxUpload <= 0 {
		a.maxUpload = jobs.MaxUploadBytes
	}
	if a.log == nil {
		a.log = logx.New("lens.web")
	}
	if a.now == nil {
		a.now = time.Now
	}
	return a
}

// Logger is the API's logger (MCP logs tool failures with it).
func (a *API) Logger() *slog.Logger { return a.log }

// --- Authentication ----------------------------------------------------------

// Authenticate resolves the request's bearer token. A missing or unusable
// token is a 401 *Error; any other error is a storage failure.
func (a *API) Authenticate(r *http.Request) (*apitoken.Token, error) {
	header := r.Header.Get("Authorization")
	scheme, raw, _ := strings.Cut(header, " ")
	if !strings.EqualFold(scheme, "bearer") || strings.TrimSpace(raw) == "" {
		return nil, apiErr(http.StatusUnauthorized, "unauthorized",
			"Missing API token. Send 'Authorization: Bearer lens_…' with a token from "+
				"`lens token create` on the Lens host.")
	}
	token, err := a.tokens.Verify(r.Context(), raw)
	if err != nil {
		var te *apitoken.TokenError
		if errors.As(err, &te) {
			return nil, apiErr(http.StatusUnauthorized, "unauthorized", "%s", te.Error())
		}
		return nil, err
	}
	return token, nil
}

// RequireScope refuses a token without scope with a 403 *Error.
func RequireScope(token *apitoken.Token, scope string) error {
	if token.Allows(scope) {
		return nil
	}
	return apiErr(http.StatusForbidden, "forbidden",
		"This token lacks the %s scope (it has: %s).", scope, strings.Join(token.Scopes, " "))
}

// cappedReader counts what passes through and fails once more than limit
// bytes arrived, so a chunked body with no Content-Length cannot make the
// server buffer (or spool) an unbounded body before a size check runs: the
// reverse proxy imposes no body limit.
type cappedReader struct {
	r       io.Reader
	limit   int64
	n       int64
	tripped bool
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.tripped {
		return 0, c.err()
	}
	n, err := c.r.Read(p)
	c.n += int64(n)
	if c.n > c.limit {
		c.tripped = true
		return n, c.err()
	}
	return n, err
}

func (c *cappedReader) err() *Error {
	return apiErr(http.StatusRequestEntityTooLarge, "too_large", "Request body exceeds %d bytes", c.limit)
}

// ReadBodyCapped returns the request body, refused with a 413 too_large
// *Error as soon as it passes limit. The stream is read incrementally.
func ReadBodyCapped(r *http.Request, limit int64) ([]byte, error) {
	if r.ContentLength > limit {
		return nil, apiErr(http.StatusRequestEntityTooLarge, "too_large", "Request body exceeds %d bytes", limit)
	}
	if r.Body == nil {
		return []byte{}, nil
	}
	body := &cappedReader{r: r.Body, limit: limit}
	data, err := io.ReadAll(body)
	if body.tripped {
		return nil, body.err()
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}

// PublicBaseURL is the URL people and Fleet reach this Lens at, for links in
// responses: LENS_PUBLIC_URL when set, otherwise the request's (forwarded)
// host. r may be nil.
func PublicBaseURL(r *http.Request) string {
	if configured := strings.TrimRight(strings.TrimSpace(os.Getenv("LENS_PUBLIC_URL")), "/"); configured != "" {
		return configured
	}
	if r == nil {
		return ""
	}
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	host, _, _ = strings.Cut(host, ",")
	host = strings.TrimSpace(host)
	if host == "" {
		return ""
	}
	// "[::1]:8808" keeps its brackets; "127.0.0.1:8808" drops its port.
	var hostname string
	if strings.HasPrefix(host, "[") {
		before, _, _ := strings.Cut(host, "]")
		hostname = before + "]"
	} else if i := strings.LastIndex(host, ":"); i >= 0 {
		hostname = host[:i]
	} else {
		hostname = host
	}
	scheme := r.Header.Get("X-Forwarded-Proto")
	if scheme == "" {
		scheme = "https"
		if hostname == "127.0.0.1" || hostname == "localhost" || hostname == "[::1]" {
			scheme = "http"
		}
	}
	scheme, _, _ = strings.Cut(scheme, ",")
	return strings.TrimSpace(scheme) + "://" + host
}

func ctxOf(r *http.Request) context.Context {
	if r == nil {
		return context.Background()
	}
	return r.Context()
}
