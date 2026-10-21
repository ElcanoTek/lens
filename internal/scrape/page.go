// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package scrape

import (
	"fmt"
	"mime"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/htmlindex"

	"github.com/ElcanoTek/lens/internal/htmldom"
)

// Page is what classification reads from a fetched document.
type Page struct {
	Title           string
	MetaDescription string
	// Content is the visible text, space-joined and truncated to
	// MaxContentChars characters (plus "...").
	Content string
	URL     string
}

// TruncateContent cuts text to MaxContentChars characters plus "...", the
// budget every scrape backend shares.
func TruncateContent(text string) string {
	if utf8.RuneCountInString(text) <= MaxContentChars {
		return text
	}
	n := 0
	for i := range text {
		if n == MaxContentChars {
			return text[:i] + "..."
		}
		n++
	}
	return text
}

// ParseHTML extracts the title, meta description and visible text of a
// page, after dropping script, style and noscript subtrees.
func ParseHTML(pageURL, markup string) Page {
	doc := htmldom.Parse(markup)
	for _, el := range doc.FindAllAny([]string{"script", "style", "noscript"}) {
		el.Decompose()
	}
	title := ""
	if t := doc.Title(); t != nil {
		if s, ok := t.StringValue(); ok && s != "" {
			title = htmldom.TrimSpace(s)
		}
	}
	meta := ""
	if tag := doc.Find("meta", htmldom.AttrEquals("name", "description")); tag != nil {
		if content := tag.Attr("content"); content != "" {
			meta = htmldom.TrimSpace(content)
		}
	}
	return Page{
		Title:           title,
		MetaDescription: meta,
		Content:         TruncateContent(doc.GetText(" ", true)),
		URL:             pageURL,
	}
}

// ISOTime formats t like Python's datetime.isoformat() on a naive local
// time: microseconds only when non-zero. Output CSVs and progress files have
// always carried timestamps in this form.
func ISOTime(t time.Time) string {
	s := t.Format("2006-01-02T15:04:05")
	if us := t.Nanosecond() / 1000; us != 0 {
		s += fmt.Sprintf(".%06d", us)
	}
	return s
}

// windows1252High decodes bytes 0x80–0x9F; 0 marks the five bytes the
// codec leaves undefined (dropped, as errors="ignore" dropped them).
var windows1252High = [32]rune{
	0x20ac, 0, 0x201a, 0x0192, 0x201e, 0x2026, 0x2020, 0x2021,
	0x02c6, 0x2030, 0x0160, 0x2039, 0x0152, 0, 0x017d, 0,
	0, 0x2018, 0x2019, 0x201c, 0x201d, 0x2022, 0x2013, 0x2014,
	0x02dc, 0x2122, 0x0161, 0x203a, 0x0153, 0, 0x017e, 0x0178,
}

// DecodeBody turns a response body into text using the Content-Type
// charset, dropping undecodable bytes (Python's errors="ignore"). UTF-8 is
// the default and the fallback for charsets this build does not know;
// Latin-1, Windows-1252 and ASCII are decoded exactly.
func DecodeBody(raw []byte, contentType string) string {
	charset := ""
	if _, params, err := mime.ParseMediaType(contentType); err == nil {
		charset = strings.ToLower(strings.TrimSpace(params["charset"]))
	}
	charset = strings.ReplaceAll(charset, "_", "-")
	switch charset {
	case "iso-8859-1", "iso8859-1", "latin-1", "latin1", "l1", "cp819", "iso-ir-100", "8859":
		var b strings.Builder
		b.Grow(len(raw))
		for _, c := range raw {
			b.WriteRune(rune(c))
		}
		return b.String()
	case "windows-1252", "cp1252", "x-cp1252":
		var b strings.Builder
		b.Grow(len(raw))
		for _, c := range raw {
			switch {
			case c < 0x80 || c > 0x9f:
				b.WriteRune(rune(c))
			case windows1252High[c-0x80] != 0:
				b.WriteRune(windows1252High[c-0x80])
			}
		}
		return b.String()
	case "ascii", "us-ascii", "646":
		var b strings.Builder
		b.Grow(len(raw))
		for _, c := range raw {
			if c < 0x80 {
				b.WriteByte(c)
			}
		}
		return b.String()
	}
	if charset != "" && charset != "utf-8" && charset != "utf8" {
		// Every other declared charset (Shift_JIS, GBK, EUC-KR, KOI8-R...)
		// goes through the WHATWG encoding registry, as a browser would.
		// Undecodable bytes are dropped, like errors="ignore".
		if enc, err := htmlindex.Get(charset); err == nil {
			if decoded, err := enc.NewDecoder().Bytes(raw); err == nil {
				return strings.ToValidUTF8(strings.ReplaceAll(string(decoded), "\uFFFD", ""), "")
			}
		}
	}
	return strings.ToValidUTF8(string(raw), "")
}
