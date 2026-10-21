// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package htmldom

import (
	"strings"
	"unicode/utf8"
)

// This file is a port of the tokenizer in CPython 3.12's html.parser
// (HTMLParser with convert_charrefs=False, which is how bs4's html.parser
// tree builder — and the Python html_dom module after it — drove it). Every
// regular expression of the original is replaced by a hand-written scanner
// that returns exactly what Python's backtracking engine would match; the
// comments name the regex each scanner stands for. Go strings are UTF-8
// bytes where Python indexes code points, but every delimiter the scanners
// look for is ASCII, so byte offsets land on the same boundaries.
//
// Parse runs goahead twice — once as feed() does (end=false) and once as
// close() does (end=true) — because a few constructs (a failed character
// reference, an incomplete entity) stop the first pass early and the second
// pass then re-reads the rest from that point. Running one end=true pass
// instead would turn everything after the first such construct into text.

// rawTextElements are parsed in RAWTEXT mode: only their own end tag ends
// them (CDATA_CONTENT_ELEMENTS).
var rawTextElements = map[string]bool{
	"script": true, "style": true, "xmp": true, "iframe": true, "noembed": true, "noframes": true,
}

// rcDataElements are RCDATA: their end tag and character references are
// recognised, nothing else (RCDATA_CONTENT_ELEMENTS).
var rcDataElements = map[string]bool{"textarea": true, "title": true}

const (
	// tagSpace is [\t\n\r\f ], the whitespace html.parser's tag regexes use.
	tagSpace = "\t\n\r\f "
	// tagNameStop ends a tag name: [^\t\n\r\f />].
	tagNameStop = "\t\n\r\f />"
	// attrNameStop ends an attribute name after its first character.
	attrNameStop = "\t\n\r\f /=>"
	// attrLookbehind is (?<=['"\t\n\r\f /]): what must precede an attribute.
	attrLookbehind = "'\"\t\n\r\f /"
	// bareValueStop ends an unquoted attribute value.
	bareValueStop = ">\t\n\r\f "
)

func in(c byte, set string) bool { return strings.IndexByte(set, c) >= 0 }

func isLetter(c byte) bool { return c|0x20 >= 'a' && c|0x20 <= 'z' }

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isHex(c byte) bool { return isDigit(c) || (c|0x20 >= 'a' && c|0x20 <= 'f') }

func isAlnum(c byte) bool { return isLetter(c) || isDigit(c) }

// asciiHasPrefixFold reports whether s[i:] starts with prefix, ignoring ASCII
// case only (re.IGNORECASE|re.ASCII, and str.lower() on an ASCII literal).
func asciiHasPrefixFold(s string, i int, prefix string) bool {
	if len(s)-i < len(prefix) {
		return false
	}
	for k := 0; k < len(prefix); k++ {
		a, b := s[i+k], prefix[k]
		if a >= 'A' && a <= 'Z' {
			a |= 0x20
		}
		if b >= 'A' && b <= 'Z' {
			b |= 0x20
		}
		if a != b {
			return false
		}
	}
	return true
}

// tagFind stands for tagfind_tolerant,
// ([a-zA-Z][^\t\n\r\f />]*)(?:[\t\n\r\f ]|/(?!>))*, matched at p (s[p] is a
// letter). It returns where the name ends and where the match ends.
func tagFind(s string, p int) (nameEnd, end int) {
	nameEnd = p + 1
	for nameEnd < len(s) && !in(s[nameEnd], tagNameStop) {
		nameEnd++
	}
	return nameEnd, skipSpaceOrSlash(s, nameEnd)
}

// skipSpaceOrSlash consumes (?:[\t\n\r\f ]|/(?!>))*.
func skipSpaceOrSlash(s string, p int) int {
	for p < len(s) {
		switch {
		case in(s[p], tagSpace):
			p++
		case s[p] == '/' && (p+1 >= len(s) || s[p+1] != '>'):
			p++
		default:
			return p
		}
	}
	return p
}

// matchValue stands for the optional value indicator group of both
// attrfind_tolerant and locatetagend, matched at p (just after an attribute
// name):
//
//	[\t\n\r\f ]*=[\t\n\r\f ]*('[^']*'|"[^"]*"|(?!['"])[^>\t\n\r\f ]*)
//
// The backtracking matters: when the first non-space after "=" is an
// unclosed quote, Python gives back the spaces it skipped and matches an
// empty bare value at the last of them. It returns the value's span.
func matchValue(s string, p int) (start, end int, ok bool) {
	w := p
	for w < len(s) && in(s[w], tagSpace) {
		w++
	}
	if w >= len(s) || s[w] != '=' {
		return 0, 0, false
	}
	after := w + 1
	m := after
	for m < len(s) && in(s[m], tagSpace) {
		m++
	}
	for pos := m; pos >= after; pos-- {
		if pos < len(s) && (s[pos] == '\'' || s[pos] == '"') {
			if closing := strings.IndexByte(s[pos+1:], s[pos]); closing >= 0 {
				return pos, pos + 1 + closing + 1, true
			}
			continue // the bare alternative refuses a leading quote
		}
		e := pos
		for e < len(s) && !in(s[e], bareValueStop) {
			e++
		}
		return pos, e, true
	}
	return 0, 0, false
}

