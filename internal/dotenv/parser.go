// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
//
// The parser in this file is a port of python-dotenv 1.2.3
// (https://github.com/theskumar/python-dotenv, src/dotenv/parser.py),
// BSD-3-Clause; see LICENSE in this directory for its copyright and terms.

package dotenv

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Original is the exact source text a binding was parsed from and the
// (1-based) line it starts on.
type Original struct {
	String string
	Line   int
}

// Binding is one statement of a .env file. Key is nil for blank lines,
// comments and statements that failed to parse (Error set); Value is nil for
// a bare key with no "=".
type Binding struct {
	Key      *string
	Value    *string
	Original Original
	Error    bool
}

// position mirrors python-dotenv's Position: a byte offset plus the line it
// is on. Lines are counted per consumed chunk, with "\r\n" as one newline,
// exactly like Position.advance — including its quirk that a "\r" and "\n"
// consumed by two different reads count as two lines.
type position struct {
	chars int
	line  int
}

func (p *position) advance(chunk string) {
	p.chars += len(chunk)
	for i := 0; i < len(chunk); i++ {
		switch chunk[i] {
		case '\n':
			p.line++
		case '\r':
			p.line++
			if i+1 < len(chunk) && chunk[i+1] == '\n' {
				i++
			}
		}
	}
}

// errParse is python-dotenv's parser.Error: a statement does not match the
// grammar. It never escapes the package; the statement becomes an error
// binding and parsing resumes on the next line.
type errParse struct{}

func (errParse) Error() string { return "dotenv: pattern not found" }

type reader struct {
	s    string
	pos  position
	mark position
}

func newReader(s string) *reader {
	s = strings.TrimPrefix(s, "\ufeff")
	return &reader{s: s, pos: position{line: 1}, mark: position{line: 1}}
}

func (r *reader) hasNext() bool { return r.pos.chars < len(r.s) }

func (r *reader) setMark() { r.mark = r.pos }

func (r *reader) marked() Original {
	return Original{String: r.s[r.mark.chars:r.pos.chars], Line: r.mark.line}
}

// rest is the unread input.
func (r *reader) rest() string { return r.s[r.pos.chars:] }

// peek returns the next rune as a string ("" at the end), like peek(1).
func (r *reader) peek() string {
	rest := r.rest()
	if rest == "" {
		return ""
	}
	_, n := utf8.DecodeRuneInString(rest)
	return rest[:n]
}

// consume advances past the next n bytes and returns them.
func (r *reader) consume(n int) string {
	chunk := r.s[r.pos.chars : r.pos.chars+n]
	r.pos.advance(chunk)
	return chunk
}

// isSpace is Python's `\s` / str.isspace() for str patterns: Go's
// unicode.IsSpace plus the ASCII separators U+001C..U+001F, which Python
// counts as whitespace (bidi class B/S) and Go does not.
func isSpace(c rune) bool {
	return unicode.IsSpace(c) || (c >= 0x1c && c <= 0x1f)
}

// isInlineSpace is `[^\S\r\n]`: whitespace that does not end a line.
func isInlineSpace(c rune) bool { return c != '\r' && c != '\n' && isSpace(c) }

// spanWhile returns the byte length of the longest prefix of s whose runes
// all satisfy f.
func spanWhile(s string, f func(rune) bool) int {
	for i, c := range s {
		if !f(c) {
			return i
		}
	}
	return len(s)
}

// newlineLen is the length of the line break at the start of s matched as
// `\r\n|\n|\r` (0 when there is none).
func newlineLen(s string) int {
	switch {
	case strings.HasPrefix(s, "\r\n"):
		return 2
	case strings.HasPrefix(s, "\n"), strings.HasPrefix(s, "\r"):
		return 1
	}
	return 0
}

// readMultilineWhitespace is `\s*`.
func (r *reader) readMultilineWhitespace() { r.consume(spanWhile(r.rest(), isSpace)) }

// readWhitespace is `[^\S\r\n]*`.
func (r *reader) readWhitespace() { r.consume(spanWhile(r.rest(), isInlineSpace)) }

// readExport is `(?:export[^\S\r\n]+)?`.
func (r *reader) readExport() {
	rest := r.rest()
	if !strings.HasPrefix(rest, "export") {
		return
	}
	if n := spanWhile(rest[len("export"):], isInlineSpace); n > 0 {
		r.consume(len("export") + n)
	}
}

// parseKey returns nil when the statement is a comment.
func (r *reader) parseKey() (*string, error) {
	rest := r.rest()
	switch r.peek() {
	case "#":
		return nil, nil
	case "'":
		// '([^']+)'
		end := strings.IndexByte(rest[1:], '\'')
		if end <= 0 {
			return nil, errParse{}
		}
		key := rest[1 : 1+end]
		r.consume(end + 2)
		return &key, nil
	default:
		// ([^=\#\s]+)
		n := spanWhile(rest, func(c rune) bool { return c != '=' && c != '#' && !isSpace(c) })
		if n == 0 {
			return nil, errParse{}
		}
		key := r.consume(n)
		return &key, nil
	}
}

