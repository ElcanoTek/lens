// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package categories implements optional, evidence-based custom categories:
// per-run definitions validated here and answered by an OpenRouter decision
// model (TypeSafe's Jev by default) through the Decisions API.
//
// A definition set (a [Spec]) is shared by the CLI, persisted dashboard jobs
// and the dashboard form, and is snapshotted in _jobs.json, the progress file
// and the <output>.categories.json sidecar. Its JSON shape is therefore part
// of the on-disk format and identical to the Python release:
//
//	{"categories": [{"name": ..., "type": "boolean"|"choice", "question": ..., "options": [...]}]}
//
// with "options" present only for choice categories.
package categories

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ElcanoTek/lens/internal/categories/pyjson"
	"github.com/ElcanoTek/lens/internal/pystr"
)

// Limits shared with the dashboard form and documented in
// docs/CUSTOM_CATEGORIES.md.
const (
	MaxSpecBytes    = 32_768
	MaxCategories   = 12
	MaxContentChars = 24_000
)

// Category types.
const (
	TypeBoolean = "boolean"
	TypeChoice  = "choice"
)

// Category is one custom question.
type Category struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	Question string   `json:"question"`
	Options  []string `json:"options,omitempty"`
}

// Spec is a validated, normalised set of custom categories. Build one with
// [Validate], [Parse] or [Load]; a Spec decoded straight from JSON (for
// example out of _jobs.json) should be passed through [Validate] before use.
type Spec struct {
	Categories []Category `json:"categories"`
}

// Equal reports whether two specs define the same categories. It is the Go
// counterpart of Python's dict comparison used on resume ("Custom categories
// changed..."): nil equals only nil.
func Equal(a, b *Spec) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if len(a.Categories) != len(b.Categories) {
		return false
	}
	for i := range a.Categories {
		x, y := a.Categories[i], b.Categories[i]
		if x.Name != y.Name || x.Type != y.Type || x.Question != y.Question ||
			len(x.Options) != len(y.Options) || (x.Options == nil) != (y.Options == nil) {
			return false
		}
		for j := range x.Options {
			if x.Options[j] != y.Options[j] {
				return false
			}
		}
	}
	return true
}

// object is the spec as an ordered JSON object, in Python's key order.
func (s *Spec) object() pyjson.Object {
	cats := make([]any, 0, len(s.Categories))
	for _, c := range s.Categories {
		entry := pyjson.Object{
			{Key: "name", Value: c.Name},
			{Key: "type", Value: c.Type},
			{Key: "question", Value: c.Question},
		}
		if c.Options != nil {
			options := make([]any, len(c.Options))
			for i, o := range c.Options {
				options[i] = o
			}
			entry = append(entry, pyjson.Member{Key: "options", Value: options})
		}
		cats = append(cats, entry)
	}
	return pyjson.Object{{Key: "categories", Value: cats}}
}

// PythonJSON returns the spec exactly as Python's json.dumps(spec,
// indent=indent) wrote it (indent <= 0 means the compact one-line form).
// The <output>.categories.json sidecar was written with indent=2.
func (s *Spec) PythonJSON(indent int) string {
	return pyjson.Encode(s.object(), pyjson.Options{EnsureASCII: true, Indent: indent})
}

// ValidationError is returned for every definition problem; its message is
// shown to users verbatim (CLI, dashboard, API).
type ValidationError struct{ msg string }

func (e *ValidationError) Error() string { return e.msg }

func invalid(msg string) error { return &ValidationError{msg: msg} }

