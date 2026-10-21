// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package detect

import (
	"bufio"
	"encoding/json"
	"os"
	"testing"

	"github.com/ElcanoTek/lens/internal/item"
	"github.com/ElcanoTek/lens/internal/tabular"
)

// TestGoldenAgainstPython checks ContentType, LooksProcessable and
// SpreadsheetCell against values recorded from the Python input_detector
// (detect_content_type, _looks_processable, spreadsheet_cell) over ~1,700
// generated and hand-picked identifiers.
func TestGoldenAgainstPython(t *testing.T) {
	fh, err := os.Open("testdata/content_types.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	n := 0
	for sc.Scan() {
		var rec []any
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatal(err)
		}
		v := rec[0].(string)
		if got := ContentType(v); string(got) != rec[1].(string) {
			t.Errorf("ContentType(%q) = %s, Python %s", v, got, rec[1])
		}
		if got := LooksProcessable(v); got != rec[2].(bool) {
			t.Errorf("LooksProcessable(%q) = %v, Python %v", v, got, rec[2])
		}
		if got := SpreadsheetCell(v); got != rec[3].(string) && !tabular.IsNA(v) {
			t.Errorf("SpreadsheetCell(%q) = %q, Python %q", v, got, rec[3])
		}
		n++
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if n < 1000 {
		t.Fatalf("only %d golden records", n)
	}
}

func expectAll(t *testing.T, want item.ContentType, values ...string) {
	t.Helper()
	for _, v := range values {
		if got := ContentType(v); got != want {
			t.Errorf("ContentType(%q) = %s, want %s", v, got, want)
		}
	}
}

// Ported from tests/test_input_detector.py.
func TestWebsiteDetection(t *testing.T) {
	// full URLs with protocol
	expectAll(t, item.Website, "https://example.com", "http://example.com", "https://www.example.com",
		"http://www.example.com/path/to/page", "https://example.com:8080", "https://example.com/path?query=1&foo=bar",
		"https://sub.domain.example.com", "ftp://files.example.com", "//example.com/resource")
	// common TLDs
	expectAll(t, item.Website, "example.com", "example.org", "example.net", "example.io", "example.co", "example.edu",
		"example.gov", "example.info", "example.biz", "example.me", "example.tv", "example.xyz", "example.app",
		"example.dev", "example.tech", "example.ai")
	// country TLDs
	expectAll(t, item.Website, "example.uk", "example.de", "example.fr", "example.jp", "example.cn", "example.br",
		"example.in", "example.au", "example.ca", "example.ru")
	// compound TLDs
	expectAll(t, item.Website, "example.co.uk", "example.com.au", "example.com.br", "example.co.jp", "example.co.kr", "example.co.nz")
	// subdomains
	expectAll(t, item.Website, "api.example.com", "www.example.org", "blog.example.net", "app.example.io",
		"mail.example.co.uk", "cdn.static.example.com")
	// The key case: TLD-like prefixes on real domains are websites.
	expectAll(t, item.Website, "io.example.com", "co.example.com", "me.example.com", "dev.example.org", "ai.startup.com",
		"app.company.io", "net.something.com", "org.website.net", "com.company.io")
	expectAll(t, item.Website, "www.example.com", "www.example.org", "www.example.net")
	// paths, queries, fragments
	expectAll(t, item.Website, "example.com/path", "example.com/path/to/page", "example.com/page.html", "example.com/api/v1/users")
	expectAll(t, item.Website, "example.com?query=1", "example.com/page?id=123&name=test", "example.com/search?q=hello+world")
	expectAll(t, item.Website, "example.com#section", "example.com/page#top", "example.com/docs#api-reference")
	// ports, IPs, localhost
	expectAll(t, item.Website, "example.com:8080", "example.com:3000", "example.com:443", "localhost:8080", "localhost:3000/api")
	expectAll(t, item.Website, "192.168.1.1", "10.0.0.1", "127.0.0.1", "192.168.1.1:8080", "10.0.0.1/api")
	expectAll(t, item.Website, "localhost", "localhost:8080", "localhost/api", "localhost:3000/path")
	// hyphens
	expectAll(t, item.Website, "my-website.com", "test-site.org", "hello-world.net", "sub-domain.example.com", "api-v2.company.io")
}

func TestAndroidAppDetection(t *testing.T) {
	expectAll(t, item.AndroidApp, "com.facebook.katana", "com.instagram.android", "com.twitter.android",
		"com.google.android.apps.maps", "com.spotify.music", "com.netflix.mediaclient", "org.mozilla.firefox", "net.oneplus.launcher")
	expectAll(t, item.AndroidApp, "com.example.my_app", "com.company.app_name", "org.project.module_one")
	expectAll(t, item.AndroidApp, "com.example.app2", "com.company.v2app", "org.project.module1")
	expectAll(t, item.AndroidApp, "com.example.app", "net.company.product", "org.foundation.project", "io.startup.mobile",
		"co.company.app", "me.developer.app", "app.company.product", "dev.studio.game", "ai.company.assistant", "tv.company.streaming")
	// no known prefix, non-TLD leaf
	expectAll(t, item.AndroidApp, "tunein.player", "mobi.ifunny", "jigsaw.puzzle.game.banana", "kidultlovin.word.zen", "beetles.puzzle.solitaire")
	// TLD leaf but nested too deep for a domain
	expectAll(t, item.AndroidApp, "easy.sudoku.puzzle.solver.free", "ball.sort.puzzle.color.sorting.bubble.games",
		"classic.relax.happy.color.by.number.game", "park.master.car.parking.games")
	// real packages
	expectAll(t, item.AndroidApp, "com.whatsapp", "com.facebook.orca", "com.snapchat.android", "com.zhiliaoapp.musically",
		"com.amazon.mShop.android.shopping", "com.google.android.youtube", "com.microsoft.teams", "com.discord",
		"org.telegram.messenger", "com.Slack")
}

func TestAndroidAppNotDetected(t *testing.T) {
	expectAll(t, item.Website, "example.com", "company.io", "startup.ai", "mysite.dev", "website.app")
	expectAll(t, item.Website, "api.example.com", "mobile.company.io", "app.startup.dev", "client.service.app")
	// Shallow strings whose leaf is a real TLD are ambiguous: default to website.
	expectAll(t, item.Website, "kik.android", "flipboard.social")
	// New gTLDs / ccTLDs resolve as websites through the IANA list.
	expectAll(t, item.Website, "brand.shop", "example.store", "mysite.blog", "fullmeasure.news", "eltiempo.pe", "ojo.pe",
		"conectate.com.do", "pulse.com.gh")
	expectAll(t, item.Website, "io.github.com", "co.pinterest.com", "me.medium.com", "dev.to", "ai.google.com",
		"app.slack.com", "api.twitter.com", "cdn.example.com", "static.example.org")
}

func TestIOSAppDetection(t *testing.T) {
	expectAll(t, item.IOSApp, "123456789", "1", "999999999999", "284882215", "389801252", "00123456", "0001",
		"333903271", "310633997", "835599320")
}

func TestEdgeCases(t *testing.T) {
	expectAll(t, item.Website, "", "   ")
	expectAll(t, item.Website, "  example.com  ", "EXAMPLE.COM", "Example.Com")
	expectAll(t, item.AndroidApp, "  com.example.app  ", "COM.EXAMPLE.APP")
	expectAll(t, item.IOSApp, "  123456  ")
	expectAll(t, item.Website, "example.com/path?a=1&b=2", "example.com#section", "example.com/page.html", "user@example.com")
	expectAll(t, item.Website, "com-example-app", "com.example/app", "com.example:app", "com.example?app")
}

func TestHelperFunctions(t *testing.T) {
	for _, v := range []string{"https://example.com", "http://example.com", "ftp://files.example.com", "//example.com",
		"example.com", "example.org", "example.net", "example.io", "example.com/path", "example.com?query=1", "example.com#fragment"} {
		if !isURLOrWebsite(v) {
			t.Errorf("isURLOrWebsite(%q) = false", v)
		}
	}
	for v, want := range map[string]bool{
		"com.example.app": true, "org.mozilla.firefox": true, "com.company.my_app": true,
		"example.com": true, // valid format, just short
		"com-example-app": false, "com.example/app": false, "com": false, "": false,
	} {
		if got := isValidAndroidPackage(v); got != want {
			t.Errorf("isValidAndroidPackage(%q) = %v", v, got)
		}
	}
}

func TestContentTypeHint(t *testing.T) {
	for v, want := range map[string]item.ContentType{
		"IOS": item.IOSApp, "iPhone": item.IOSApp, "ANDROID": item.AndroidApp, "Google Play": item.AndroidApp,
		"  Website ": item.Website, "url": item.Website, "android": item.AndroidApp,
	} {
		if got, ok := ParseContentTypeHint(v); !ok || got != want {
			t.Errorf("ParseContentTypeHint(%q) = %s, %v", v, got, ok)
		}
	}
	// CTV is intentionally not mapped: it is routed as a whole file.
	for _, v := range []string{"", "news", "CTV", "nan"} {
		if got, ok := ParseContentTypeHint(v); ok {
			t.Errorf("ParseContentTypeHint(%q) = %s", v, got)
		}
	}
	// The hint wins over value detection on an ambiguous identifier.
	if ContentType("kik.android") != item.Website {
		t.Error("kik.android should be a website by value")
	}
}

func frame(cols []string, columns ...[]string) *tabular.Frame {
	n := 0
	for _, c := range columns {
		n = max(n, len(c))
	}
	rows := make([][]string, n)
	for r := range rows {
		rows[r] = make([]string, len(cols))
		for c := range cols {
			if r < len(columns[c]) {
				rows[r][c] = columns[c][r]
			}
		}
	}
	return tabular.New(cols, rows)
}

func TestTypeColumn(t *testing.T) {
	f := frame([]string{"Domain", "Type"}, []string{"333903271", "com.foo.bar", "example.com"}, []string{"IOS", "ANDROID", "WEBSITE"})
	if got := TypeColumn(f, "Domain", TypeColumnMinRatio); got != "Type" {
		t.Errorf("master schema: %q", got)
	}
	f = frame([]string{"Domain", "Type"}, []string{"a.com", "b.com"}, []string{"News", "Sports"})
	if got := TypeColumn(f, "Domain", TypeColumnMinRatio); got != "" {
		t.Errorf("unrelated Type column accepted: %q", got)
	}
	f = frame([]string{"type"}, []string{"website", "website"})
	if got := TypeColumn(f, "type", TypeColumnMinRatio); got != "" {
		t.Errorf("input column used as type column: %q", got)
	}
	// Half recognised is enough; blanks don't count against it.
	f = frame([]string{"Domain", " Platform "}, []string{"a", "b", "c", "d"}, []string{"ios", "tv", "", "NA"})
	if got := TypeColumn(f, "Domain", TypeColumnMinRatio); got != " Platform " {
		t.Errorf("platform column: %q", got)
	}
}

func TestInputColumn(t *testing.T) {
	cases := []struct {
		cols []string
		want string
	}{
		{[]string{"Domain"}, "Domain"},
		{[]string{"Notes", "url", "pageURL"}, "pageURL"},     // exact, candidate order
		{[]string{"Notes", "PAGEURL", "DOMAIN"}, "PAGEURL"},  // case-insensitive, candidate order
		{[]string{"x", "Domain ", "y"}, "Domain "},           // partial
		{[]string{"Ad Unit", "Package Names"}, "Package Names"},
		{[]string{"rank", "notes"}, ""},
		{[]string{"URL", "url"}, "url"}, // exact "url" precedes "URL" in the candidates
	}
	for _, tc := range cases {
		if got := InputColumn(tabular.New(tc.cols, nil)); got != tc.want {
			t.Errorf("InputColumn(%q) = %q, want %q", tc.cols, got, tc.want)
		}
	}
}

func TestInputColumnByContent(t *testing.T) {
	f := frame([]string{"Position", "Comments", "Address"},
		[]string{"1", "2", "3"}, []string{"great site", "blocked last week", "n/a"}, []string{"nytimes.com", "wsj.com", "cnn.com"})
	if got := InputColumnByContent(f, ContentMinRatio, ContentSampleSize); got != "Address" {
		t.Errorf("got %q", got)
	}
	f = frame([]string{"a", "b"}, []string{"hello", "world"}, []string{"1", "2"})
	if got := InputColumnByContent(f, ContentMinRatio, ContentSampleSize); got != "" {
		t.Errorf("unclassifiable file got %q", got)
	}
	// Only the first sampleSize non-blank values are scored.
	vals := append(make([]string, 0, 30), "a.com", "", "b.com")
	for range 20 {
		vals = append(vals, "junk")
	}
	f = frame([]string{"c"}, vals)
	if got := InputColumnByContent(f, ContentMinRatio, 2); got != "c" {
		t.Errorf("sample size ignored: %q", got)
	}
}

func TestBatchType(t *testing.T) {
	if ct, ok := BatchType(nil); ok || ct != "" {
		t.Error("empty batch")
	}
	vals := []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "a.com"}
	if ct, ok := BatchType(vals); !ok || ct != item.IOSApp {
		t.Errorf("90%% iOS = %s %v", ct, ok)
	}
	if _, ok := BatchType(vals[1:]); !ok {
		t.Error("all-but-one iOS should still dominate")
	}
	if _, ok := BatchType([]string{"1", "a.com"}); ok {
		t.Error("mixed batch reported as homogeneous")
	}
}

