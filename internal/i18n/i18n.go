// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package i18n owns Lens presentation language, never stored or API identifiers.
package i18n

import (
	"embed"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

//go:embed locales/*.json
var localeFS embed.FS
var languageCatalogs = loadLanguageCatalogs()
var messageSlot = regexp.MustCompile(`\{([0-9]+)\}`)

func loadLanguageCatalogs() map[string]map[string]string {
	catalogs := map[string]map[string]string{"en": {}}
	for _, lang := range []string{"es", "pt"} {
		raw, err := localeFS.ReadFile("locales/" + lang + ".json")
		if err != nil {
			panic(err)
		}
		var catalog map[string]string
		if err := json.Unmarshal(raw, &catalog); err != nil {
			panic(err)
		}
		catalogs[lang] = catalog
	}
	return catalogs
}

func Normalize(raw string) string {
	switch lang := strings.SplitN(strings.ToLower(strings.TrimSpace(raw)), "-", 2)[0]; lang {
	case "en":
		return "en"
	case "es":
		return "es"
	case "pt":
		return "pt"
	default:
		return ""
	}
}

func T(lang, source string, values ...any) string {
	message := source
	if translated, ok := languageCatalogs[lang][source]; ok {
		message = translated
	}
	return messageSlot.ReplaceAllStringFunc(message, func(slot string) string {
		index, _ := strconv.Atoi(slot[1 : len(slot)-1])
		if index >= len(values) {
			return slot
		}
		return Number(values[index], lang)
	})
}

func RequestLanguage(r *http.Request) string {
	if lang := Normalize(r.URL.Query().Get("lang")); lang != "" {
		return lang
	}
	// OIDC UI preference is display-only: never rewrite redirect_uri, state,
	// authorization codes or the relying application's callback parameters.
	for _, value := range strings.Fields(r.URL.Query().Get("ui_locales")) {
		if lang := Normalize(value); lang != "" {
			return lang
		}
	}
	if saved, err := r.Cookie("elcano_language"); err == nil {
		if lang := Normalize(saved.Value); lang != "" {
			return lang
		}
	}
	type preference struct {
		lang    string
		quality float64
	}
	var preferences []preference
	for _, value := range strings.Split(r.Header.Get("Accept-Language"), ",") {
		parts := strings.Split(strings.TrimSpace(value), ";")
		lang := Normalize(parts[0])
		quality := 1.0
		for _, param := range parts[1:] {
			if q, ok := strings.CutPrefix(strings.TrimSpace(param), "q="); ok {
				if parsed, err := strconv.ParseFloat(q, 64); err == nil {
					quality = parsed
				} else {
					quality = 0
				}
			}
		}
		if lang != "" && quality > 0 && quality <= 1 {
			preferences = append(preferences, preference{lang, quality})
		}
	}
	sort.SliceStable(preferences, func(i, j int) bool { return preferences[i].quality > preferences[j].quality })
	if len(preferences) != 0 {
		return preferences[0].lang
	}
	return "en"
}

// Counts are presentation values; opaque strings such as codes remain untouched.
func Number(value any, language string) string {
	switch value.(type) {
	case int, int64, int32, uint, uint64, uint32:
	default:
		return fmt.Sprint(value)
	}
	digits := fmt.Sprint(value)
	sign := ""
	if strings.HasPrefix(digits, "-") {
		sign, digits = "-", digits[1:]
	}
	if len(digits) <= 3 || ((language == "es" || language == "pt") && len(digits) == 4) {
		return sign + digits
	}
	separator := ","
	if language == "es" {
		separator = "."
	}
	if language == "pt" {
		separator = "\u00a0"
	}
	var groups []string
	for len(digits) > 3 {
		groups = append([]string{digits[len(digits)-3:]}, groups...)
		digits = digits[:len(digits)-3]
	}
	return sign + strings.Join(append([]string{digits}, groups...), separator)
}

// CatalogJSON is escaped by html/template when used in a metadata attribute.
func CatalogJSON(language string) string {
	encoded, _ := json.Marshal(languageCatalogs[language])
	return string(encoded)
}

type languageWriter struct {
	http.ResponseWriter
	language string
}

func (w *languageWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Language returns the presentation language selected by Middleware.
func Language(w http.ResponseWriter) string {
	if localized, ok := w.(*languageWriter); ok {
		return localized.language
	}
	return "en"
}

// Middleware keeps a host-only preference cookie for server-rendered pages and
// requests. The outer trusted-proxy middleware resolves the public scheme;
// supported HTTP deployments must also retain this non-credential preference.
// It does not change session cookies, redirect validation or CSRF.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		language := RequestLanguage(r)
		if Normalize(r.URL.Query().Get("lang")) != "" && !strings.HasPrefix(r.URL.Path, "/static/") {
			http.SetCookie(w, &http.Cookie{Name: "elcano_language", Value: language, Path: "/", MaxAge: 31536000, Secure: r.TLS != nil || r.URL.Scheme == "https", HttpOnly: true, SameSite: http.SameSiteLaxMode})
		}
		next.ServeHTTP(&languageWriter{ResponseWriter: w, language: language}, r)
	})
}

// URL adds a display preference to an already constructed navigation URL.
// Authentication state and redirect targets remain byte-for-byte query values.
func URL(raw, language, parameter string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	query := parsed.Query()
	query.Set(parameter, language)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

// Message translates a diagnostic from an owned component at the UI boundary.
// Only catalogued printf messages match; captured identifiers remain untouched.
// Programmatic API payloads and persisted diagnostics keep their original form.
func Message(language, source string) string {
	if target, ok := languageCatalogs[language][source]; ok {
		return target
	}
	if language == "en" {
		return source
	}
	for _, pattern := range diagnosticPatterns() {
		captures := pattern.expression.FindStringSubmatch(source)
		if captures == nil {
			continue
		}
		target, ok := languageCatalogs[language][pattern.source]
		if !ok {
			continue
		}
		index := 0
		return printfSlot.ReplaceAllStringFunc(target, func(_ string) string {
			index++
			if index < len(captures) {
				return captures[index]
			}
			return ""
		})
	}
	return source
}

type diagnosticPattern struct {
	source     string
	expression *regexp.Regexp
}

var printfSlot = regexp.MustCompile(`%(?:\.[0-9]+)?[sdqvf]`)
var diagnosticPatterns = sync.OnceValue(func() []diagnosticPattern {
	var keys []string
	for key := range languageCatalogs["es"] {
		if printfSlot.MatchString(key) {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	out := make([]diagnosticPattern, 0, len(keys))
	for _, key := range keys {
		var expression strings.Builder
		expression.WriteString("(?s)^")
		end := 0
		for _, match := range printfSlot.FindAllStringIndex(key, -1) {
			expression.WriteString(regexp.QuoteMeta(key[end:match[0]]))
			switch key[match[1]-1] {
			case 'q':
				expression.WriteString(`("(?:\\.|[^"\\])*")`)
			case 'd':
				expression.WriteString(`(-?[0-9]+)`)
			default:
				expression.WriteString(`(.+?)`)
			}
			end = match[1]
		}
		expression.WriteString(regexp.QuoteMeta(key[end:]) + "$")
		out = append(out, diagnosticPattern{key, regexp.MustCompile(expression.String())})
	}
	return out
})
