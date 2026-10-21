// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package openrouter

import (
	"strings"
	"testing"
)

// Named cases from tests/test_ctv_functionality.py; testdata/golden.json
// pins the exhaustive behaviour.

func newsInput(contentType, leaning string) Classification {
	return Classification{
		ContentType: contentType, Vertical: "Entertainment > Television", VerticalTier1: "Entertainment",
		VerticalTier2: "Television", PoliticalLeaning: leaning, Justification: "J.",
	}
}

func TestCTVNewsRouting(t *testing.T) {
	cases := []struct {
		name, app, research, contentType, leaning string
		tier1, tier2, vertical, wantLeaning, note string
	}{
		{"news content type -> civic affairs", "KARE 11 News",
			"This is a local news station providing local news coverage, breaking news, and headlines for the community.",
			"News", "Non-Political", "Politics", "Civic affairs", "Politics > Civic affairs", "Center", "(post-routed for CTV news)"},
		{"election keywords", "Election Central",
			"This news app covers election coverage, election results, and presidential election news with ballot tracking and electoral college analysis.",
			"News", "Non-Political", "Politics", "Elections", "Politics > Elections", "Center", "(post-routed for CTV election news)"},
		{"weather app", "The Weather Channel",
			"Weather Channel provides weather forecast, weather radar, storm tracker, severe weather alerts, and meteorologist reports. Track temperature and hurricane updates.",
			"News", "Non-Political", "Science", "Weather", "Science > Weather", "Non-Political", "(post-routed for CTV weather app)"},
		{"crime app", "Crime Watch Daily",
			"True crime news reporting. Covers crime reports, police investigations, court cases, criminal arrests, and crime blotter updates.",
			"News", "Non-Political", "Crime", "", "Crime", "Non-Political", "(post-routed for CTV crime app)"},
		{"disaster app", "Emergency Alert",
			"Emergency alert system providing disaster updates, evacuation notices, wildfire tracking, earthquake reports, and emergency management information.",
			"News", "Non-Political", "Disasters", "", "Disasters", "Non-Political", "(post-routed for CTV disaster app)"},
		{"network name", "CNN", "Cable television network providing 24-hour coverage of current events and breaking stories.",
			"Live TV", "Non-Political", "Politics", "Civic affairs", "Politics > Civic affairs", "Center", "(post-routed for CTV news)"},
		{"entertainment not routed", "Netflix",
			"Netflix is a streaming service offering movies, scripted series, comedy series, drama series, reality shows, and Netflix original programming. The platform offers video on demand and binge watch capabilities. Some news documentaries available.",
			"On-Demand Streaming", "Non-Political", "Entertainment", "Television", "Entertainment > Television", "Non-Political", ""},
		{"sports unchanged", "ESPN", "ESPN provides live sports coverage including football, basketball, baseball, and other athletic events.",
			"Live Sports", "Non-Political", "Entertainment", "Television", "Entertainment > Television", "Non-Political", ""},
		{"explicit leaning kept", "Conservative News", "Conservative news network providing right-leaning news coverage and political commentary.",
			"News", "Right", "Politics", "Civic affairs", "Politics > Civic affairs", "Right", "(post-routed for CTV news)"},
		{"news mentioning weather stays news", "KARE 11 News",
			"Local news station providing breaking news, headlines, weather forecast,\n traffic updates, local events coverage, sports scores, and community news. The station covers\n hurricane season updates, temperature reports, and storm tracking along with election coverage,\n crime reports, and human interest stories.",
			"News", "Non-Political", "Politics", "Civic affairs", "Politics > Civic affairs", "Center", "(post-routed for CTV news)"},
		{"local station", "WCCO News", "WCCO is a CBS affiliate providing local news, weather, sports,\n and community coverage. Features breaking news, investigative reports, and daily newscasts.",
			"News", "Non-Political", "Politics", "Civic affairs", "Politics > Civic affairs", "Center", "(post-routed for CTV news)"},
	}
	for _, tc := range cases {
		got := ApplyCTVRouting(tc.app, tc.research, newsInput(tc.contentType, tc.leaning))
		if got.VerticalTier1 != tc.tier1 || got.VerticalTier2 != tc.tier2 || got.Vertical != tc.vertical ||
			got.PoliticalLeaning != tc.wantLeaning || (got.VerticalTier1 != "Entertainment" && got.VerticalTier3 != "") {
			t.Errorf("%s: got %+v", tc.name, got)
		}
		if tc.note != "" && !strings.Contains(got.Justification, tc.note) {
			t.Errorf("%s: justification %q lacks %q", tc.name, got.Justification, tc.note)
		}
		if tc.note == "" && got.Justification != "J." {
			t.Errorf("%s: justification changed: %q", tc.name, got.Justification)
		}
		if tc.wantLeaning == "Center" && !strings.Contains(got.Justification, "Political_Leaning set to Center due to Politics vertical") {
			t.Errorf("%s: missing leaning note", tc.name)
		}
	}
}

