// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package logx is Lens's logging: log/slog with the line format the Python
// release wrote, which the dashboard's live log view and operators' greps
// expect:
//
//	2026-10-21 14:03:07,123 - orchestration - INFO - Processing 10 items
//
// Setup routes INFO and above to the run's log file and WARNING and above to
// stdout, like the CLI always did. Before Setup, records go to stderr at
// WARNING and above, so tests and library callers stay quiet.
package logx

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type sink struct {
	w     io.Writer
	level slog.Level
}

var (
	mu    sync.Mutex
	sinks = []sink{{w: os.Stderr, level: slog.LevelWarn}}
	file  *os.File
)

// Setup sends INFO+ to logFile (created with its parent directory) and
// consoleLevel+ to console. It replaces any earlier setup.
func Setup(logFile string, console io.Writer, consoleLevel slog.Level) error {
	mu.Lock()
	defer mu.Unlock()
	if file != nil {
		_ = file.Close()
		file = nil
	}
	sinks = nil
	if logFile != "" {
		if err := os.MkdirAll(filepath.Dir(logFile), 0o755); err != nil { //nolint:gosec // log dir is world-readable like the Python release's
			return err
		}
		f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644) //nolint:gosec // operator-chosen path
		if err != nil {
			return err
		}
		file = f
		sinks = append(sinks, sink{w: f, level: slog.LevelInfo})
	}
	if console != nil {
		sinks = append(sinks, sink{w: console, level: consoleLevel})
	}
	return nil
}

// SetOutput sends records at level and above to w only (tests).
func SetOutput(w io.Writer, level slog.Level) {
	mu.Lock()
	defer mu.Unlock()
	sinks = []sink{{w: w, level: level}}
}

// Close flushes and closes the log file, if any.
func Close() {
	mu.Lock()
	defer mu.Unlock()
	if file != nil {
		_ = file.Close()
		file = nil
	}
}

// New returns a logger whose records carry name, like Python's
// logging.getLogger(name).
func New(name string) *slog.Logger { return slog.New(&handler{name: name}) }

type handler struct {
	name  string
	attrs []slog.Attr
}

func (h *handler) Enabled(_ context.Context, level slog.Level) bool {
	mu.Lock()
	defer mu.Unlock()
	for _, s := range sinks {
		if level >= s.level {
			return true
		}
	}
	return false
}

func levelName(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "ERROR"
	case l >= slog.LevelWarn:
		return "WARNING"
	case l >= slog.LevelInfo:
		return "INFO"
	default:
		return "DEBUG"
	}
}

func (h *handler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	t := r.Time
	if t.IsZero() {
		t = time.Now()
	}
	fmt.Fprintf(&b, "%s,%03d - %s - %s - %s", t.Format("2006-01-02 15:04:05"), t.Nanosecond()/1e6, h.name, levelName(r.Level), r.Message)
	write := func(a slog.Attr) bool {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value.Resolve())
		return true
	}
	for _, a := range h.attrs {
		write(a)
	}
	r.Attrs(write)
	b.WriteByte('\n')
	line := b.String()

	mu.Lock()
	defer mu.Unlock()
	for _, s := range sinks {
		if r.Level >= s.level {
			_, _ = io.WriteString(s.w, line)
		}
	}
	return nil
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &handler{name: h.name, attrs: append(append([]slog.Attr{}, h.attrs...), attrs...)}
}

func (h *handler) WithGroup(string) slog.Handler { return h }
