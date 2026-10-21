// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package openrouter

// CTV text helpers. In the Python release these were static methods of
// CTVProcessor (_strip_markdown, _truncate_research, _detect_ctv_platform);
// they are pure functions over research text and bundle IDs, so they live
// here next to the research call and the pipeline (and input detection)
// can share one implementation.

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	mdHeaderRe     = regexp.MustCompile(`(?m)^#{1,6}[\t\n\v\f\r \x{85}\p{Z}\x1c-\x1f]+`)
	mdLinkRe       = regexp.MustCompile(`\[([^\]]+)\]\([^)]+\)`)
	mdCitationRe   = regexp.MustCompile(`\[\p{Nd}+\]`)
	mdBoldStarRe   = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	mdBoldUnderRe  = regexp.MustCompile(`__([^_]+)__`)
	mdCodeRe       = regexp.MustCompile("`([^`]+)`")
	mdNewlinesRe   = regexp.MustCompile(`\n{3,}`)
	mdLineIndentRe = regexp.MustCompile(`(?m)^[ \t]+`)
)

// stripSingle removes single-character emphasis markers: m + text + m
// becomes text when neither marker touches another m. It reproduces
// Python's re.sub(r"(?<!m)m([^m]+)m(?!m)", r"\1", s) (Go's regexp has no
// lookarounds).
func stripSingle(s string, m byte) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		if s[i] == m && (i == 0 || s[i-1] != m) {
			if j := strings.IndexByte(s[i+1:], m); j > 0 {
				end := i + 1 + j
				if end+1 >= len(s) || s[end+1] != m {
					b.WriteString(s[i+1 : end])
					i = end + 1
					continue
				}
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// StripMarkdown removes common Markdown from a research summary before it
// is stored in the Research_Summary CSV column: header markers, link syntax
// (keeping the text), citation markers like [1], bold/italic markers,
// inline-code backticks, runs of three or more newlines (collapsed to one
// blank line) and indentation at line starts.
func StripMarkdown(text string) string {
	s := mdHeaderRe.ReplaceAllString(text, "")
	s = mdLinkRe.ReplaceAllString(s, "${1}")
	s = mdCitationRe.ReplaceAllString(s, "")
	s = mdBoldStarRe.ReplaceAllString(s, "${1}")
	s = mdBoldUnderRe.ReplaceAllString(s, "${1}")
	s = stripSingle(s, '*')
	s = stripSingle(s, '_')
	s = mdCodeRe.ReplaceAllString(s, "${1}")
	s = mdNewlinesRe.ReplaceAllString(s, "\n\n")
	// [ \t] rather than \s, so the newlines survive.
	s = mdLineIndentRe.ReplaceAllString(s, "")
	return pyStrip(s)
}

func isPySpace(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) }

// DefaultResearchSummaryLength is the Research_Summary column's limit.
const DefaultResearchSummaryLength = 1000

// TruncateResearch shortens research for the CSV, preferring to end at the
// last sentence boundary (. ! ? followed by whitespace or the cut) when that
// keeps at least half of maxLength characters; otherwise it hard-cuts and
// appends "...". Lengths count characters, not bytes.
func TruncateResearch(content string, maxLength int) string {
	if utf8.RuneCountInString(content) <= maxLength {
		return content
	}
	runes := []rune(content)[:maxLength]
	cut := -1
	for k, r := range runes {
		if r == '.' || r == '!' || r == '?' {
			if k == len(runes)-1 || isPySpace(runes[k+1]) {
				cut = k + 1
			}
		}
	}
	if cut >= 0 && float64(cut) >= float64(maxLength)*0.5 {
		return pyStrip(string(runes[:cut]))
	}
	return pyStrip(string(runes)) + "..."
}

func allRunes(s string, ok func(rune) bool) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !ok(r) {
			return false
		}
	}
	return true
}

// DetectCTVPlatform guesses the CTV platform from a bundle ID's shape:
// all digits (54092) Roku; B0... Fire TV; com.* Android TV; vizio.* Vizio;
// tv.* Apple TV; G + digits Samsung TV; 9 + alphanumerics Xbox; *.roku Roku;
// xbox.* Xbox; playstation.* PlayStation; anything else "Unknown".
func DetectCTVPlatform(bundleID string) string {
	b := pyStrip(bundleID)
	if b == "" {
		return "Unknown"
	}
	lowered, upper := strings.ToLower(b), strings.ToUpper(b)
	switch {
	case allRunes(b, unicode.IsDigit):
		return "Roku"
	case strings.HasPrefix(upper, "B0"):
		return "Fire TV"
	case strings.HasPrefix(lowered, "com."):
		return "Android TV"
	case strings.HasPrefix(lowered, "vizio."):
		return "Vizio"
	case strings.HasPrefix(lowered, "tv."):
		return "Apple TV"
	case strings.HasPrefix(upper, "G") && allRunes(b[1:], unicode.IsDigit):
		return "Samsung TV"
	case strings.HasPrefix(b, "9") && allRunes(b, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) }):
		return "Xbox"
	case strings.HasSuffix(lowered, ".roku"):
		return "Roku"
	case strings.HasPrefix(lowered, "xbox."):
		return "Xbox"
	case strings.HasPrefix(lowered, "playstation."):
		return "PlayStation"
	}
	return "Unknown"
}
