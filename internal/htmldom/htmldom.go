// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package htmldom is a small HTML tree: find / find-all by tag name and
// attribute, the page title, the text under an element, the next sibling
// element, and removing subtrees. It is what the scrapers parse with, and it
// does exactly that much.
//
// What it reproduces, because the extractors' output depends on it, is the
// tree BeautifulSoup 4.15.0 builds with its html.parser backend (which Lens
// always used); the golden fixtures in testdata/ were recorded from it:
//
//   - No implicit closing. An end tag closes the most recent open element of
//     that name and everything opened inside it; an end tag with no open
//     element of that name is ignored. Void elements (br, img, meta …) close
//     at once, and a later stray </br> for one of them is swallowed.
//   - Character references are resolved like bs4's, not like html.unescape:
//     an unknown &name; stays &name (semicolon dropped), and numeric
//     references follow the HTML5 rules (0, surrogates and out-of-range
//     become U+FFFD; 0x80–0x9F map through Windows-1252).
//   - Whitespace-only text is collapsed to a single "\n" (if it held a
//     newline) or " ", except inside <pre> and <textarea>.
//   - Text kinds. Strings inside script, style, template, rt and rp belong
//     to that container, and comments, CDATA, doctypes, declarations and
//     processing instructions are their own kinds. Text on an ordinary
//     element or the document returns only plain text and CDATA; on one of
//     the containers it returns that container's own strings.
//
// The tokenizer (parser.go) is a port of CPython's html.parser, which
// differs from the HTML5 tokenizer in golang.org/x/net/html in many corner
// cases; that is why it is not used.
//
// Attribute matching is deliberately simpler than bs4's: values are plain
// strings, so class is not split into a token list. No caller matches on
// class; add token matching here first if one ever needs to.
//
// Every walk is iterative and linear in the nodes it visits: response bodies
// are up to 5 MiB, and hostile markup nests deeper than any recursion limit.
package htmldom

import (
	"iter"
	"regexp"
	"strings"
	"unicode"
)

// NodeType tells elements from strings.
type NodeType uint8

const (
	// ElementNode is a tag (or the document root).
	ElementNode NodeType = iota
	// TextNode is a string of some Kind.
	TextNode
)

// Kind says what sort of string a text node is. String containers (script,
// style, template, rt, rp) use their tag name as the kind.
type Kind string

// The string kinds that are not container names.
const (
	KindText        Kind = "text"
	KindCDATA       Kind = "cdata"
	KindComment     Kind = "comment"
	KindDoctype     Kind = "doctype"
	KindDeclaration Kind = "declaration"
	KindPI          Kind = "pi"
)

// RootName is the document root's element name, as bs4 called it.
const RootName = "[document]"

var stringContainers = map[string]bool{"rt": true, "rp": true, "style": true, "script": true, "template": true}

var preserveWhitespace = map[string]bool{"pre": true, "textarea": true}

var voidElements = map[string]bool{
	"area": true, "base": true, "br": true, "col": true, "embed": true, "hr": true, "img": true,
	"input": true, "keygen": true, "link": true, "menuitem": true, "meta": true, "param": true,
	"source": true, "track": true, "wbr": true,
	// Pre-HTML5 void elements.
	"basefont": true, "bgsound": true, "command": true, "frame": true, "image": true,
	"isindex": true, "nextid": true, "spacer": true,
}

// Attr is one attribute. Values are always plain strings; a valueless
// attribute has Val "".
type Attr struct {
	Key, Val string
}

// Node is an element or a string. Children form a doubly linked list, so
// appending, removing and stepping to a sibling are all O(1) whatever the
// page's shape.
type Node struct {
	Type NodeType
	// Name is the lowercase tag name (elements only).
	Name string
	// Attrs keeps the first spelling's position and the last spelling's value
	// when an attribute repeats.
	Attrs []Attr
	// Data is the string (text nodes only).
	Data string
	// Kind is the string kind (text nodes only).
	Kind Kind

	Parent, FirstChild, LastChild, PrevSibling, NextSibling *Node

	childCount int
}

// IsElement reports whether n is an element.
func (n *Node) IsElement() bool { return n.Type == ElementNode }

// Lookup returns the attribute's value and whether it is present.
func (n *Node) Lookup(key string) (string, bool) {
	for _, a := range n.Attrs {
		if a.Key == key {
			return a.Val, true
		}
	}
	return "", false
}

// Attr returns the attribute's value, or "" when it is absent.
func (n *Node) Attr(key string) string {
	v, _ := n.Lookup(key)
	return v
}

