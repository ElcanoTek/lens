// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package web is the Lens dashboard server (`lens serve`): the browser UI
// (index page, file library, job queue, live monitor), sign-in routes for the
// configured auth provider, the embedded static assets with content-hash
// versioning, the `lens submit` spool watcher, and the mount points for the
// /api/v1 REST surface and the /mcp server.
//
// Jobs run as `lens run` subprocesses through internal/jobs; the server never
// imports the pipeline. Every response carries the security headers and the
// cache policy, and HTML pages a per-response CSP nonce.
package web

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	lens "github.com/ElcanoTek/lens"
	"github.com/ElcanoTek/lens/internal/catalog"
	"github.com/ElcanoTek/lens/internal/categories"
	"github.com/ElcanoTek/lens/internal/config"
	"github.com/ElcanoTek/lens/internal/dotenv"
	"github.com/ElcanoTek/lens/internal/jobs"
	"github.com/ElcanoTek/lens/internal/logx"
)

var logger = logx.New("lens.web")

// ModelCatalog is what the dashboard needs from the model catalog
// (*catalog.Source implements it).
type ModelCatalog interface {
	// Get returns the dropdowns, fetching when the cache is stale.
	Get(ctx context.Context) catalog.Catalog
	// Allowed reports whether id may be picked for kind ("classify",
	// "research", "decision") per the cached catalog.
	Allowed(kind, id string) bool
}

// Mount is an extra handler served under Pattern (a ServeMux pattern such
// as "/api/v1/" or "/mcp"), behind the same header policy as every route.
type Mount struct {
	Pattern string
	Handler http.Handler
}

// MountFunc builds mounts from a constructed server (its Manager, Catalog
// and Dirs), e.g. the /api/v1 and /mcp handlers.
type MountFunc func(s *Server) ([]Mount, error)

// DefaultMounts are applied by Main; the binary registers the API and MCP
// handlers here (they live in packages that must not import this one).
var DefaultMounts []MountFunc

// Options configure New. Zero values mean "from the environment".
type Options struct {
	// Home is the Lens home (managed-files/, .env, config.json); default
	// LENS_HOME or the working directory.
	Home string
	// Getenv reads configuration; default os.LookupEnv.
	Getenv func(string) (string, bool)
	// Auth is the sign-in provider; default ProviderFromEnv.
	Auth AuthProvider
	// Catalog is the model catalog; default an OpenRouter-backed source.
	Catalog ModelCatalog
	// Manager runs jobs; default jobs.NewManager(Home).
	Manager *jobs.Manager
	// Mounts are extra handlers (API, MCP).
	Mounts []MountFunc
	// Now is the clock (tests).
	Now func() time.Time
	// SpoolInterval overrides SpoolPollInterval (tests).
	SpoolInterval time.Duration
	// Static and Templates override the embedded trees (tests).
	Static    fs.FS
	Templates fs.FS
}

// Server is the dashboard. Build it with New, then Start it and serve
// Handler().
type Server struct {
	home          string
	dirs          jobs.Dirs
	auth          AuthProvider
	catalog       ModelCatalog
	manager       *jobs.Manager
	templates     *template.Template
	staticVersion string
	mux           *http.ServeMux
	handler       http.Handler
	now           func() time.Time
	spoolInterval time.Duration
	// maxUpload caps each uploaded file (tests shrink it).
	maxUpload int

	wg sync.WaitGroup
}

// HomeFromEnv is LENS_HOME, or the working directory (systemd runs the
// service with WorkingDirectory=/opt/lens).
func HomeFromEnv(getenv func(string) (string, bool)) (string, error) {
	if home, ok := getenv("LENS_HOME"); ok && strings.TrimSpace(home) != "" {
		return filepath.Abs(strings.TrimSpace(home))
	}
	return os.Getwd()
}

// validateCategories re-validates persisted definitions for the manager.
func validateCategories(raw json.RawMessage) (json.RawMessage, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	spec, err := categories.Validate(v)
	if err != nil {
		return nil, err
	}
	return marshalSpec(spec)
}

