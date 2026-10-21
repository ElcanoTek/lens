// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package openrouter

import (
	"regexp"
	"sort"
	"strings"
	"sync"

	lens "github.com/ElcanoTek/lens"
)

// taxonomy is the parsed IAB content taxonomy: every tier name and leaf
// name, normalised, mapped to its tier path. keys keeps insertion order,
// because the fuzzy matcher breaks length ties by first occurrence (Python
// iterated its dict in insertion order).
type taxonomy struct {
	paths map[string][]string
	keys  []string
	// prompt is the hierarchical category list embedded in every
	// classification prompt.
	prompt string
}

var (
	taxonomyOnce   sync.Once
	taxonomyParsed *taxonomy
)

// loadTaxonomy parses lens.ContentTaxonomy once per process. The file is
// embedded in the binary, so classification can never run without it.
func loadTaxonomy() *taxonomy {
	taxonomyOnce.Do(func() { taxonomyParsed = parseTaxonomy(lens.ContentTaxonomy) })
	return taxonomyParsed
}

// fallbackTaxonomyPrompt is what the Python release sent when the TSV was
// unreadable; kept for a malformed embedded file.
const fallbackTaxonomyPrompt = "Sports\nNews\nEntertainment\nTechnology & Computing\n"

// parseTaxonomy reads the TSV: a description line, a header line, then one
// row per category with Name and Tier 1..Tier 4 columns.
func parseTaxonomy(data []byte) *taxonomy {
	t := &taxonomy{paths: map[string][]string{}, prompt: fallbackTaxonomyPrompt}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) < 2 {
		log.Error("Failed to load taxonomy file: missing header")
		return t
	}
	header := strings.Split(strings.TrimSpace(lines[1]), "\t")
	col := map[string]int{}
	for i, name := range header {
		if _, dup := col[name]; !dup {
			col[name] = i
		}
	}
	get := func(fields []string, name string) string {
		i, ok := col[name]
		if !ok || i >= len(fields) {
			return ""
		}
		return strings.TrimSpace(fields[i])
	}

	groups := map[string]map[string][]string{}
	for _, line := range lines[2:] {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")

		// Lookup map: every tier and the leaf name resolve to their path.
		if name := get(fields, "Name"); name != "" {
			var tiers []string
			for _, column := range []string{"Tier 1", "Tier 2", "Tier 3", "Tier 4"} {
				if v := get(fields, column); v != "" && !containsString(tiers, v) {
					tiers = append(tiers, v)
				}
			}
			if !containsString(tiers, name) {
				tiers = append(tiers, name)
			}
			for i, tier := range tiers {
				key := normalizeTaxonomyLabel(tier)
				if key == "" {
					continue
				}
				path := tiers[:i+1]
				existing, ok := t.paths[key]
				if !ok {
					t.keys = append(t.keys, key)
				}
				if !ok || len(path) > len(existing) {
					t.paths[key] = append([]string(nil), path...)
				}
			}
		}

		// Prompt list: Tier 1 -> Tier 2 -> Tier 3.
		tier1, tier2, tier3 := get(fields, "Tier 1"), get(fields, "Tier 2"), get(fields, "Tier 3")
		if tier1 == "" {
			continue
		}
		if groups[tier1] == nil {
			groups[tier1] = map[string][]string{}
		}
		if tier2 != "" {
			list := groups[tier1][tier2]
			if list == nil {
				list = []string{}
			}
			if tier3 != "" && !containsString(list, tier3) {
				list = append(list, tier3)
			}
			groups[tier1][tier2] = list
		}
	}

	var out []string
	for _, tier1 := range sortedKeys(groups) {
		out = append(out, "• "+tier1)
		for _, tier2 := range sortedKeys(groups[tier1]) {
			out = append(out, "  - "+tier2)
			tier3s := append([]string(nil), groups[tier1][tier2]...)
			sort.Strings(tier3s)
			for _, tier3 := range tier3s {
				out = append(out, "    • "+tier3)
			}
		}
	}
	if len(out) > 0 {
		t.prompt = strings.Join(out, "\n")
	}
	return t
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func containsString(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

// TaxonomyPrompt returns the approved category list appended to every
// classification prompt.
func TaxonomyPrompt() string { return loadTaxonomy().prompt }

var (
	nonAlnumRe     = regexp.MustCompile(`[^a-z0-9]+`)
	spacesRe       = regexp.MustCompile(`\s+`)
	hierarchySepRe = regexp.MustCompile(`[>|/]+`)
	candidateSepRe = regexp.MustCompile(`[>|/,;]+`)
	parenSepRe     = regexp.MustCompile(`[()]`)
)

// normalizeTaxonomyLabel lowercases, spells & as "and" and reduces
// everything else to single spaces, so "Bars & Restaurants" matches
// "bars and restaurants".
func normalizeTaxonomyLabel(label string) string {
	n := strings.ReplaceAll(strings.ToLower(label), "&", "and")
	n = nonAlnumRe.ReplaceAllString(n, " ")
	n = spacesRe.ReplaceAllString(n, " ")
	return strings.TrimSpace(n)
}

// firstLine is Python's s.splitlines()[0].
func firstLine(s string) string {
	if i := strings.IndexFunc(s, func(r rune) bool {
		switch r {
		case '\n', '\r', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
			return true
		}
		return false
	}); i >= 0 {
		return s[:i]
	}
	return s
}

func splitNonEmpty(re *regexp.Regexp, s string) []string {
	var out []string
	for _, token := range re.Split(s, -1) {
		if token = strings.TrimSpace(token); token != "" {
			out = append(out, token)
		}
	}
	return out
}

func firstN(list []string, n int) []string {
	if len(list) > n {
		return list[:n]
	}
	return list
}

// VerticalTiers resolves a model-supplied vertical label into up to three
// IAB tiers: an explicit "A > B > C" hierarchy is taken as given; otherwise
// the label (and each of its comma/slash/parenthesis-separated parts) is
// matched against the taxonomy, exactly first and then by the longest
// containing/contained key; failing that the label's own parts are used.
func VerticalTiers(vertical string) []string {
	if vertical == "" {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(vertical)) {
	case "unknown", "n/a", "not provided":
		return nil
	}
	// Field names inside the vertical mean the model's output was garbled.
	lowered := strings.ToLower(vertical)
	for _, field := range []string{"description:", "confidence:", "quality:", "justification:"} {
		if strings.Contains(lowered, field) {
			log.Warn("Vertical field contains other field names: '" + vertical + "', returning empty")
			return nil
		}
	}

	tax := loadTaxonomy()
	first := strings.TrimSpace(firstLine(vertical))
	if first == "" {
		return nil
	}

	// A hierarchy the model spelled out is used directly.
	if strings.ContainsAny(first, ">|/") {
		if tiers := splitNonEmpty(hierarchySepRe, first); len(tiers) > 1 {
			return firstN(tiers, 3)
		}
	}

	candidates := []string{first}
	for _, re := range []*regexp.Regexp{candidateSepRe, parenSepRe} {
		for _, token := range re.Split(first, -1) {
			if token = strings.TrimSpace(token); token != "" && !containsString(candidates, token) {
				candidates = append(candidates, token)
			}
		}
	}

	for _, candidate := range candidates {
		if match := tax.match(candidate); len(match) > 0 {
			return firstN(append([]string(nil), match...), 3)
		}
	}
	return firstN(splitNonEmpty(candidateSepRe, first), 3)
}

func (t *taxonomy) match(label string) []string {
	normalized := normalizeTaxonomyLabel(label)
	if normalized == "" || len(t.keys) == 0 {
		return nil
	}
	if path, ok := t.paths[normalized]; ok {
		return path
	}
	best := ""
	for _, key := range t.keys {
		if strings.Contains(key, normalized) || strings.Contains(normalized, key) {
			if best == "" || len(key) > len(best) {
				best = key
			}
		}
	}
	if best != "" {
		return t.paths[best]
	}
	return nil
}