// readQuoted is `Q((?:\\.|[^Q\\])*)Q` with DOTALL: a backslash always escapes
// the character after it (so an escaped backslash is never mistaken for the
// start of an escaped quote), and the first unescaped quote closes the value.
func (r *reader) readQuoted(quote byte) (string, error) {
	rest := r.rest()
	i := 1
	for i < len(rest) {
		switch rest[i] {
		case '\\':
			if i+1 >= len(rest) {
				return "", errParse{}
			}
			_, n := utf8.DecodeRuneInString(rest[i+1:])
			i += 1 + n
		case quote:
			r.consume(i + 1)
			return rest[1:i], nil
		default:
			i++
		}
	}
	return "", errParse{}
}

// decodeEscapes replaces, left to right and without overlap, every backslash
// pair whose second character is in set with what Python's unicode-escape
// codec makes of it. Other backslashes stay literal.
func decodeEscapes(s, set string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && strings.IndexByte(set, s[i+1]) >= 0 {
			b.WriteByte(escapeValue(s[i+1]))
			i++
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func escapeValue(c byte) byte {
	switch c {
	case 'a':
		return '\a'
	case 'b':
		return '\b'
	case 'f':
		return '\f'
	case 'n':
		return '\n'
	case 'r':
		return '\r'
	case 't':
		return '\t'
	case 'v':
		return '\v'
	}
	return c // \\ \' \"
}

const (
	singleQuoteEscapes = `\'`
	doubleQuoteEscapes = `\'"abfnrtv`
)

// stripInlineComment is re.sub(r"\s+#.*", "", part).rstrip(): an unquoted
// value ends at the first whitespace run that is followed by '#'. A '#' glued
// to the value (`a=b#c`) is part of it.
func stripInlineComment(part string) string {
	for i := 0; i < len(part); {
		c, n := utf8.DecodeRuneInString(part[i:])
		if !isSpace(c) {
			i += n
			continue
		}
		run := spanWhile(part[i:], isSpace)
		if i+run < len(part) && part[i+run] == '#' {
			part = part[:i]
			break
		}
		i += run
	}
	return strings.TrimRightFunc(part, isSpace)
}

func (r *reader) parseValue() (string, error) {
	switch r.peek() {
	case "'":
		v, err := r.readQuoted('\'')
		if err != nil {
			return "", err
		}
		return decodeEscapes(v, singleQuoteEscapes), nil
	case `"`:
		v, err := r.readQuoted('"')
		if err != nil {
			return "", err
		}
		return decodeEscapes(v, doubleQuoteEscapes), nil
	case "", "\n", "\r":
		return "", nil
	default:
		// ([^\r\n]*)
		part := r.consume(strings.IndexFunc(r.rest()+"\n", func(c rune) bool { return c == '\r' || c == '\n' }))
		return stripInlineComment(part), nil
	}
}

// readComment is `(?:[^\S\r\n]*#[^\r\n]*)?`.
func (r *reader) readComment() {
	rest := r.rest()
	ws := spanWhile(rest, isInlineSpace)
	if ws < len(rest) && rest[ws] == '#' {
		r.consume(ws + strings.IndexAny(rest[ws:]+"\n", "\r\n"))
	}
}

// readEndOfLine is `[^\S\r\n]*(?:\r\n|\n|\r|$)`.
func (r *reader) readEndOfLine() error {
	rest := r.rest()
	ws := spanWhile(rest, isInlineSpace)
	nl := newlineLen(rest[ws:])
	if nl == 0 && ws < len(rest) {
		return errParse{}
	}
	r.consume(ws + nl)
	return nil
}

// readRestOfLine is `[^\r\n]*(?:\r|\n|\r\n)?`. The alternation tries "\r"
// first, so a CRLF after a bad statement leaves its "\n" for the next one.
func (r *reader) readRestOfLine() {
	rest := r.rest()
	n := strings.IndexAny(rest+"\n", "\r\n")
	if n < len(rest) {
		n++
	}
	r.consume(n)
}

func (r *reader) parseStatement() (key, value *string, err error) {
	r.readExport()
	if key, err = r.parseKey(); err != nil {
		return nil, nil, err
	}
	r.readWhitespace()
	if r.peek() == "=" {
		r.consume(1)
		r.readWhitespace()
		v, err := r.parseValue()
		if err != nil {
			return nil, nil, err
		}
		value = &v
	}
	r.readComment()
	if err := r.readEndOfLine(); err != nil {
		return nil, nil, err
	}
	return key, value, nil
}

func (r *reader) parseBinding() Binding {
	r.setMark()
	r.readMultilineWhitespace()
	if !r.hasNext() {
		return Binding{Original: r.marked()}
	}
	key, value, err := r.parseStatement()
	if err != nil {
		// Like upstream, the reader keeps whatever the failed statement
		// consumed and skips to the end of the line from there.
		r.readRestOfLine()
		return Binding{Original: r.marked(), Error: true}
	}
	return Binding{Key: key, Value: value, Original: r.marked()}
}

// ParseBindings splits s into its statements, python-dotenv's
// parse_stream. A leading UTF-8 BOM is dropped.
func ParseBindings(s string) []Binding {
	r := newReader(s)
	var out []Binding
	for r.hasNext() {
		before := r.pos.chars
		out = append(out, r.parseBinding())
		if r.pos.chars == before { // cannot happen; guards against a hang
			r.consume(len(r.rest()))
		}
	}
	return out
}
