// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package htmldom_test

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/lens/internal/htmldom"
	"github.com/ElcanoTek/lens/internal/scrape"
	"github.com/ElcanoTek/lens/internal/stores"
)

// Every operation the scrapers use must stay linear. Three quadratic paths
// reached review one after another in the Python module (closed-void
// bookkeeping, list.remove() in decompose, slicing in find_next_sibling),
// each fast on ordinary pages and seconds-long on a hostile one. A response
// body is up to 5 MiB, so this harness times each operation at N and 2N on
// flat (many siblings) and deep (many nested levels) markup and fails when
// doubling the input costs much more than double the time.
//
// Timings take the best of several runs, and a size pair is re-measured
// before failing, so a busy runner does not flake: linear work doubles
// (ratio ~2), quadratic work quadruples (ratio ~4), and the bound sits
// between them.

const (
	scaleN   = 20_000 // nodes (or nesting levels) at the smaller size
	maxRatio = 3.0    // 2N vs N; linear is ~2, quadratic ~4
	maxTime  = 10 * time.Second
	devLink  = `<a href="/store/apps/dev?id=1">Dev</a>`
)

// flat has many siblings: scripts, styles, plain and developer-like divs,
// links.
func flat(n int) string {
	unit := `<div class="row">x</div><script>s()</script><style>p{}</style>` +
		`<noscript>n</noscript><img src=x></img><a href="/store/apps/details">a</a>`
	return "<body>" + strings.Repeat(unit, n/6) + devLink + "</body>"
}

// deep has many nested levels, with the one interesting node at the bottom.
func deep(n int) string { return strings.Repeat("<div><span>", n/2) + devLink + "x" }

var devRe = regexp.MustCompile(`/store/apps/dev`)

var operations = map[string]func(*htmldom.Node){
	"find_regex_last":       func(d *htmldom.Node) { d.Find("a", htmldom.AttrMatches("href", devRe)) },
	"find_attr_missing":     func(d *htmldom.Node) { d.Find("", htmldom.AttrEquals("data-g-id", "description")) },
	"find_all":              func(d *htmldom.Node) { d.FindAll("div") },
	"find_all_attr_present": func(d *htmldom.Node) { d.FindAll("", htmldom.AttrPresent("href")) },
	"get_text":              func(d *htmldom.Node) { _ = d.Text() },
	"get_text_strip":        func(d *htmldom.Node) { d.GetText(" ", true) },
	"every_next_sibling": func(d *htmldom.Node) {
		for _, el := range d.FindAll("") {
			el.NextElementSibling()
		}
	},
	"title_string": func(d *htmldom.Node) {
		if t := d.Title(); t != nil {
			t.StringValue()
		}
	},
	"serialise": func(d *htmldom.Node) { _ = d.String() },
	"descendants": func(d *htmldom.Node) {
		for range d.Descendants() {
		}
	},
	"decompose_noise": func(d *htmldom.Node) { // mutating: parsed fresh each run
		for _, el := range d.FindAllAny([]string{"script", "style", "noscript"}) {
			el.Decompose()
		}
	},
}

func best(run func(), prepare func(), repeats int) time.Duration {
	var fastest time.Duration
	for i := 0; i < repeats; i++ {
		prepare()
		start := time.Now()
		run()
		if d := time.Since(start); i == 0 || d < fastest {
			fastest = d
		}
	}
	return fastest
}

// assertLinear times op(n) for n = N and 2N.
func assertLinear(t *testing.T, timeAt func(n int) time.Duration) {
	t.Helper()
	var small, large time.Duration
	for attempt := 0; attempt < 3; attempt++ {
		small, large = timeAt(scaleN), timeAt(2*scaleN)
		if large > maxTime {
			t.Fatalf("%v at 2N", large)
		}
		// Below ~5 ms the clock is noise; only a slow operation can be quadratic.
		if large < 5*time.Millisecond || float64(large) < maxRatio*float64(small) {
			return
		}
	}
	t.Fatalf("doubling the input took %.1fx as long (%v -> %v)", float64(large)/float64(small), small, large)
}

func TestOperationsScaleLinearly(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	shapes := map[string]func(int) string{"flat": flat, "deep": deep}
	for shapeName, shape := range shapes {
		markup := map[int]string{scaleN: shape(scaleN), 2 * scaleN: shape(2 * scaleN)}
		t.Run(shapeName+"/parse", func(t *testing.T) {
			assertLinear(t, func(n int) time.Duration {
				return best(func() { htmldom.Parse(markup[n]) }, func() {}, 3)
			})
		})
		parsed := map[int]*htmldom.Node{scaleN: htmldom.Parse(markup[scaleN]), 2 * scaleN: htmldom.Parse(markup[2*scaleN])}
		for name, op := range operations {
			t.Run(shapeName+"/"+name, func(t *testing.T) {
				assertLinear(t, func(n int) time.Duration {
					var doc *htmldom.Node
					prepare := func() { doc = parsed[n] }
					if name == "decompose_noise" {
						prepare = func() { doc = htmldom.Parse(markup[n]) }
					}
					return best(func() { op(doc) }, prepare, 3)
				})
			})
		}
	}
}

func TestExtractorsScaleLinearlyOnFlatPages(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	// Deep markup is left out on purpose: the Play Store developer fallback
	// takes the text of every <div>, and on nested divs that output itself
	// grows quadratically (bs4 behaved the same). Flat pages are the shape a
	// real store or news page has, and they must stay linear. The app name
	// makes the Play Store parse run every extractor, and without a
	// developer link its fallback (a NextElementSibling per <div>) runs.
	page := func(n int) string {
		return `<h1 itemprop="name"><span>App</span></h1>` + strings.Replace(flat(n), devLink, "", 1)
	}
	extractors := map[string]func(string){
		"site_parse_html": func(html string) { scrape.ParseHTML("https://example.test/", html) },
		"play_parse_html": func(html string) { stores.ParsePlayStorePage("com.example", html, "t") },
	}
	for name, extract := range extractors {
		t.Run(name, func(t *testing.T) {
			pages := map[int]string{scaleN: page(scaleN), 2 * scaleN: page(2 * scaleN)}
			assertLinear(t, func(n int) time.Duration {
				return best(func() { extract(pages[n]) }, func() {}, 3)
			})
		})
	}
}
