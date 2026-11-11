// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package web

import (
	"bytes"
	"html/template"
	"io/fs"
	"net/http"
	"strings"

	"github.com/ElcanoTek/lens/internal/i18n"
)

// loadTemplates parses the dashboard templates (Go html/template; they were
// Jinja2 in the Python release and render the same markup).
func loadTemplates(fsys fs.FS, staticURL func(string) string, languages ...string) (*template.Template, error) {
	language := "en"
	if len(languages) != 0 {
		language = languages[0]
	}
	funcs := template.FuncMap{
		"tr":          func(source string, values ...any) string { return i18n.T(language, source, values...) },
		"language":    func() string { return language },
		"catalog":     func() string { return i18n.CatalogJSON(language) },
		"number":      func(value any) string { return i18n.Number(value, language) },
		"languageURL": func(raw string) string { return i18n.URL(raw, language, "lang") },
		"static_url":  staticURL,
		"modelLabel":  func(raw string) string { return displayModelLabel(raw, language) },
		// html/template drops HTML comments from template text; the few the
		// page has always shipped (the SPDX header, section markers) are
		// emitted through this instead. Only ever called with literals.
		"comment": func(text string) template.HTML {
			return template.HTML("<!-- " + template.HTMLEscapeString(text) + " -->") //nolint:gosec // escaped literal from our own templates
		},
		"inc": func(i int) int { return i + 1 },
		// html/template escapes "+" as "&#43;" everywhere; Jinja (markupsafe)
		// did not. These two emit a server-generated value with exactly
		// markupsafe's escaping (& < > " '), so timestamps ("+00:00") and
		// the cost cell ("$0.012+") keep their bytes. Quotes are escaped,
		// so the value can never leave its attribute or text node.
		"jattr": func(name, value string) template.HTMLAttr {
			return template.HTMLAttr(name + `="` + markupEscape(value) + `"`) //nolint:gosec // value escaped; name is a template literal
		},
		"jtext": func(value string) template.HTML {
			return template.HTML(markupEscape(value)) //nolint:gosec // escaped like markupsafe
		},
	}
	return template.New("").Funcs(funcs).ParseFS(fsys, "templates/*.html")
}

var markupReplacer = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&#34;", "'", "&#39;")

// markupEscape is markupsafe.escape.
func markupEscape(s string) string { return markupReplacer.Replace(s) }

// render executes a template and writes it as an HTML page.
func (s *Server) render(w http.ResponseWriter, name string, data any) {
	var buf bytes.Buffer
	if err := s.templates[i18n.Language(w)].ExecuteTemplate(&buf, name, data); err != nil {
		logger.Error("template " + name + " failed: " + err.Error())
		writeDetail(w, http.StatusInternalServerError, "")
		return
	}
	// Jinja dropped the template's single trailing newline
	// (keep_trailing_newline=False); keep the bytes identical.
	out := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Language", i18n.Language(w))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}
