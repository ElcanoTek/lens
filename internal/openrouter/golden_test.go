// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package openrouter

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/ElcanoTek/lens/internal/categories/pyjson"
)

// testdata/golden.json was recorded from the Python release
// (openrouter_client.py and ctv_processor.py) with a throwaway script that
// fed fixed inputs to the prompt builders, the vertical resolver, the
// response parsers, the CTV news routing and the CTV text helpers. These
// tests pin the Go port to byte-identical output.

func loadGolden(t *testing.T) pyjson.Object {
	t.Helper()
	raw, err := os.ReadFile("testdata/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	v, err := pyjson.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	return v.(pyjson.Object)
}

func goldenGet(t *testing.T, o pyjson.Object, key string) any {
	t.Helper()
	v, ok := o.Get(key)
	if !ok {
		t.Fatalf("golden has no %q", key)
	}
	return v
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func strList(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, item := range list {
		out = append(out, str(item))
	}
	return out
}

func firstDiff(a, b string) string {
	ra, rb := []rune(a), []rune(b)
	for i := 0; i < len(ra) && i < len(rb); i++ {
		if ra[i] != rb[i] {
			lo := max(0, i-40)
			return "at rune " + itoa(i) + ": got " + quote(string(ra[lo:min(len(ra), i+40)])) + " want " + quote(string(rb[lo:min(len(rb), i+40)]))
		}
	}
	return "length " + itoa(len(ra)) + " vs " + itoa(len(rb))
}

func itoa(i int) string { return pyStr(int64(i)) }

func quote(s string) string { return pyjson.DumpsUnicode(s) }

func TestGoldenPrompts(t *testing.T) {
	prompts := goldenGet(t, loadGolden(t), "prompts").(pyjson.Object)
	long := strings.Repeat("Plain words é ü 日本 ", 200)
	got := map[string]string{
		"site_scrape":       buildClassificationPrompt(SiteInput{Domain: "example.com", Content: long, Title: "Title é", MetaDescription: "Meta desc", Source: SourceScrape}),
		"site_scrape_short": buildClassificationPrompt(SiteInput{Domain: "example.com", Content: "short"}),
		"site_research":     buildClassificationPrompt(SiteInput{Domain: "reuters.com", Content: long + long, Title: "ignored", MetaDescription: "ignored", Source: SourceResearch}),
		"website_research": websiteResearchPrompt("example.com/travel") +
			customResearchInstructions([]string{`Is it "safe"?`, "Who? Options: Kids, Adults ü"}),
		"website_research_plain": websiteResearchPrompt("example.com") + customResearchInstructions(nil),
		"app_ios": buildAppPrompt(AppInput{AppID: "123", AppName: "App é", Developer: "Dev", StoreCategory: "Games",
			Description: long, Rating: 4.25, RatingCount: 1234567, ContentForLLM: "extra context", Platform: "ios"}),
		"app_android": buildAppPrompt(AppInput{AppID: "com.x", AppName: "App", Developer: "Dev", StoreCategory: "Tools",
			Description: "desc", Rating: 3.0, RatingCount: 12, Downloads: "1,000,000+", ContentForLLM: "desc", Platform: "android"}),
		"app_empty":         buildAppPrompt(AppInput{AppID: "1", Platform: "ios"}),
		"app_round":         buildAppPrompt(AppInput{AppID: "1", Description: "d", Rating: 4.05, RatingCount: 999, ContentForLLM: "other", Platform: "ios"}),
		"ctv_research_min":  buildCTVResearchPrompt(CTVApp{AppName: "Netflix"}),
		"ctv_research_full": buildCTVResearchPrompt(CTVApp{AppName: "ESPN", BundleID: "com.espn.score", Platform: "Roku", URL: "https://espn.com", Publisher: "Disney"}) + customResearchInstructions([]string{"Q1"}),
		"ctv_class_min":     buildCTVClassificationPrompt(CTVApp{AppName: "Netflix"}, "Research text"),
		"ctv_class_full":    buildCTVClassificationPrompt(CTVApp{AppName: "Hulu", BundleID: "com.hulu.plus", Platform: "Fire TV", URL: "https://hulu.com", Publisher: "Disney"}, "Hulu is a streaming service..."),
	}
	for _, m := range prompts {
		want := str(m.Value)
		g, ok := got[m.Key]
		if !ok {
			t.Errorf("no Go case for golden prompt %q", m.Key)
			continue
		}
		if g != want {
			t.Errorf("prompt %s differs: %s", m.Key, firstDiff(g, want))
		}
	}
}

func TestGoldenVerticals(t *testing.T) {
	for _, row := range goldenGet(t, loadGolden(t), "verticals").([]any) {
		pair := row.([]any)
		label, want := str(pair[0]), strList(pair[1])
		got := VerticalTiers(label)
		if len(got) == 0 && len(want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("VerticalTiers(%q) = %q, want %q", label, got, want)
		}
	}
}

func TestGoldenMarkdown(t *testing.T) {
	for _, row := range goldenGet(t, loadGolden(t), "markdown").([]any) {
		pair := row.([]any)
		if got := StripMarkdown(str(pair[0])); got != str(pair[1]) {
			t.Errorf("StripMarkdown(%q) = %q, want %q", str(pair[0]), got, str(pair[1]))
		}
	}
}

func TestGoldenTruncate(t *testing.T) {
	for _, row := range goldenGet(t, loadGolden(t), "truncate").([]any) {
		triple := row.([]any)
		text, n, want := str(triple[0]), int(triple[1].(int64)), str(triple[2])
		if got := TruncateResearch(text, n); got != want {
			t.Errorf("TruncateResearch(%.40q, %d) = %q, want %q", text, n, got, want)
		}
	}
}

func TestGoldenPlatforms(t *testing.T) {
	for _, row := range goldenGet(t, loadGolden(t), "platforms").([]any) {
		pair := row.([]any)
		if got := DetectCTVPlatform(str(pair[0])); got != str(pair[1]) {
			t.Errorf("DetectCTVPlatform(%q) = %q, want %q", str(pair[0]), got, str(pair[1]))
		}
	}
}

func classificationFromDict(o pyjson.Object) Classification {
	g := func(k string) string { v, _ := o.Get(k); return str(v) }
	return Classification{
		Quality: g("quality"), Justification: g("justification"), Vertical: g("vertical"),
		VerticalTier1: g("vertical_tier_1"), VerticalTier2: g("vertical_tier_2"), VerticalTier3: g("vertical_tier_3"),
		Description: g("description"), Confidence: g("confidence"), Language: g("language"),
		PoliticalLeaning: g("political_leaning"), AudienceSize: g("audience_size"),
		TargetAudience: g("target_audience"), ContentType: g("content_type"), NetworkAffiliation: g("network_affiliation"),
	}
}

func TestGoldenRouting(t *testing.T) {
	cases := goldenGet(t, loadGolden(t), "routing").([]any)
	if len(cases) < 1000 {
		t.Fatalf("only %d routing cases", len(cases))
	}
	for _, raw := range cases {
		tc := raw.(pyjson.Object)
		name, research := str(goldenGet(t, tc, "app_name")), str(goldenGet(t, tc, "research"))
		in := classificationFromDict(goldenGet(t, tc, "input").(pyjson.Object))
		want := classificationFromDict(goldenGet(t, tc, "output").(pyjson.Object))
		if got := ApplyCTVRouting(name, research, in); got != want {
			t.Errorf("ApplyCTVRouting(%q, %.50q, %s/%s):\n got %+v\nwant %+v", name, research, in.ContentType, in.PoliticalLeaning, got, want)
		}
		if got := IsCTVNewsApp(name, research, in.ContentType); got != goldenGet(t, tc, "is_news") {
			t.Errorf("IsCTVNewsApp(%q, %.50q, %s) = %v", name, research, in.ContentType, got)
		}
	}
	for _, row := range goldenGet(t, loadGolden(t), "entertainment").([]any) {
		pair := row.([]any)
		if got := IsPrimarilyEntertainment(str(pair[0])); got != pair[1] {
			t.Errorf("IsPrimarilyEntertainment(%.50q) = %v", str(pair[0]), got)
		}
	}
}

func TestGoldenParsing(t *testing.T) {
	golden := loadGolden(t)
	inputs := goldenGet(t, golden, "parse_inputs").(pyjson.Object)
	for _, kind := range []string{"parse_site", "parse_ctv"} {
		for _, m := range goldenGet(t, golden, kind).(pyjson.Object) {
			resp := goldenGet(t, inputs, m.Key).(pyjson.Object)
			var got Classification
			var err error
			if kind == "parse_site" {
				got, err = parseClassification(resp, "classify_website")
			} else {
				got, err = parseCTVClassification(resp)
			}
			want := m.Value.(pyjson.Object)
			if wantErr, ok := want.Get("error"); ok {
				if err == nil {
					t.Errorf("%s/%s: no error, want %q", kind, m.Key, str(wantErr))
				} else if typ, _ := want.Get("type"); typ == "ValueError" && err.Error() != str(wantErr) {
					t.Errorf("%s/%s: error %q, want %q", kind, m.Key, err, str(wantErr))
				}
				continue
			}
			if err != nil {
				t.Errorf("%s/%s: %v", kind, m.Key, err)
				continue
			}
			if w := classificationFromDict(goldenGet(t, want, "ok").(pyjson.Object)); got != w {
				t.Errorf("%s/%s:\n got %+v\nwant %+v", kind, m.Key, got, w)
			}
		}
	}
}