// formatCategories writes the <output>.categories.json sidecar exactly as
// the Python release did (json.dumps(spec, indent=2)).
func formatCategories(raw json.RawMessage) ([]byte, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	spec, err := categories.Validate(v)
	if err != nil {
		return nil, err
	}
	return []byte(spec.PythonJSON(2)), nil
}

// New builds the server: it constructs (and so validates) the auth
// provider first, so a broken central-auth deployment fails at startup
// instead of returning 200 until the first user arrives; then it ensures the
// managed directories and hydrates the job table from _jobs.json.
func New(opts Options) (*Server, error) {
	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.LookupEnv
	}
	home := opts.Home
	if home == "" {
		h, err := HomeFromEnv(getenv)
		if err != nil {
			return nil, err
		}
		home = h
	}
	provider := opts.Auth
	if provider == nil {
		p, err := ProviderFromEnv(getenv)
		if err != nil {
			return nil, err
		}
		provider = p
	}
	cat := opts.Catalog
	if cat == nil {
		key, _ := getenv("OPENROUTER_API_KEY")
		cat = catalog.NewSource(strings.TrimSpace(key))
	}
	manager := opts.Manager
	if manager == nil {
		manager = jobs.NewManager(home)
	}
	if manager.ValidateCategories == nil {
		manager.ValidateCategories = validateCategories
	}
	if manager.FormatCategories == nil {
		manager.FormatCategories = formatCategories
	}
	staticFS, templatesFS := opts.Static, opts.Templates
	if staticFS == nil {
		sub, err := fs.Sub(lens.Static, "static")
		if err != nil {
			return nil, err
		}
		staticFS = sub
	}
	if templatesFS == nil {
		templatesFS = lens.Templates
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	s := &Server{
		home: home, dirs: manager.Dirs, auth: provider, catalog: cat, manager: manager,
		staticVersion: StaticVersion(staticFS), now: now, spoolInterval: opts.SpoolInterval,
		maxUpload: jobs.MaxUploadBytes,
	}
	if s.spoolInterval <= 0 {
		s.spoolInterval = SpoolPollInterval
	}
	tmpl, err := loadTemplates(templatesFS, staticURLFunc(s.staticVersion))
	if err != nil {
		return nil, fmt.Errorf("templates: %w", err)
	}
	s.templates = tmpl
	if err := s.dirs.Ensure(); err != nil {
		return nil, err
	}
	manager.Load()
	s.routes(staticFS)
	for _, mf := range opts.Mounts {
		mounts, err := mf(s)
		if err != nil {
			return nil, err
		}
		for _, m := range mounts {
			s.Mount(m.Pattern, m.Handler)
		}
	}
	s.handler = proxyHeaders(headerPolicy(s.staticVersion, limitBackchannelBody(s.mux)))
	return s, nil
}

// Manager is the job queue (shared with the API and MCP handlers).
func (s *Server) Manager() *jobs.Manager { return s.manager }

// Catalog is the model catalog.
func (s *Server) Catalog() ModelCatalog { return s.catalog }

// Dirs are the managed-files directories.
func (s *Server) Dirs() jobs.Dirs { return s.dirs }

// Home is the Lens home directory.
func (s *Server) Home() string { return s.home }

// Auth is the configured sign-in provider.
func (s *Server) Auth() AuthProvider { return s.auth }

// StaticVersion is the content hash appended to asset URLs.
func (s *Server) StaticVersion() string { return s.staticVersion }

// StaticURL is the versioned URL for a static asset.
func (s *Server) StaticURL(p string) string { return staticURLFunc(s.staticVersion)(p) }

// Mount serves h under pattern. Call before serving.
func (s *Server) Mount(pattern string, h http.Handler) { s.mux.Handle(pattern, h) }

