// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package htmldom

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Expected values were taken from BeautifulSoup 4.15.0 (html.parser) on the
// same markup before the dependency was removed; the extractor-level goldens
// are in golden_test.go.

func elementNames(nodes []*Node) []string {
	var out []string
	for _, n := range nodes {
		if n.IsElement() {
			out = append(out, n.Name)
		}
	}
	return out
}

func TestCharacterReferencesResolveLikeBs4(t *testing.T) {
	cases := []struct{ markup, want string }{
		{"a &amp; b", "a & b"},
		{"&copy 2026", "© 2026"},             // legacy entity without a semicolon
		{"&foo; &notit; x", "&foo &notit x"}, // unknown: kept, semicolon dropped
		{"&#150;&#x41;&#65;", "–AA"},         // 0x80-0x9F go through Windows-1252
		{"&#0;", "�"},
		{"&#xD800;", "�"},
		{"&#x110000;", "�"},
		{"&#x81;", "\u0081"}, // undefined in Windows-1252: kept as-is
		{"&#12a;", "&#12a;"},
		{"&#99999999999999999999999;", "�"},
	}
	for _, c := range cases {
		if got := Parse("<p>" + c.markup + "</p>").Text(); got != c.want {
			t.Errorf("%q: got %q, want %q", c.markup, got, c.want)
		}
	}
}

