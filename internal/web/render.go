// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package web

import (
	"bytes"
	"html/template"
	"io/fs"
	"net/http"
)

// loadTemplates parses the dashboard templates (Go html/template; they were
// Jinja2 in the Python release and render the same markup).
func loadTemplates(fsys fs.FS, staticURL func(string) string) (*template.Template, error) {
	funcs := template.FuncMap{
		"static_url": staticURL,
		// html/template drops HTML comments from template text; the few the
		// page has always shipped (the SPDX header, section markers) are
		// emitted through this instead. Only ever called with literals.
		"comment": func(text string) template.HTML {
			return template.HTML("<!-- " + template.HTMLEscapeString(text) + " -->") //nolint:gosec // escaped literal from our own templates
		},
		"inc": func(i int) int { return i + 1 },
	}
	return template.New("").Funcs(funcs).ParseFS(fsys, "templates/*.html")
}

// render executes a template and writes it as an HTML page.
func (s *Server) render(w http.ResponseWriter, name string, data any) {
	var buf bytes.Buffer
	if err := s.templates.ExecuteTemplate(&buf, name, data); err != nil {
		logger.Error("template " + name + " failed: " + err.Error())
		writeDetail(w, http.StatusInternalServerError, "")
		return
	}
	// Jinja dropped the template's single trailing newline
	// (keep_trailing_newline=False); keep the bytes identical.
	out := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}