func TestIsCTVNewsAppDetection(t *testing.T) {
	if !IsCTVNewsApp("TestApp", "Some content", "News") ||
		!IsCTVNewsApp("TestApp", "breaking news and headlines coverage", "Live TV") ||
		!IsCTVNewsApp("CBS News", "general content", "Live TV") ||
		IsCTVNewsApp("ESPN", "sports coverage and games", "Live Sports") {
		t.Fatal("news detection")
	}
	if !IsPrimarilyEntertainment("Netflix offers movies, scripted series, comedy series, drama series, TV shows, streaming content, video on demand, and original programming.") ||
		IsPrimarilyEntertainment("Breaking news, headlines, current events, newscast, news coverage, journalism, and news programming.") {
		t.Fatal("entertainment detection")
	}
}

func TestStripMarkdownCases(t *testing.T) {
	cases := map[string]string{
		"":                                 "",
		"# Header 1":                       "Header 1",
		"#### Header 4":                    "Header 4",
		"**bold text**":                    "bold text",
		"__bold text__":                    "bold text",
		"Some **bold** word":               "Some bold word",
		"*italic text*":                    "italic text",
		"_italic text_":                    "italic text",
		"Some *italic* word":               "Some italic word",
		"`code`":                           "code",
		"Use `print()` function":           "Use print() function",
		"[Google](https://google.com)":     "Google",
		"[Link](url) and [Another](url2)":  "Link and Another",
		"Multiple[1] citations[2] here[3]": "Multiple citations here",
		"Reference [12] number":            "Reference  number",
		"Line 1\n\n\n\nLine 2":             "Line 1\n\nLine 2",
		"A\n\n\n\n\nB\n\n\n\nC":            "A\n\nB\n\nC",
		"This is plain text with no Markdown formatting.": "This is plain text with no Markdown formatting.",
	}
	for in, want := range cases {
		if got := StripMarkdown(in); got != want {
			t.Errorf("StripMarkdown(%q) = %q, want %q", in, got, want)
		}
	}
	got := StripMarkdown("# Title\nSome content\n## Subtitle")
	if strings.Contains(got, "# ") || !strings.Contains(got, "Title") || !strings.Contains(got, "Subtitle") {
		t.Errorf("headers: %q", got)
	}
}

func TestTruncateResearchCases(t *testing.T) {
	if got := TruncateResearch("This is short text.", DefaultResearchSummaryLength); got != "This is short text." {
		t.Error(got)
	}
	got := TruncateResearch(strings.Repeat("First sentence. ", 50)+strings.Repeat("Second sentence. ", 50), 100)
	if !strings.HasSuffix(got, ".") || strings.Contains(got, "...") || len(got) > 100 {
		t.Errorf("sentence boundary: %q", got)
	}
	if got := TruncateResearch(strings.Repeat("A", 1500), 1000); !strings.HasSuffix(got, "...") {
		t.Errorf("ellipsis: %q", got)
	}
	if got := TruncateResearch(strings.Repeat("Wow! ", 100), 100); !strings.HasSuffix(got, "!") {
		t.Errorf("exclamation: %q", got)
	}
	if got := TruncateResearch(strings.Repeat("How does this work? ", 100), 100); !strings.HasSuffix(got, "?") {
		t.Errorf("question: %q", got)
	}
	// Characters, not bytes.
	if got := TruncateResearch(strings.Repeat("é", 1200), 1000); got != strings.Repeat("é", 1000)+"..." {
		t.Errorf("runes: %d", len(got))
	}
}

func TestDetectCTVPlatformCases(t *testing.T) {
	cases := map[string]string{
		"54092": "Roku", "B091RFCS5V": "Fire TV", "b0lowercase": "Fire TV", "com.foxsports.videogo": "Android TV",
		"COM.UPPERCASE.APP": "Android TV", "vizio.app.streaming": "Vizio", "tv.streaming.app": "Apple TV",
		"G00009197740": "Samsung TV", "g99999": "Samsung TV", "9wzdncrfjv7w": "Xbox", "crunchyroll.roku": "Roku",
		"xbox.crunchyroll": "Xbox", "playstation.crunchyroll": "PlayStation", "randomstring": "Unknown",
		"netflix": "Unknown", "": "Unknown", "   ": "Unknown", "  54092  ": "Roku", " B091RFCS5V ": "Fire TV",
		"Google": "Unknown", "G": "Unknown", "GabcDEF": "Unknown", "9abc-def": "Unknown", "9test.app": "Unknown",
	}
	for in, want := range cases {
		if got := DetectCTVPlatform(in); got != want {
			t.Errorf("DetectCTVPlatform(%q) = %q, want %q", in, got, want)
		}
	}
}