func TestAttributeValuesUseHTMLUnescape(t *testing.T) {
	doc := Parse(`<a title="x &amp; y &#65; &copyz &notin; &#x80;&#1;&bogus;">`)
	if got, want := doc.Find("a").Attr("title"), "x & y A ©z ∉ €&bogus;"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestWhitespaceOnlyStringsCollapseOutsidePreAndTextarea(t *testing.T) {
	doc := Parse("<div>a</div> \t <div>b</div>\n \n<pre>  \n </pre><textarea>   </textarea>")
	if got, want := doc.Text(), "a b\n  \n    "; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestGetTextSkipsScriptStyleTemplateRubyAndComments(t *testing.T) {
	doc := Parse("<title>T</title><style>s{}</style><script>var a</script><template>tp</template>" +
		"<!--c--><![CDATA[cd]]><?pi?><!DOCTYPE html><rt>r</rt><rp>(</rp><p>body</p>")
	if got := doc.Text(); got != "Tcdbody" {
		t.Errorf("document text %q", got)
	}
	if got := doc.Find("script").Text(); got != "var a" {
		t.Errorf("script text %q", got)
	}
	if got := doc.Find("rt").Text(); got != "r" {
		t.Errorf("rt text %q", got)
	}
}

func TestGetTextStripAndSeparator(t *testing.T) {
	div := Parse("<div>  one <b> two </b>\n<i></i> three </div>").Find("div")
	if got := div.GetText(" ", true); got != "one two three" {
		t.Errorf("got %q", got)
	}
	if got := div.GetText("", true); got != "onetwothree" {
		t.Errorf("got %q", got)
	}
}

func TestEndTagsCloseTheMostRecentMatchAndStraysAreIgnored(t *testing.T) {
	doc := Parse("</div><p>one<p>two</span><b>bold <i>both</b> after</i>")
	if got := elementNames(doc.Find("p").Children()); !reflect.DeepEqual(got, []string{"p"}) {
		t.Errorf("children of first p: %v", got)
	}
	if got := doc.Find("b").Text(); got != "bold both" {
		t.Errorf("b text %q", got)
	}
	if got := doc.Find("i").Text(); got != "both" {
		t.Errorf("i text %q", got)
	}
	if got := doc.Text(); got != "onetwobold both after" {
		t.Errorf("document text %q", got)
	}
}

func TestVoidElementsCloseImmediatelyAndSwallowOneStrayEndTag(t *testing.T) {
	div := Parse("<div><br>text</br></br><img src=x>more</div>").Find("div")
	if got := elementNames(div.Children()); !reflect.DeepEqual(got, []string{"br", "img"}) {
		t.Errorf("children %v", got)
	}
	if got := div.Text(); got != "textmore" {
		t.Errorf("text %q", got)
	}
	// A non-void self-closing tag is an empty element, not an open one.
	if got := Parse("<p/>after").Find("p").Text(); got != "" {
		t.Errorf("p text %q", got)
	}
}

func TestFindAndFindAllFilters(t *testing.T) {
	doc := Parse(`<a href="/store/apps/dev?id=1"><span>Dev</span></a>` +
		`<a href="/store/apps/category/GAME">Game</a>` +
		`<div aria-label="">empty</div><div aria-label="x">x</div>` +
		`<meta property="og:title" content="OG"><span itemprop="genre">G</span>`)
	if got := doc.Find("a", AttrMatches("href", regexp.MustCompile(`/store/apps/category`))).Text(); got != "Game" {
		t.Errorf("category link %q", got)
	}
	if got := doc.Find("a", AttrMatches("href", regexp.MustCompile(`/store/apps/dev`))).Find("span").Text(); got != "Dev" {
		t.Errorf("dev link %q", got)
	}
	var labels []string
	for _, d := range doc.FindAll("", AttrPresent("aria-label")) {
		labels = append(labels, d.Text())
	}
	if !reflect.DeepEqual(labels, []string{"empty", "x"}) {
		t.Errorf("aria labels %v", labels)
	}
	if got := doc.Find("meta", AttrEquals("property", "og:title")).Attr("content"); got != "OG" {
		t.Errorf("og:title %q", got)
	}
	if got := doc.Find("", AttrEquals("itemprop", "genre")).Text(); got != "G" {
		t.Errorf("genre %q", got)
	}
	if doc.Find("table") != nil {
		t.Error("found a table")
	}
	if got := elementNames(doc.FindAllAny([]string{"span", "meta"})); !reflect.DeepEqual(got, []string{"span", "meta", "span"}) {
		t.Errorf("span/meta %v", got)
	}
}

func TestAttributesValuelessAndRepeated(t *testing.T) {
	tag := Parse(`<div data-g-id="a" data-g-id="b" hidden></div>`).Find("div")
	if want := []Attr{{"data-g-id", "b"}, {"hidden", ""}}; !reflect.DeepEqual(tag.Attrs, want) {
		t.Errorf("attrs %v", tag.Attrs)
	}
	if _, ok := tag.Lookup("missing"); ok || tag.Attr("missing") != "" {
		t.Error("missing attribute reported present")
	}
}

func TestTitleStringFollowsSingleChildren(t *testing.T) {
	if v, ok := Parse("<title> Hi </title>").Title().StringValue(); !ok || v != " Hi " {
		t.Errorf("title string %q %v", v, ok)
	}
	if _, ok := Parse("<title></title>").Title().StringValue(); ok {
		t.Error("empty title has a string")
	}
	if v, ok := Parse("<div><b>x</b></div>").Find("div").StringValue(); !ok || v != "x" {
		t.Errorf("nested string %q %v", v, ok)
	}
	if _, ok := Parse("<div><b>x</b>y</div>").Find("div").StringValue(); ok {
		t.Error("two children have a string")
	}
	if Parse("<p>x</p>").Title() != nil {
		t.Error("found a title")
	}
}

func TestDecomposeRemovesTheSubtree(t *testing.T) {
	doc := Parse("<p>keep</p><script>drop()</script><noscript>also <b>drop</b></noscript>")
	for _, el := range doc.FindAllAny([]string{"script", "style", "noscript"}) {
		el.Decompose()
	}
	if got := doc.GetText(" ", true); got != "keep" {
		t.Errorf("text %q", got)
	}
	if doc.Find("b") != nil {
		t.Error("b survived")
	}
}

func TestNextSiblingSkipsStringsAndMarkupIncludesAttributes(t *testing.T) {
	doc := Parse(`<div id="a">A</div> text <div class="Category-row">B</div>`)
	sibling := doc.Find("div").NextElementSibling()
	if got := sibling.Text(); got != "B" {
		t.Errorf("sibling text %q", got)
	}
	if !strings.Contains(strings.ToLower(sibling.String()), "category") {
		t.Errorf("markup %q", sibling.String())
	}
	if sibling.NextElementSibling() != nil {
		t.Error("unexpected second sibling")
	}
}

func TestSerialisation(t *testing.T) {
	doc := Parse(`<div a='x"y' b="it's" c="&lt;&amp;"><br><!--c--><img>t&amp;</div><p></p>`)
	want := `<div a='x"y' b="it's" c="&lt;&amp;"><br/><!--c--><img/>t&amp;</div><p></p>`
	if got := doc.String(); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	if got, want := doc.Find("div").Find("br").String(), "<br/>"; got != want {
		t.Errorf("br %q", got)
	}
}

func TestDeeplyNestedUnclosedMarkupDoesNotRecurse(t *testing.T) {
	const depth = 20_000
	doc := Parse(strings.Repeat("<div>", depth) + "deep")
	if got := doc.Text(); got != "deep" {
		t.Errorf("text %q", got)
	}
	if got := len(doc.FindAll("div")); got != depth {
		t.Errorf("%d divs", got)
	}
	if !strings.HasPrefix(doc.String(), "<div><div>") {
		t.Error("serialisation does not start with nested divs")
	}
}

func TestStrayVoidEndTagKeepsTheStringOpenLikeBs4(t *testing.T) {
	// bs4 4.15.0 swallows the redundant </br> without ending the current
	// string, so "b" and "c" are one string: get_text gives "a bc", not
	// "a b c". Parity with the recorded extractor output wins here.
	cases := map[string]string{
		"<p>a<br>b</br>c</p>":       "a bc",
		"<p>a<img>b</img>c</p>":     "a bc",
		"<p>a<br>b</br>c</br>d</p>": "a bc d", // each void swallows at most one end tag
	}
	for markup, want := range cases {
		if got := Parse(markup).GetText(" ", true); got != want {
			t.Errorf("%q: got %q, want %q", markup, got, want)
		}
	}
}

func TestNextSiblingAfterDecompose(t *testing.T) {
	doc := Parse("<div>A</div><script>x</script><div>B</div><div>C</div>")
	doc.Find("script").Decompose()
	divs := doc.FindAll("div")
	if divs[0].NextElementSibling() != divs[1] || divs[1].NextElementSibling() != divs[2] {
		t.Error("siblings not relinked")
	}
}

// Tokenizer corner cases, each checked against CPython 3.12's html.parser
// driving the Python html_dom module.
func TestTokenizerCornerCases(t *testing.T) {
	cases := []struct{ markup, want string }{
		{"<script>a</b>c</script>d", "d"},
		{"<title>a<b>c</b>&amp;</title>", "a<b>c</b>&"},
		{"<textarea>  x </textarea>", "  x "},
		{"x<y", "x"},
		{"a < b", "a < b"},
		{"a &amp", "a &amp"}, // an incomplete entity at the end stays as written
		{"a & b", "a & b"},
		{"a&", "a&"},
		{"<!-->hidden<!-- -->shown", "shown"},
		{"<!---->a", "a"},
		{"a<!--unterminated", "a"},
		{"</>x</ x>y", "xy"},
		{"<a b='x>y'>z</a>", "z"},
		{"<a b= 'x>y", "y"},
		{"<p>a<b", "a"}, // an unterminated start tag swallows the rest
		{"<plaintext><b>x</b>", "<b>x</b>"},
		{"<![if !IE]>x<![endif]>", "x"},
		{"<SCRIPT>x</script >y", "y"},
		{"&#12a; &#zz; <b>bold</b>", "&#12a; &#zz; <b>bold</b>"},
		{"&a-", "&a-"},
	}
	for _, c := range cases {
		if got := Parse(c.markup).Text(); got != c.want {
			t.Errorf("%q: got %q, want %q", c.markup, got, c.want)
		}
	}
}

func TestHostileRepeatsParseInLinearTime(t *testing.T) {
	// A response body is capped at 5 MiB and is parsed in full. Each closed
	// void element used to be appended to a list that every end tag
	// scanned, so ~320 KB of "<div><img></div>" took seconds. The bound sits
	// far from linear so a slow runner does not flake.
	for _, unit := range []string{"<div><img></div>", "<br></br>x", "<div>"} {
		markup := strings.Repeat(unit, 1024*1024/len(unit))
		start := time.Now()
		Parse(markup)
		if elapsed := time.Since(start); elapsed > 10*time.Second {
			t.Errorf("%q: %v", unit, elapsed)
		}
	}
}
