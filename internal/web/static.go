// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

// StaticVersion is the content hash of every file under static/, used to
// version asset URLs.
//
// Browsers heuristically cache /static responses, so a deploy can pair
// fresh HTML with a stale main.js whose selectors no longer match — the live
// monitor then sits at "No progress data yet." forever. Hashing the actual
// bytes (not mtimes) means the version changes exactly when any asset's
// content changes. The walk order (lexical per directory) equals Python's
// sorted(Path.rglob("*")), so the value is the one the Python release
// computed for the same tree.
func StaticVersion(static fs.FS) string {
	digest := sha256.New()
	_ = fs.WalkDir(static, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // unreadable entries are skipped, as before
		}
		data, rerr := fs.ReadFile(static, p)
		if rerr != nil {
			return nil //nolint:nilerr // skipped like an OSError in the Python walk
		}
		digest.Write([]byte(p))
		digest.Write(data)
		return nil
	})
	return hex.EncodeToString(digest.Sum(nil))[:12]
}

// staticTypes pins the Content-Type of every asset kind we ship, matching
// what Python's mimetypes told Starlette (the system mime.types differs
// between hosts).
var staticTypes = map[string]string{
	".css":   "text/css; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".svg":   "image/svg+xml",
	".woff":  "font/woff",
	".woff2": "font/woff2",
	".ttf":   "font/ttf",
	".otf":   "font/otf",
	".png":   "image/png",
	".ico":   "image/vnd.microsoft.icon",
	".json":  "application/json",
	".txt":   "text/plain; charset=utf-8",
	".md":    "text/markdown; charset=utf-8",
	".html":  "text/html; charset=utf-8",
}

// staticHandler serves the embedded static tree under /static/ with strong
// ETags (content hashes; embedded files have no mtime) so revalidation of
// unversioned URLs is a cheap 304.
type staticHandler struct {
	fsys  fs.FS
	etags map[string]string
}

func newStaticHandler(fsys fs.FS) *staticHandler {
	h := &staticHandler{fsys: fsys, etags: map[string]string{}}
	_ = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // best effort; a missing ETag only costs a full response
		}
		if data, rerr := fs.ReadFile(fsys, p); rerr == nil {
			sum := sha256.Sum256(data)
			h.etags[p] = `"` + hex.EncodeToString(sum[:16]) + `"`
		}
		return nil
	})
	return h
}

func (h *staticHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeDetail(w, http.StatusMethodNotAllowed, "")
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/static/")
	name = path.Clean("/" + name)[1:]
	if name == "" || !fs.ValidPath(name) {
		writeDetail(w, http.StatusNotFound, "")
		return
	}
	data, err := fs.ReadFile(h.fsys, name)
	if err != nil {
		writeDetail(w, http.StatusNotFound, "")
		return
	}
	ctype, ok := staticTypes[strings.ToLower(path.Ext(name))]
	if !ok {
		ctype = "text/plain; charset=utf-8"
	}
	w.Header().Set("Content-Type", ctype)
	if etag, ok := h.etags[name]; ok {
		w.Header().Set("ETag", etag)
	}
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
}

// staticURLFunc returns the static_url() template helper: the versioned URL
// for an asset. Every template resolves assets through it, so no reference
// can be left unversioned by accident.
func staticURLFunc(version string) func(string) string {
	return func(p string) string {
		return "/static/" + strings.TrimLeft(p, "/") + "?v=" + version
	}
}