func (n *Node) appendChild(c *Node) {
	c.Parent = n
	c.PrevSibling = n.LastChild
	c.NextSibling = nil
	if n.LastChild == nil {
		n.FirstChild = c
	} else {
		n.LastChild.NextSibling = c
	}
	n.LastChild = c
	n.childCount++
}

// Children returns the direct children, in order.
func (n *Node) Children() []*Node {
	out := make([]*Node, 0, n.childCount)
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		out = append(out, c)
	}
	return out
}

// Descendants yields every node below n in document order, by walking the
// links (no recursion, no stack).
func (n *Node) Descendants() iter.Seq[*Node] {
	return func(yield func(*Node) bool) {
		node := n.FirstChild
		for node != nil {
			if !yield(node) {
				return
			}
			if node.FirstChild != nil {
				node = node.FirstChild
				continue
			}
			for node != n && node.NextSibling == nil {
				node = node.Parent
			}
			if node == n {
				return
			}
			node = node.NextSibling
		}
	}
}

// Filter is an attribute condition for Find and FindAll.
type Filter func(*Node) bool

// AttrEquals matches elements whose attribute key is exactly val.
func AttrEquals(key, val string) Filter {
	return func(n *Node) bool {
		v, ok := n.Lookup(key)
		return ok && v == val
	}
}

// AttrPresent matches elements that carry the attribute key at all.
func AttrPresent(key string) Filter {
	return func(n *Node) bool {
		_, ok := n.Lookup(key)
		return ok
	}
}

// AttrMatches matches elements whose attribute key contains a match of re.
func AttrMatches(key string, re *regexp.Regexp) Filter {
	return func(n *Node) bool {
		v, ok := n.Lookup(key)
		return ok && re.MatchString(v)
	}
}

func (n *Node) matches(names []string, filters []Filter) iter.Seq[*Node] {
	return func(yield func(*Node) bool) {
		for node := range n.Descendants() {
			if node.Type != ElementNode {
				continue
			}
			if len(names) > 0 && !nameIn(node.Name, names) {
				continue
			}
			ok := true
			for _, f := range filters {
				if !f(node) {
					ok = false
					break
				}
			}
			if ok && !yield(node) {
				return
			}
		}
	}
}

func nameIn(name string, names []string) bool {
	for _, want := range names {
		if want == name {
			return true
		}
	}
	return false
}

func namesOf(name string) []string {
	if name == "" {
		return nil
	}
	return []string{name}
}

// Find returns the first descendant element named name ("" for any name)
// that passes every filter, or nil.
func (n *Node) Find(name string, filters ...Filter) *Node {
	return n.FindAny(namesOf(name), filters...)
}

// FindAny is Find over several names (none means any name).
func (n *Node) FindAny(names []string, filters ...Filter) *Node {
	for node := range n.matches(names, filters) {
		return node
	}
	return nil
}

// FindAll returns every descendant element named name ("" for any name)
// that passes every filter, in document order.
func (n *Node) FindAll(name string, filters ...Filter) []*Node {
	return n.FindAllAny(namesOf(name), filters...)
}

// FindAllAny is FindAll over several names (none means any name).
func (n *Node) FindAllAny(names []string, filters ...Filter) []*Node {
	var out []*Node
	for node := range n.matches(names, filters) {
		out = append(out, node)
	}
	return out
}

// NextElementSibling returns the next sibling that is an element, skipping
// strings (bs4's find_next_sibling()).
func (n *Node) NextElementSibling() *Node {
	node := n.NextSibling
	for node != nil && node.Type != ElementNode {
		node = node.NextSibling
	}
	return node
}

// Title returns the first <title> element, or nil.
func (n *Node) Title() *Node { return n.Find("title") }

func (n *Node) textKinds() (Kind, Kind) {
	if stringContainers[n.Name] {
		k := Kind(n.Name)
		return k, k
	}
	return KindText, KindCDATA
}

// Text returns every string under n concatenated (bs4's get_text()).
func (n *Node) Text() string { return n.GetText("", false) }

// GetText joins the strings under n with separator; with strip each string
// is stripped of whitespace (Python's str.strip) and empty ones are dropped.
func (n *Node) GetText(separator string, strip bool) string {
	k1, k2 := n.textKinds()
	var b strings.Builder
	first := true
	for node := range n.Descendants() {
		if node.Type != TextNode || (node.Kind != k1 && node.Kind != k2) {
			continue
		}
		value := node.Data
		if strip {
			value = TrimSpace(value)
			if value == "" {
				continue
			}
		}
		if !first {
			b.WriteString(separator)
		}
		first = false
		b.WriteString(value)
	}
	return b.String()
}