// locateTagEnd stands for locatetagend matched at p (s[p] is a letter):
// the tag name, then attributes, then an optional ">". The caller checks
// whether the match ended in ">" to see if the tag is complete.
func locateTagEnd(s string, p int) int {
	q := p + 1
	for q < len(s) && !in(s[q], tagNameStop) {
		q++
	}
	for q < len(s) && in(s[q], "\t\n\r\f /") {
		q++
	}
	for q < len(s) && in(s[q-1], attrLookbehind) && !in(s[q], tagNameStop) {
		e := q + 1
		for e < len(s) && !in(s[e], attrNameStop) {
			e++
		}
		if _, valueEnd, ok := matchValue(s, e); ok {
			e = valueEnd
		}
		for e < len(s) && in(s[e], "\t\n\r\f /") {
			e++
		}
		q = e
	}
	if q < len(s) && s[q] == '>' {
		q++
	}
	return q
}

// attrFind stands for attrfind_tolerant matched at k. hasValue is false when
// the attribute had no "=" at all (Python's attrvalue None).
func attrFind(s string, k int) (name, value string, hasValue bool, end int, ok bool) {
	if k <= 0 || k >= len(s) || !in(s[k-1], attrLookbehind) || in(s[k], tagNameStop) {
		return "", "", false, k, false
	}
	nameEnd := k + 1
	for nameEnd < len(s) && !in(s[nameEnd], attrNameStop) {
		nameEnd++
	}
	end = nameEnd
	if vs, ve, matched := matchValue(s, nameEnd); matched {
		value, hasValue, end = s[vs:ve], true, ve
	}
	return s[k:nameEnd], value, hasValue, skipSpaceOrSlash(s, end), true
}

// matchCharref stands for charref, &#(?:[0-9]+|[xX][0-9a-fA-F]+)[^0-9a-fA-F],
// matched at i. name is the reference without "&#" and the terminator; next
// is where parsing resumes (after a ";" terminator, else at it).
func matchCharref(s string, i int) (name string, next int, ok bool) {
	p := i + 2
	if p >= len(s) {
		return "", 0, false
	}
	var e int
	switch {
	case isDigit(s[p]):
		e = p + 1
		for e < len(s) && isDigit(s[e]) {
			e++
		}
	case (s[p] == 'x' || s[p] == 'X') && p+1 < len(s) && isHex(s[p+1]):
		e = p + 2
		for e < len(s) && isHex(s[e]) {
			e++
		}
	default:
		return "", 0, false
	}
	// The terminator must be a non-hex character. Giving back digits never
	// helps (they are hex), so a hex letter or the end of input is a miss.
	if e >= len(s) || isHex(s[e]) {
		return "", 0, false
	}
	if s[e] == ';' {
		return s[p:e], e + 1, true
	}
	return s[p:e], e, true
}

// matchEntityref stands for entityref, &([a-zA-Z][-.a-zA-Z0-9]*)[^a-zA-Z0-9],
// matched at i.
func matchEntityref(s string, i int) (name string, next int, ok bool) {
	p := i + 1
	if p >= len(s) || !isLetter(s[p]) {
		return "", 0, false
	}
	e := p + 1
	for e < len(s) && (isAlnum(s[e]) || s[e] == '-' || s[e] == '.') {
		e++
	}
	if e >= len(s) {
		// At the end of input Python backtracks into the name to use its
		// last "-" or "." as the terminator.
		e = strings.LastIndexAny(s[p+1:], "-.")
		if e < 0 {
			return "", 0, false
		}
		e += p + 1
	}
	if s[e] == ';' {
		return s[p:e], e + 1, true
	}
	return s[p:e], e, true
}

// commentClose stands for commentclose.search, --!?>, from p.
func commentClose(s string, p int) (start, end int, ok bool) {
	for {
		k := strings.Index(s[p:], "--")
		if k < 0 {
			return 0, 0, false
		}
		k += p
		if k+2 < len(s) && s[k+2] == '>' {
			return k, k + 3, true
		}
		if k+3 < len(s) && s[k+2] == '!' && s[k+3] == '>' {
			return k, k + 4, true
		}
		p = k + 1
	}
}