// Handler is the root handler with every middleware applied.
func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) routes(staticFS fs.FS) {
	mux := http.NewServeMux()
	get := func(h http.HandlerFunc) methods { return methods{http.MethodGet: h} }
	post := func(h http.HandlerFunc) methods { return methods{http.MethodPost: h} }

	mux.Handle("/{$}", get(s.handleIndex))
	mux.Handle("/logout", post(s.handleLogout))
	mux.Handle("/signed-out", get(s.handleSignedOut))
	mux.Handle("/login", get(s.handleLogin))
	mux.Handle("/auth/login", get(s.handleLogin))
	mux.Handle("/auth/callback", get(func(w http.ResponseWriter, r *http.Request) { s.auth.Callback(w, r) }))
	mux.Handle("/auth/backchannel-logout", post(func(w http.ResponseWriter, r *http.Request) { s.auth.BackchannelLogout(w, r) }))
	mux.Handle("/jobs", post(s.handleEnqueue))
	mux.Handle("/jobs/{id}/cancel", post(s.handleCancel))
	mux.Handle("/jobs/{id}/delete", post(s.handleDeleteJob))
	mux.Handle("/files/upload", post(s.handleUpload))
	mux.Handle("/files/rename", post(s.handleRename))
	mux.Handle("/files/download/{destination}/{filename}", get(s.handleDownload))
	mux.Handle("/files/delete", post(s.handleDeleteFile))
	mux.Handle("/files/batch-delete", post(s.handleBatchDelete))
	mux.Handle("/files/batch-download", post(s.handleBatchDownload))
	mux.Handle("/api/jobs/live", get(s.handleLive))
	mux.Handle("/api/jobs/queue", get(s.handleQueue))
	mux.Handle("/health", get(s.handleHealth))
	mux.Handle("/static/", newStaticHandler(staticFS))
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { writeDetail(w, http.StatusNotFound, "") })
	s.mux = mux
}

// Start runs the job worker and the spool watcher until ctx is done; Wait
// blocks until both have stopped.
func (s *Server) Start(ctx context.Context) {
	s.wg.Add(2)
	go func() { defer s.wg.Done(); s.manager.Run(ctx) }()
	go func() { defer s.wg.Done(); s.runSpool(ctx) }()
}

// Wait waits for the goroutines Start launched.
func (s *Server) Wait() { s.wg.Wait() }

// loadEnv mirrors config.py's import-time .env load: the home's .env (or
// the nearest one) fills variables systemd did not already set.
func loadEnv(getenv func(string) (string, bool)) {
	if home, ok := getenv("LENS_HOME"); ok && strings.TrimSpace(home) != "" {
		path := filepath.Join(strings.TrimSpace(home), ".env")
		if isRegular(path) {
			_ = dotenv.Load(path, false)
		}
		return
	}
	config.LoadDotenv()
}

// Main is `lens serve [--addr 127.0.0.1:8808]`. SIGTERM/SIGINT stop
// accepting connections, let in-flight requests finish (up to 10 s) and
// stop the worker; running job subprocesses are left to systemd's KillMode,
// as with the Python service.
func Main(args []string) int {
	return serve(args, os.Stderr, nil)
}

func serve(args []string, stderr io.Writer, ready func(addr string)) int {
	fl := flag.NewFlagSet("lens serve", flag.ContinueOnError)
	fl.SetOutput(stderr)
	addr := fl.String("addr", "127.0.0.1:8808", "listen address")
	if err := fl.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	loadEnv(os.LookupEnv)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	s, err := New(Options{Mounts: DefaultMounts})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "lens serve: %v\n", err)
		return 1
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", *addr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "lens serve: %v\n", err)
		return 1
	}
	workerCtx, stopWorker := context.WithCancel(context.Background())
	defer stopWorker()
	s.Start(workerCtx)

	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	logger.Info("Lens dashboard listening on http://" + listener.Addr().String())
	if ready != nil {
		ready(listener.Addr().String())
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(listener) }()

	code := 0
	select {
	case <-ctx.Done():
		logger.Info("Shutting down")
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			_, _ = fmt.Fprintf(stderr, "lens serve: %v\n", err)
			code = 1
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	stopWorker()
	// The worker waits for running jobs' bookkeeping, i.e. for their
	// subprocesses, which systemd is stopping alongside us. Don't outlive
	// the stop timeout for it: a job still running after this is marked
	// "Interrupted by server restart" on the next start, as before.
	done := make(chan struct{})
	go func() { s.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		logger.Warn("Stopped with jobs still running")
	}
	return code
}