func TestAnalyze(t *testing.T) {
	f := frame([]string{"pageURL"}, []string{"example.com", "333903271", "NA", "com.spotify.music"})
	a, err := Analyze(f)
	if err != nil {
		t.Fatal(err)
	}
	if a.InputColumn != "pageURL" || len(a.Items) != 3 || !a.Mixed() {
		t.Errorf("analysis = %+v", a)
	}
	if !a.Has(item.IOSApp) || !a.Has(item.AndroidApp) || !a.Has(item.Website) || a.Has(item.CTVApp) {
		t.Error("Has")
	}
	if got := a.ItemsOfType(item.IOSApp); len(got) != 1 || got[0] != "333903271" {
		t.Errorf("ItemsOfType = %q", got)
	}
	if _, err := Analyze(frame([]string{"pageURL"}, []string{"", "NA"})); err != ErrNoValues { //nolint:errorlint // sentinel returned unwrapped
		t.Errorf("err = %v", err)
	}
	if _, err := Analyze(frame([]string{"x"}, []string{"hello"})); err != ErrNoInputColumn { //nolint:errorlint // sentinel returned unwrapped
		t.Errorf("err = %v", err)
	}
}

func TestSpreadsheetCell(t *testing.T) {
	for in, want := range map[string]string{
		"54092.0": "54092", " 54092.0 ": "54092", "54092": "54092", "-5.0": "-5.0", "1.50": "1.50",
		"nan": "", "NaN": "", "None": "", "<NA>": "", "NaT": "", "nat": "", "NA": "", "  ": "", "": "",
		" B091RFCS5V ": "B091RFCS5V", ".0": ".0",
	} {
		if got := SpreadsheetCell(in); got != want {
			t.Errorf("SpreadsheetCell(%q) = %q, want %q", in, got, want)
		}
	}
}