// parser is HTMLParser's state plus the tree builder's handlers.
type parser struct {
	raw       string
	pos       int
	cdataElem string // lowercase name of the open RAWTEXT/RCDATA element
	escapable bool   // RCDATA: character references are still recognised
	tree      *builder
}

// searchCdata stands for the "interesting" regex in RAWTEXT/RCDATA mode,
// </name(?=[\t\n\r\f />]) (with "&|" in front for RCDATA), searched from i.
func (p *parser) searchCdata(i int) int {
	s := p.raw
	stops := "<"
	if p.escapable {
		stops = "&<"
	}
	for {
		k := strings.IndexAny(s[i:], stops)
		if k < 0 {
			return -1
		}
		k += i
		if s[k] == '&' {
			return k
		}
		after := k + 2 + len(p.cdataElem)
		if k+1 < len(s) && s[k+1] == '/' && asciiHasPrefixFold(s, k+2, p.cdataElem) &&
			after < len(s) && in(s[after], tagNameStop) {
			return k
		}
		i = k + 1
	}
}

func (p *parser) setCdataMode(elem string, escapable bool) {
	p.cdataElem = elem
	p.escapable = escapable
}

func (p *parser) clearCdataMode() {
	p.cdataElem = ""
	p.escapable = true
}

// goahead handles data as far as reasonable; with end it treats the input
// as complete.
func (p *parser) goahead(end bool) {
	s := p.raw
	n := len(s)
	i := p.pos
	t := p.tree
loop:
	for i < n {
		var j int
		switch p.cdataElem {
		case "plaintext":
			j = n // interesting is \Z: everything left is text
		case "":
			k := strings.IndexAny(s[i:], "&<")
			if k < 0 {
				j = n
			} else {
				j = i + k
			}
		default:
			j = p.searchCdata(i)
			if j < 0 {
				break loop
			}
		}
		if i < j {
			t.data(s[i:j])
		}
		i = j
		if i == n {
			break
		}
		switch {
		case s[i] == '<':
			var k int
			switch {
			case i+1 < n && isLetter(s[i+1]):
				k = p.parseStartTag(i)
			case strings.HasPrefix(s[i:], "</"):
				k = p.parseEndTag(i)
			case strings.HasPrefix(s[i:], "<!--"):
				k = p.parseComment(i)
			case strings.HasPrefix(s[i:], "<?"):
				k = p.parsePI(i)
			case strings.HasPrefix(s[i:], "<!"):
				k = p.parseHTMLDeclaration(i)
			case i+1 < n || end:
				t.data("<")
				k = i + 1
			default:
				break loop
			}
			if k < 0 {
				if !end {
					break loop
				}
				p.unterminated(i)
				k = n
			}
			i = k
		case strings.HasPrefix(s[i:], "&#"):
			if name, next, ok := matchCharref(s, i); ok {
				t.charref(name)
				i = next
				continue
			}
			if strings.IndexByte(s[i:], ';') >= 0 { // bail by consuming &#
				t.data("&#")
				i += 2
			}
			break loop
		default: // '&'
			if name, next, ok := matchEntityref(s, i); ok {
				t.entityref(name)
				i = next
				continue
			}
			if i+1 < n && (isLetter(s[i+1]) || s[i+1] == '#') { // incomplete
				if end && n-i == 2 {
					i++
				}
				break loop
			}
			if i+1 < n {
				t.data("&")
				i++
				continue
			}
			break loop
		}
	}
	if end && i < n {
		t.data(s[i:n])
		i = n
	}
	p.pos = i
}

// unterminated handles a construct at i that never closed, at end of input.
func (p *parser) unterminated(i int) {
	s := p.raw
	n := len(s)
	t := p.tree
	switch {
	case i+1 < n && isLetter(s[i+1]):
		// An unterminated start tag swallows the rest of the input.
	case strings.HasPrefix(s[i:], "</"):
		switch {
		case i+2 == n:
			t.data("</")
		case isLetter(s[i+2]):
		default:
			t.special(s[i+2:], KindComment) // bogus comment
		}
	case strings.HasPrefix(s[i:], "<!--"):
		j := n
		for _, suffix := range []string{"--!", "--", "-"} {
			if n-(i+4) >= len(suffix) && strings.HasSuffix(s, suffix) {
				j -= len(suffix)
				break
			}
		}
		t.special(s[i+4:j], KindComment)
	case strings.HasPrefix(s[i:], "<![CDATA["):
		t.unknownDecl(s[i+3:])
	case asciiHasPrefixFold(s, i, "<!doctype"):
		t.decl(s[i+2:])
	case strings.HasPrefix(s[i:], "<!"):
		t.special(s[i+2:], KindComment)
	case strings.HasPrefix(s[i:], "<?"):
		t.special(s[i+2:], KindPI)
	}
}