var (
	nameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9 _-]{0,47}$`)
	errBig = invalid("Custom category definitions are too large.")
)

// pyIsSpace matches Python's str.isspace, which also counts the ASCII
// information separators U+001C..U+001F.
func pyIsSpace(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) }

func pyStrip(s string) string { return strings.TrimFunc(s, pyIsSpace) }

// foldKey is Python's str.casefold, which the uniqueness checks use: full
// folding, so "ẞ"/"ss" or "ﬀ"/"ff" count as the same label.
func foldKey(s string) string { return pystr.CaseFold(s) }

// Validate returns the normalised spec for a decoded JSON value (the result
// of json.Unmarshal into any, or a pyjson value). A *Spec or Spec is
// re-validated. Error messages match the Python release.
func Validate(value any) (*Spec, error) {
	switch v := value.(type) {
	case *Spec:
		if v == nil {
			return nil, invalid(`Custom categories must be an object containing "categories".`)
		}
		return Validate(pyjson.Plain(v.object()))
	case Spec:
		return Validate(&v)
	case pyjson.Object:
		return Validate(pyjson.Plain(v))
	}
	doc, ok := value.(map[string]any)
	if _, has := doc["categories"]; !ok || len(doc) != 1 || !has {
		return nil, invalid(`Custom categories must be an object containing "categories".`)
	}
	list, ok := doc["categories"].([]any)
	if !ok || len(list) < 1 || len(list) > MaxCategories {
		return nil, invalid(fmt.Sprintf("Define between 1 and %d custom categories.", MaxCategories))
	}
	spec := &Spec{Categories: make([]Category, 0, len(list))}
	names := map[string]bool{}
	for _, raw := range list {
		entry, ok := raw.(map[string]any)
		if ok {
			for key := range entry {
				switch key {
				case "name", "type", "question", "options":
				default:
					ok = false
				}
			}
		}
		if !ok {
			return nil, invalid("Each category needs name, type, question, and optional options.")
		}
		name, ok := entry["name"].(string)
		if !ok || !nameRe.MatchString(name) {
			return nil, invalid("Category names must start with a letter and use up to 48 letters, digits, spaces, _ or -.")
		}
		name = pyStrip(name)
		if names[foldKey(name)] {
			return nil, invalid("Category names must be unique.")
		}
		names[foldKey(name)] = true
		question, ok := entry["question"].(string)
		if n := utf8.RuneCountInString(pyStrip(question)); !ok || n < 1 || n > 1000 {
			return nil, invalid("Each category needs a question of 1–1000 characters.")
		}
		kind, _ := entry["type"].(string)
		_, isString := entry["type"].(string)
		cat := Category{Name: name, Type: kind, Question: pyStrip(question)}
		_, hasOptions := entry["options"]
		switch {
		case isString && kind == TypeChoice:
			options, err := validateOptions(entry["options"])
			if err != nil {
				return nil, err
			}
			cat.Options = options
		case !isString || kind != TypeBoolean || hasOptions:
			return nil, invalid("Category type must be boolean or choice; only choices have options.")
		}
		spec.Categories = append(spec.Categories, cat)
	}
	// Python measured len(json.dumps(result).encode()).
	if len(spec.PythonJSON(0)) > MaxSpecBytes {
		return nil, errBig
	}
	return spec, nil
}

func validateOptions(raw any) ([]string, error) {
	list, ok := raw.([]any)
	if !ok || len(list) < 2 || len(list) > 12 {
		return nil, invalid("Choice categories need 2–12 options (include Other or Unknown when useful).")
	}
	options := make([]string, 0, len(list))
	for _, item := range list {
		s, ok := item.(string)
		if n := utf8.RuneCountInString(pyStrip(s)); !ok || n < 1 || n > 80 {
			return nil, invalid("Choice options must be 1–80 characters each.")
		}
		options = append(options, pyStrip(s))
	}
	seen := map[string]bool{}
	for _, o := range options {
		seen[foldKey(o)] = true
	}
	if len(seen) != len(options) {
		return nil, invalid("Choice options must be unique.")
	}
	// Labels are exported to spreadsheets; never emit a formula as a label.
	for _, o := range options {
		if strings.ContainsAny(o[:1], "=+-@") || strings.IndexFunc(o, func(r rune) bool { return r < 32 }) >= 0 {
			return nil, invalid("Choice options cannot start with =, +, -, @ or contain control characters.")
		}
	}
	return options, nil
}

// Parse validates a JSON document (CLI file, dashboard form, API body).
func Parse(text string) (*Spec, error) {
	if len(text) > MaxSpecBytes {
		return nil, errBig
	}
	var value any
	if err := json.Unmarshal([]byte(text), &value); err != nil {
		return nil, invalid("Custom categories must be valid JSON.")
	}
	return Validate(value)
}

// Load reads and validates a definitions file. At most MaxSpecBytes+1 bytes
// are read, so an oversized file is rejected without reading all of it.
func Load(path string) (*Spec, error) {
	f, err := os.Open(path) //nolint:gosec // operator-chosen definitions file
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, MaxSpecBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) <= MaxSpecBytes && !utf8.Valid(data) {
		return nil, errors.New("custom categories file is not valid UTF-8")
	}
	return Parse(string(data))
}

// Columns returns the CSV columns a spec adds, in output order.
//
// The label and one probability per choice come first, so a spreadsheet
// filter can select a person or sort by how likely they are. Status and the
// raw JSON follow, instead of sitting in front of the answer.
func Columns(spec *Spec) []string {
	if spec == nil {
		return nil
	}
	var columns []string
	for _, c := range spec.Categories {
		columns = append(columns, "Custom: "+c.Name, "P(yes/choice): "+c.Name)
		if c.Type == TypeChoice {
			columns = append(columns, "Confidence: "+c.Name)
			for _, o := range c.Options {
				columns = append(columns, "P: "+c.Name+" / "+o)
			}
		}
	}
	return append(columns,
		"Decision_Status",
		"Decision_Error",
		"Decision_Model",
		"Decision_Provider",
		"Decision_Answers",
	)
}

// LegacyColumnNames maps the output columns' old names (from when TypeSafe
// was the only backend) to the current ones. Resumed runs rename them so
// older rows keep their values.
var LegacyColumnNames = map[string]string{
	"TypeSafe_Status":  "Decision_Status",
	"TypeSafe_Error":   "Decision_Error",
	"TypeSafe_Model":   "Decision_Model",
	"TypeSafe_Answers": "Decision_Answers",
}

// Only Jev itself: typesafe/jev-router is a chat router, not a decision model.
var jevRe = regexp.MustCompile(`^~?typesafe/(jev-(?:latest|[0-9][A-Za-z0-9._-]*))$`)

// TypesafeModelFor returns TypeSafe's own name for an OpenRouter Jev model
// ID ("~typesafe/jev-latest" -> "jev-latest"), and false for other models.
func TypesafeModelFor(model string) (string, bool) {
	m := jevRe.FindStringSubmatch(model)
	if m == nil {
		return "", false
	}
	return m[1], true
}
