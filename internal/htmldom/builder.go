// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package htmldom

import (
	"math/big"
	"strconv"
	"strings"
	"unicode/utf8"
)

// builder turns parser events into the tree, the way bs4's html.parser tree
// builder (and the Python html_dom module) did.
type builder struct {
	root       *Node
	stack      []*Node
	containers []*Node // open string containers (script, style …)
	preserving []*Node // open pre/textarea
	text       strings.Builder
	// hasText is separate from text.Len(): Python kept a list of pieces, and
	// a list holding only "" still flushes (as " ", by the whitespace rule).
	hasText bool
	open    map[string]int // open elements by name, for end tags
	// closedVoids counts void elements closed on their start tag, by name: a
	// matching end tag later is redundant and swallowed (once per such
	// element). A counter, not a list, so each end tag is O(1).
	closedVoids map[string]int
}

func newBuilder() *builder {
	root := &Node{Type: ElementNode, Name: RootName}
	return &builder{
		root:        root,
		stack:       []*Node{root},
		open:        map[string]int{},
		closedVoids: map[string]int{},
	}
}

func (b *builder) top() *Node { return b.stack[len(b.stack)-1] }

func (b *builder) data(s string) {
	b.text.WriteString(s)
	b.hasText = true
}

func (b *builder) flush(kind Kind) {
	if !b.hasText {
		return
	}
	data := b.text.String()
	b.text.Reset()
	b.hasText = false
	if len(b.preserving) == 0 && strings.Trim(data, "\x20\x0a\x09\x0c\x0d") == "" {
		if strings.Contains(data, "\n") {
			data = "\n"
		} else {
			data = " "
		}
	}
	if kind == KindText && len(b.containers) > 0 {
		kind = Kind(b.containers[len(b.containers)-1].Name)
	}
	b.top().appendChild(&Node{Type: TextNode, Data: data, Kind: kind})
}

// special records a comment, declaration, CDATA section or processing
// instruction as its own string.
func (b *builder) special(data string, kind Kind) {
	b.flush(KindText)
	b.data(data)
	b.flush(kind)
}

func (b *builder) decl(decl string) {
	b.special(skipRunes(decl, len("DOCTYPE ")), KindDoctype)
}

func (b *builder) unknownDecl(data string) {
	if asciiHasPrefixFold(data, 0, "CDATA[") {
		b.special(data[len("CDATA["):], KindCDATA)
		return
	}
	b.special(data, KindDeclaration)
}

func (b *builder) charref(name string) {
	base := 10
	if name[0] == 'x' || name[0] == 'X' {
		name, base = name[1:], 16
	}
	b.data(numericReference(name, base))
}

func (b *builder) entityref(name string) {
	if v, ok := html5Entities[name]; ok {
		b.data(v)
		return
	}
	if v, ok := html5Entities[name+";"]; ok {
		b.data(v)
		return
	}
	b.data("&" + name)
}

func (b *builder) startTag(tag string, attrs []Attr, closesVoid bool) {
	b.flush(KindText)
	el := &Node{Type: ElementNode, Name: tag}
	for _, a := range attrs {
		replaced := false
		for i := range el.Attrs {
			if el.Attrs[i].Key == a.Key { // a repeat wins, in the first one's place
				el.Attrs[i].Val = a.Val
				replaced = true
				break
			}
		}
		if !replaced {
			el.Attrs = append(el.Attrs, a)
		}
	}
	b.top().appendChild(el)
	b.stack = append(b.stack, el)
	b.open[tag]++
	if stringContainers[tag] {
		b.containers = append(b.containers, el)
	}
	if preserveWhitespace[tag] {
		b.preserving = append(b.preserving, el)
	}
	if closesVoid && voidElements[tag] {
		b.endTag(tag, false)
		b.closedVoids[tag]++
	}
}

func (b *builder) endTag(tag string, checkAlreadyClosed bool) {
	if checkAlreadyClosed && b.closedVoids[tag] > 0 {
		// bs4 swallows this end tag without ending the current string, so
		// "a<br>b</br>c" keeps "bc" as one string; kept for parity.
		b.closedVoids[tag]--
		return
	}
	b.flush(KindText)
	if b.open[tag] == 0 {
		return
	}
	for {
		if b.pop().Name == tag {
			return
		}
	}
}

func (b *builder) pop() *Node {
	el := b.top()
	b.stack = b.stack[:len(b.stack)-1]
	b.open[el.Name]--
	if n := len(b.containers); n > 0 && b.containers[n-1] == el {
		b.containers = b.containers[:n-1]
	}
	if n := len(b.preserving); n > 0 && b.preserving[n-1] == el {
		b.preserving = b.preserving[:n-1]
	}
	return el
}

func (b *builder) finish() *Node {
	b.flush(KindText)
	for len(b.stack) > 1 {
		b.pop()
	}
	return b.root
}