func (p *parser) parseHTMLDeclaration(i int) int {
	s := p.raw
	switch {
	case strings.HasPrefix(s[i:], "<!--"):
		return p.parseComment(i)
	case strings.HasPrefix(s[i:], "<![CDATA["):
		j := strings.Index(s[i+9:], "]]>")
		if j < 0 {
			return -1
		}
		j += i + 9
		p.tree.unknownDecl(s[i+3 : j])
		return j + 3
	case asciiHasPrefixFold(s, i, "<!doctype"):
		gt := strings.IndexByte(s[i+9:], '>')
		if gt < 0 {
			return -1
		}
		gt += i + 9
		p.tree.decl(s[i+2 : gt])
		return gt + 1
	case strings.HasPrefix(s[i:], "<!["):
		j := strings.IndexByte(s[i+3:], '>')
		if j < 0 {
			return -1
		}
		j += i + 3
		if s[j-1] == ']' {
			p.tree.unknownDecl(s[i+3 : j-1])
		} else {
			p.tree.special(s[i+2:j], KindComment)
		}
		return j + 1
	default:
		return p.parseBogusComment(i)
	}
}

func (p *parser) parseComment(i int) int {
	s := p.raw
	start, end, ok := commentClose(s, i+4)
	if !ok {
		// commentabruptclose, -?>, matched right after "<!--".
		switch {
		case i+4 < len(s) && s[i+4] == '>':
			start, end = i+4, i+5
		case i+5 < len(s) && s[i+4] == '-' && s[i+5] == '>':
			start, end = i+4, i+6
		default:
			return -1
		}
	}
	p.tree.special(s[i+4:start], KindComment)
	return end
}

func (p *parser) parseBogusComment(i int) int {
	s := p.raw
	pos := strings.IndexByte(s[i+2:], '>')
	if pos < 0 {
		return -1
	}
	pos += i + 2
	p.tree.special(s[i+2:pos], KindComment)
	return pos + 1
}

func (p *parser) parsePI(i int) int {
	s := p.raw
	j := strings.IndexByte(s[i+2:], '>')
	if j < 0 {
		return -1
	}
	j += i + 2
	p.tree.special(s[i+2:j], KindPI)
	return j + 1
}

func (p *parser) parseStartTag(i int) int {
	s := p.raw
	endpos := locateTagEnd(s, i+1)
	if s[endpos-1] != '>' {
		return -1
	}
	nameEnd, k := tagFind(s, i+1)
	tag := strings.ToLower(s[i+1 : nameEnd])
	var attrs []Attr
	for k < endpos {
		name, value, hasValue, next, ok := attrFind(s, k)
		if !ok {
			break
		}
		if hasValue {
			if len(value) >= 2 && (value[0] == '\'' || value[0] == '"') && value[len(value)-1] == value[0] {
				value = value[1 : len(value)-1]
			}
			if value != "" {
				value = unescape(value)
			}
		}
		attrs = append(attrs, Attr{Key: strings.ToLower(name), Val: value})
		k = next
	}
	rest := ""
	if k < endpos {
		rest = TrimSpace(s[k:endpos])
	}
	if rest != ">" && rest != "/>" {
		p.tree.data(s[i:endpos])
		return endpos
	}
	if strings.HasSuffix(rest, "/>") {
		// XHTML-style empty tag: <span attr="value" />
		p.tree.startTag(tag, attrs, false)
		p.tree.endTag(tag, false)
		return endpos
	}
	p.tree.startTag(tag, attrs, true)
	switch {
	case rawTextElements[tag] || tag == "plaintext":
		p.setCdataMode(tag, false)
	case rcDataElements[tag]:
		p.setCdataMode(tag, true)
	}
	return endpos
}

func (p *parser) parseEndTag(i int) int {
	s := p.raw
	if strings.IndexByte(s[i+2:], '>') < 0 {
		return -1
	}
	if !isLetter(s[i+2]) {
		if s[i+2] == '>' { // "</>" is ignored
			return i + 3
		}
		return p.parseBogusComment(i)
	}
	j := locateTagEnd(s, i+2)
	if s[j-1] != '>' {
		return -1
	}
	nameEnd, _ := tagFind(s, i+2)
	p.tree.endTag(strings.ToLower(s[i+2:nameEnd]), true)
	p.clearCdataMode()
	return j
}

// skipRunes returns s without its first n code points (Python's s[n:]).
func skipRunes(s string, n int) string {
	for ; n > 0 && s != ""; n-- {
		_, size := utf8.DecodeRuneInString(s)
		s = s[size:]
	}
	return s
}