// StringValue returns the only string under n, following single-child elements
// down (bs4's .string); ok is false when there are zero or several children.
func (n *Node) StringValue() (value string, ok bool) {
	node := n
	for node.Type == ElementNode {
		if node.childCount != 1 {
			return "", false
		}
		node = node.FirstChild
	}
	return node.Data, true
}

// Decompose removes n and everything inside it from the tree, in O(1): it
// is unlinked from its siblings and its subtree goes with it.
func (n *Node) Decompose() {
	parent := n.Parent
	if parent == nil {
		return
	}
	if n.PrevSibling == nil {
		parent.FirstChild = n.NextSibling
	} else {
		n.PrevSibling.NextSibling = n.NextSibling
	}
	if n.NextSibling == nil {
		parent.LastChild = n.PrevSibling
	} else {
		n.NextSibling.PrevSibling = n.PrevSibling
	}
	parent.childCount--
	n.Parent, n.PrevSibling, n.NextSibling = nil, nil, nil
}

// String returns markup for n. It is approximate (attribute order and
// quoting follow the parse, entities are minimally re-escaped); callers only
// search it for substrings. One pass over the subtree's links.
func (n *Node) String() string {
	var b strings.Builder
	if n.Type == TextNode {
		writeText(&b, n)
		return b.String()
	}
	if n.Name != RootName {
		writeOpenTag(&b, n)
	}
	node := n.FirstChild
	for node != nil {
		if node.Type == TextNode {
			writeText(&b, node)
		} else {
			writeOpenTag(&b, node)
		}
		if node.Type == ElementNode && node.FirstChild != nil {
			node = node.FirstChild
			continue
		}
		for {
			if node.Type == ElementNode && !selfClosing(node) {
				b.WriteString("</" + node.Name + ">")
			}
			if node.NextSibling != nil {
				node = node.NextSibling
				break
			}
			node = node.Parent
			if node == n {
				node = nil
				break
			}
		}
	}
	if n.Name != RootName && !selfClosing(n) {
		b.WriteString("</" + n.Name + ">")
	}
	return b.String()
}

func selfClosing(n *Node) bool { return n.FirstChild == nil && voidElements[n.Name] }

var (
	textEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	quoteEscape = strings.NewReplacer(`"`, "&quot;")
)

func writeOpenTag(b *strings.Builder, n *Node) {
	b.WriteString("<" + n.Name)
	for _, a := range n.Attrs {
		b.WriteString(" " + a.Key + "=")
		v := textEscaper.Replace(a.Val)
		switch {
		case strings.Contains(v, `"`) && strings.Contains(v, "'"):
			b.WriteString(`"` + quoteEscape.Replace(v) + `"`)
		case strings.Contains(v, `"`):
			b.WriteString("'" + v + "'")
		default:
			b.WriteString(`"` + v + `"`)
		}
	}
	if selfClosing(n) {
		b.WriteString("/")
	}
	b.WriteString(">")
}

func writeText(b *strings.Builder, n *Node) {
	switch n.Kind {
	case KindText:
		b.WriteString(textEscaper.Replace(n.Data))
	case KindCDATA:
		b.WriteString("<![CDATA[" + n.Data + "]]>")
	case KindComment:
		b.WriteString("<!--" + n.Data + "-->")
	case KindDoctype:
		b.WriteString("<!DOCTYPE " + n.Data + ">")
	case KindDeclaration:
		b.WriteString("<!" + n.Data + ">")
	case KindPI:
		b.WriteString("<?" + n.Data + ">")
	default: // script/style/template/rt/rp contents are not escaped
		b.WriteString(n.Data)
	}
}

// IsSpace reports whether r is whitespace by Python's str.isspace(), which
// adds the ASCII information separators 0x1C–0x1F to Unicode White_Space.
func IsSpace(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) }

// TrimSpace is Python's str.strip(): it removes leading and trailing
// whitespace as IsSpace defines it.
func TrimSpace(s string) string { return strings.TrimFunc(s, IsSpace) }

// Parse parses markup into a tree whose root element is the document
// (named RootName).
func Parse(markup string) *Node {
	tree := newBuilder()
	p := &parser{raw: markup, escapable: true, tree: tree}
	// feed() then close(): see the comment at the top of parser.go.
	p.goahead(false)
	p.goahead(true)
	return tree.finish()
}