// windows1252 maps 0x80–0x9F to what Windows-1252 decodes them to; the five
// bytes it leaves undefined map to themselves.
var windows1252 = [32]rune{
	0x20ac, 0x81, 0x201a, 0x0192, 0x201e, 0x2026, 0x2020, 0x2021,
	0x02c6, 0x2030, 0x0160, 0x2039, 0x0152, 0x8d, 0x017d, 0x8f,
	0x90, 0x2018, 0x2019, 0x201c, 0x201d, 0x2022, 0x2013, 0x2014,
	0x02dc, 0x2122, 0x0161, 0x203a, 0x0153, 0x9d, 0x017e, 0x0178,
}

// parseCodepoint parses digits in base; anything past the Unicode range
// (however long) comes back as maxRune+1.
func parseCodepoint(digits string, base int) int64 {
	if v, err := strconv.ParseInt(digits, base, 64); err == nil && v <= utf8.MaxRune {
		return v
	}
	if _, ok := new(big.Int).SetString(digits, base); ok {
		return utf8.MaxRune + 1
	}
	return -1
}

// numericReference is how bs4 resolves &#…; in text: 0, surrogates and
// out-of-range become U+FFFD, and 0x80–0x9F go through Windows-1252.
func numericReference(digits string, base int) string {
	n := parseCodepoint(digits, base)
	switch {
	case n == 0 || n > utf8.MaxRune || (n >= 0xd800 && n <= 0xdfff):
		return "�"
	case n >= 0x80 && n <= 0x9f:
		return string(windows1252[n-0x80])
	case n < 0:
		return ""
	}
	return string(rune(n))
}

// invalidCodepoints are dropped by html.unescape (control characters and
// noncharacters), after the 0x80–0x9F and 0x0D special cases.
func invalidCodepoint(n int64) bool {
	switch {
	case n >= 0x1 && n <= 0x8, n >= 0xe && n <= 0x1f, n >= 0x7f && n <= 0x9f,
		n >= 0xfdd0 && n <= 0xfdef, n == 0xb:
		return true
	}
	return n&0xfffe == 0xfffe // U+xFFFE and U+xFFFF in every plane
}

// unescapeNumeric is html.unescape's rule for one numeric reference.
func unescapeNumeric(n int64) string {
	switch {
	case n == 0:
		return "�"
	case n == 0x0d:
		return "\r"
	case n >= 0x80 && n <= 0x9f:
		return string(windows1252[n-0x80])
	case (n >= 0xd800 && n <= 0xdfff) || n > utf8.MaxRune:
		return "�"
	case invalidCodepoint(n):
		return ""
	}
	return string(rune(n)) //nolint:gosec // n is within the Unicode range (checked above)
}

// unescape is Python's html.unescape, which html.parser applies to attribute
// values: numeric references, and named ones matched against the WHATWG
// list (longest legacy prefix when there is no exact match).
func unescape(s string) string {
	if !strings.Contains(s, "&") {
		return s
	}
	var b strings.Builder
	i := 0
	for {
		k := strings.IndexByte(s[i:], '&')
		if k < 0 {
			b.WriteString(s[i:])
			return b.String()
		}
		b.WriteString(s[i : i+k])
		i += k
		next, text, ok := unescapeAt(s, i)
		if !ok {
			b.WriteByte('&')
			i++
			continue
		}
		b.WriteString(text)
		i = next
	}
}

// unescapeAt matches html.unescape's charref regex,
// &(#[0-9]+;?|#[xX][0-9a-fA-F]+;?|[^\t\n\f <&#;]{1,32};?), at i.
func unescapeAt(s string, i int) (next int, text string, ok bool) {
	p := i + 1
	if p < len(s) && s[p] == '#' {
		q := p + 1
		base := 10
		if q < len(s) && (s[q] == 'x' || s[q] == 'X') {
			q++
			base = 16
		}
		e := q
		for e < len(s) && ((base == 10 && isDigit(s[e])) || (base == 16 && isHex(s[e]))) {
			e++
		}
		if e == q {
			return 0, "", false
		}
		text = unescapeNumeric(parseCodepoint(s[q:e], base))
		if e < len(s) && s[e] == ';' {
			e++
		}
		return e, text, true
	}
	// Up to 32 code points of anything but \t \n \f space < & # ;.
	e := p
	for count := 0; e < len(s) && count < 32; count++ {
		r, size := utf8.DecodeRuneInString(s[e:])
		if strings.ContainsRune("\t\n\f <&#;", r) {
			break
		}
		e += size
	}
	if e == p {
		return 0, "", false
	}
	if e < len(s) && s[e] == ';' {
		e++
	}
	name := s[p:e]
	if v, found := html5Entities[name]; found {
		return e, v, true
	}
	// The longest matching prefix, as the standard defines it (only the
	// legacy names without a semicolon can match here).
	for x := len(name) - 1; x > 1; x-- {
		if !utf8.RuneStart(name[x]) {
			continue
		}
		if v, found := html5Entities[name[:x]]; found {
			return e, v + name[x:], true
		}
	}
	return e, "&" + name, true
}
