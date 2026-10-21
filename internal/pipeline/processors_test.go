// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ElcanoTek/lens/internal/categories"
	"github.com/ElcanoTek/lens/internal/config"
	"github.com/ElcanoTek/lens/internal/cost"
	"github.com/ElcanoTek/lens/internal/item"
	"github.com/ElcanoTek/lens/internal/progress"
	"github.com/ElcanoTek/lens/internal/report"
)

// ---- Bot_Protection (tests/test_bot_protection.py) ----

func TestDeriveBotProtection(t *testing.T) {
	combined := "Received HTTP status 403; research fallback: Research found no meaningful public information about this domain"
	cases := []struct {
		name                      string
		mode                      string
		failed                    bool
		current, prior, priorMode string
		want                      string
	}{
		{"direct success", "direct", false, "", "", "", "None Detected"},
		{"browser rescue after 403", "deep", false, "", "Scrape validation failed: Received HTTP status 403", "direct", "Moderate"},
		{"browser rescue after non-block failure", "deep", false, "", "Scraped content too short (101 chars < 500)", "direct", "None Detected"},
		{"research rescue after browser block", "research", false, "", "Potential block page detected ('just a moment')", "deep", "Aggressive"},
		{"research rescue after plain block", "research", false, "", "Received HTTP status 403", "direct", "Moderate"},
		{"research rescue without block evidence", "research", false, "", "Chrome scraping failed: timeout", "deep", "Unknown"},
		{"direct failure with 403", "direct", true, "Scrape validation failed: Received HTTP status 403", "", "", "Moderate"},
		{"browser failure with block", "deep", true, "Potential block page detected ('captcha')", "Received HTTP status 403", "direct", "Aggressive"},
		{"research failure echoing deep block", "research", true, combined, "Received HTTP status 403", "deep", "Aggressive"},
		{"research failure echoing direct block", "research", true, combined, "Received HTTP status 403", "direct", "Moderate"},
		{"failure without block evidence", "direct", true, "Chrome scraping failed: timeout", "", "", "Unknown"},
		{"prior mode missing reads as plain HTTP", "research", false, "", "Received HTTP status 429", "", "Moderate"},
		{"empty failure message is still a failure", "direct", true, "", "", "", "Unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DeriveBotProtection(tc.mode, tc.failed, tc.current, tc.prior, tc.priorMode); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func domainProcessor(t *testing.T, s *sink, llm SiteClassifier, scraper Scraper) (*DomainProcessor, *progress.Tracker) {
	t.Helper()
	tr := newTracker(t)
	return &DomainProcessor{Config: config.Default(), Progress: tr, Scraper: scraper, Classifier: llm, Writer: s.writer}, tr
}

func TestSuccessRowCarriesBotProtectionFromPriorPass(t *testing.T) {
	s := newSink(t, config.CSVFieldnames)
	p, tr := domainProcessor(t, s, newFakeLLM(), nil)
	if err := tr.Mark("blocked.com", progress.StatusRetryPending, nil, "Scrape validation failed: Received HTTP status 403", nil, "direct"); err != nil {
		t.Fatal(err)
	}
	cls := Classification{Success: true, Quality: "Standard", Justification: "j", Description: "d"}
	if err := p.recordSuccess(context.Background(), "blocked.com", cls, ScrapeResult{ContentLength: 1000, ScrapedAt: "2026-01-01T00:00:00"}, "deep", "openrouter", 1e9); err != nil {
		t.Fatal(err)
	}
	row := s.rows(t)[0]
	if row["Bot_Protection"] != "Moderate" || row["Quality"] != "Standard" || row["Processing_Time"] != "1.0" ||
		row["Content_Length"] != "1000" || row["Language"] != "Unknown" || row["Political_Leaning"] != "Non-Political" {
		t.Fatalf("row = %v", row)
	}
}

func TestFailureRowCarriesBotProtectionAndModeIsTracked(t *testing.T) {
	s := newSink(t, config.CSVFieldnames)
	p, tr := domainProcessor(t, s, newFakeLLM(), nil)
	if err := p.recordFailure(context.Background(), "fortress.com", "Scrape validation failed: Received HTTP status 403", "direct", "openrouter", 1e9, "2026-01-01T00:00:00", 0); err != nil {
		t.Fatal(err)
	}
	row := s.rows(t)[0]
	if row["Bot_Protection"] != "Moderate" || row["Quality"] != "Failed" || row["IAB Tier 1"] != "N/A" ||
		row["Justification"] != "Processing failed: Scrape validation failed: Received HTTP status 403" {
		t.Fatalf("row = %v", row)
	}
	msg, mode := tr.FailureDetails("fortress.com")
	if !strings.Contains(msg, "403") || mode != "direct" {
		t.Fatalf("details = %q %q", msg, mode)
	}
}

// ---- website processor (test_auto_mode / test_integrations) ----

func TestDomainProcessorDefersFailures(t *testing.T) {
	s := newSink(t, config.CSVFieldnames)
	scraper := &fakeScraper{results: map[string]ScrapeResult{"blocked.com": {Error: "HTTP 403"}}}
	p, tr := domainProcessor(t, s, newFakeLLM(), scraper)
	p.DeferFailures = true
	tr.SetTotal(1)
	p.ProcessDomain(context.Background(), "blocked.com")

	if got := tr.RetryPending(); !slices.Equal(got, []string{"blocked.com"}) {
		t.Fatalf("retry pending = %v", got)
	}
	sum := tr.Summary()
	if sum.Processed != 0 || sum.Retrying != 1 {
		t.Fatalf("summary = %+v", sum)
	}
	// The Failed row is still written; a successful retry replaces it via
	// the dedupe pass.
	if row := s.rows(t)[0]; row["Quality"] != "Failed" || row["Justification"] != "Processing failed: Scrape validation failed: HTTP 403" {
		t.Fatalf("row = %v", row)
	}
	if scraper.acquired != 1 || scraper.released != 1 {
		t.Fatalf("session acquire/release = %d/%d", scraper.acquired, scraper.released)
	}
}

func TestValidateScrapeResult(t *testing.T) {
	p := &DomainProcessor{Config: config.Default()}
	long := strings.Repeat("a", 600)
	cases := []struct {
		name string
		r    ScrapeResult
		want string
	}{
		{"reported failure", ScrapeResult{Error: "boom"}, "boom"},
		{"reported failure without message", ScrapeResult{}, "Scraper reported failure"},
		{"bad status", ScrapeResult{Success: true, StatusCode: 404, Content: long}, "Received HTTP status 404"},
		{"empty", ScrapeResult{Success: true, StatusCode: 200, Content: "   "}, "Scraper returned empty content"},
		{"too short", ScrapeResult{Success: true, StatusCode: 200, Content: strings.Repeat("a", 499), ContentLength: 499}, "Scraped content too short (499 chars < 500)"},
		{"length measured in characters", ScrapeResult{Success: true, Content: strings.Repeat("é", 499)}, "Scraped content too short (499 chars < 500)"},
		{"block page", ScrapeResult{Success: true, Content: "Just a moment... " + long}, "Potential block page detected ('just a moment')"},
		{"block phrase past 512 chars is content", ScrapeResult{Success: true, Content: long + " captcha"}, ""},
		{"ok", ScrapeResult{Success: true, StatusCode: 301, Content: long}, ""},
	}
	for _, tc := range cases {
		if got := p.validateScrapeResult(tc.r); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestProcessDomainSuccessRow(t *testing.T) {
	s := newSink(t, config.CSVFieldnames)
	content := strings.Repeat("Useful source text. ", 50)
	scraper := &fakeScraper{results: map[string]ScrapeResult{"example.com": {
		Success: true, Title: "Example", Content: content, ContentLength: utf8.RuneCountInString(content),
		StatusCode: 200, Mode: "direct", ScrapedAt: "2026-10-21T10:00:00.123456",
	}}}
	llm := newFakeLLM()
	p, tr := domainProcessor(t, s, llm, scraper)
	var out strings.Builder
	p.Reporter = report.New(report.Options{Total: 1, Out: &out})
	p.ProcessDomain(context.Background(), "example.com")

	row := s.rows(t)[0]
	want := map[string]string{
		"Domain": "example.com", "Type": "WEBSITE", "App_Name": "", "Quality": "Premium",
		"IAB Tier 1": "News and Politics", "Bot_Protection": "None Detected", "Content_Length": "1000",
		"Scrape_Mode": "direct", "Classifier_Mode": "openrouter", "Scraped_At": "2026-10-21T10:00:00.123456",
		"Cost_USD": "0.000000",
	}
	for k, v := range want {
		if row[k] != v {
			t.Errorf("%s = %q, want %q", k, row[k], v)
		}
	}
	if !tr.IsProcessed("example.com") {
		t.Fatal("not marked processed")
	}
	if llm.siteCalls[0].Title != "Example" || llm.siteCalls[0].ContentSource != "scrape" {
		t.Fatalf("classify call = %+v", llm.siteCalls[0])
	}
	if !strings.Contains(out.String(), "✅ Success example.com (200)") {
		t.Fatalf("reporter output = %q", out.String())
	}
}

func TestClassificationFailureIsRecorded(t *testing.T) {
	s := newSink(t, config.CSVFieldnames)
	content := strings.Repeat("x", 600)
	scraper := &fakeScraper{results: map[string]ScrapeResult{"a.com": {Success: true, Content: content}}}
	llm := newFakeLLM()
	llm.classifySite = func(SiteInput) (Classification, error) { return Classification{Error: "classifier down"}, nil }
	p, tr := domainProcessor(t, s, llm, scraper)
	p.ProcessDomain(context.Background(), "a.com")
	if row := s.rows(t)[0]; row["Justification"] != "Processing failed: Classification failed: classifier down" {
		t.Fatalf("row = %v", row)
	}
	if !slices.Equal(tr.Failed(), []string{"a.com"}) {
		t.Fatal("not failed")
	}
}

func TestCancelledItemRecordsNothing(t *testing.T) {
	s := newSink(t, config.CSVFieldnames)
	ctx, cancel := context.WithCancel(context.Background())
	scraper := &fakeScraper{scrape: func(string) ScrapeResult {
		cancel()
		return ScrapeResult{Error: "context canceled"}
	}}
	p, tr := domainProcessor(t, s, newFakeLLM(), scraper)
	p.ProcessDomain(ctx, "a.com")
	if s.buf.Len() != 0 || len(tr.Processed()) != 0 {
		t.Fatalf("a cancelled item was recorded: %q", s.buf.String())
	}
}

// ---- research rung (tests/test_research_fallback.py) ----

func TestResearchSuccessRecordsResearchScrapeMode(t *testing.T) {
	s := newSink(t, config.CSVFieldnames)
	llm := newFakeLLM()
	p, tr := domainProcessor(t, s, llm, nil)
	p.ProcessDomainResearch(context.Background(), "reuters.com")
	if len(llm.siteCalls) != 1 || llm.siteCalls[0].ContentSource != "research" || llm.siteCalls[0].Domain != "reuters.com" {
		t.Fatalf("classify calls = %+v", llm.siteCalls)
	}
	if llm.researchOpts[0].Model != "perplexity/sonar" || llm.researchOpts[0].MaxTokens != 1500 || llm.researchOpts[0].CustomQuestions != nil {
		t.Fatalf("research options = %+v", llm.researchOpts[0])
	}
	row := s.rows(t)[0]
	if row["Quality"] != "Premium" || row["Scrape_Mode"] != "research" || row["Bot_Protection"] != "Unknown" ||
		row["Content_Length"] != "32" {
		t.Fatalf("row = %v", row)
	}
	if !tr.IsProcessed("reuters.com") || len(tr.Failed()) != 0 {
		t.Fatal("not processed")
	}
}

func TestResearchInsufficientKeepsDomainFailed(t *testing.T) {
	s := newSink(t, config.CSVFieldnames)
	llm := newFakeLLM()
	llm.researchSite = func(string, ResearchOptions) (Research, error) { return Research{Success: true}, nil }
	p, tr := domainProcessor(t, s, llm, nil)
	if err := tr.Mark("unknownsite.example", progress.StatusRetryPending, nil, "Received HTTP status 403", nil, "direct"); err != nil {
		t.Fatal(err)
	}
	p.ProcessDomainResearch(context.Background(), "unknownsite.example")
	if len(llm.siteCalls) != 0 {
		t.Fatal("classified without evidence")
	}
	row := s.rows(t)[0]
	if row["Quality"] != "Failed" || !strings.Contains(row["Justification"], "403") ||
		row["Justification"] != "Processing failed: Received HTTP status 403; research fallback: Research found no meaningful public information about this domain" ||
		row["Bot_Protection"] != "Moderate" || row["Scrape_Mode"] != "research" {
		t.Fatalf("row = %v", row)
	}
	if !slices.Contains(tr.Failed(), "unknownsite.example") {
		t.Fatal("not failed")
	}
}

func TestResearchErrorKeepsDomainFailed(t *testing.T) {
	for _, research := range []func(string, ResearchOptions) (Research, error){
		func(string, ResearchOptions) (Research, error) { return Research{Error: "boom"}, nil },
		func(string, ResearchOptions) (Research, error) { return Research{}, errors.New("transport down") },
	} {
		s := newSink(t, config.CSVFieldnames)
		llm := newFakeLLM()
		llm.researchSite = research
		p, tr := domainProcessor(t, s, llm, nil)
		p.ProcessDomainResearch(context.Background(), "example.com")
		if !slices.Contains(tr.Failed(), "example.com") {
			t.Fatal("not failed")
		}
	}
}

// ---- app processor ----

func TestAppProcessorRows(t *testing.T) {
	rating, count := 4.5, 1234
	for _, platform := range []string{"ios", "android"} {
		s := newSink(t, config.CSVFieldnames)
		store := &fakeStore{meta: AppMetadata{
			Success: true, AppName: "Demo", Developer: "Dev", PrimaryGenre: "Games", Category: "GAME_PUZZLE",
			Rating: &rating, RatingCount: &count, Downloads: "1,000,000+", ContentForLLM: "Store text",
			FetchedAt: "2026-10-21T10:00:00",
		}}
		llm := newFakeLLM()
		tr := newTracker(t)
		p := &AppProcessor{Progress: tr, IOS: store, Android: store, Classifier: llm, Writer: s.writer}
		id := "123456789"
		it := item.WorkItem{Identifier: id, Type: item.IOSApp}
		if platform == "android" {
			id = "com.demo.app"
			it = item.WorkItem{Identifier: id, Type: item.AndroidApp}
		}
		if err := p.ProcessApp(context.Background(), it); err != nil {
			t.Fatal(err)
		}
		row := s.rows(t)[0]
		wantType, wantMode, wantDownloads := "IOS", "ios_api", ""
		if platform == "android" {
			wantType, wantMode, wantDownloads = "ANDROID", "android_scrape", "1,000,000+"
		}
		if row["Type"] != wantType || row["Scrape_Mode"] != wantMode || row["Downloads"] != wantDownloads ||
			row["Rating"] != "4.5" || row["Rating_Count"] != "1234" || row["App_Name"] != "Demo" ||
			row["Store_Category"] != "Games" || row["Content_Length"] != "10" || row["Bot_Protection"] != "" {
			t.Fatalf("%s row = %v", platform, row)
		}
		if llm.appCalls[0].Platform != platform || llm.appCalls[0].RatingCount != 1234 {
			t.Fatalf("classify call = %+v", llm.appCalls[0])
		}
		if !tr.IsProcessed(id) {
			t.Fatal("not processed")
		}
	}
}

func TestAppProcessorFailures(t *testing.T) {
	s := newSink(t, config.CSVFieldnames)
	tr := newTracker(t)
	p := &AppProcessor{Progress: tr, Classifier: newFakeLLM(), Writer: s.writer,
		Android: &fakeStore{meta: AppMetadata{Error: "Blocked by Google Play Store"}}}
	p.ProcessIOSApp(context.Background(), "123")
	p.ProcessAndroidApp(context.Background(), "com.a.b")
	rows := s.rows(t)
	if rows[0]["Justification"] != "Processing failed: iOS client not initialized" ||
		rows[1]["Justification"] != "Processing failed: Failed to fetch Android app data: Blocked by Google Play Store" ||
		rows[1]["Rating"] != "" || rows[1]["Content_Length"] != "0" {
		t.Fatalf("rows = %v", rows)
	}
	if err := p.ProcessApp(context.Background(), item.WorkItem{Identifier: "x.com", Type: item.Website}); err == nil {
		t.Fatal("website routed to the app processor")
	}
	if len(tr.Failed()) != 2 {
		t.Fatalf("failed = %v", tr.Failed())
	}
}

// ---- CTV processor (tests/test_ctv_functionality.py) ----

func ctvProcessor(t *testing.T, s *sink, llm CTVClassifier) (*CTVProcessor, *progress.Tracker) {
	t.Helper()
	tr := newTracker(t)
	return &CTVProcessor{
		Progress: tr, Client: llm, Writer: s.writer,
		ResearchModel: "perplexity/sonar", ResearchTemperature: 0.3, ResearchMaxTokens: 2000,
		ClassificationModel: "~openai/gpt-luna-latest", ClassificationTemperature: 0.1, ClassificationMaxTokens: 1500,
	}, tr
}

func TestProcessCTVAppSuccess(t *testing.T) {
	s := newSink(t, config.CTVCSVFieldnames)
	llm := newFakeLLM()
	llm.researchCTV = func(CTVResearchInput) (Research, error) {
		return Research{Success: true, ResearchContent: "## Netflix\n**Netflix** is a major streaming service[1]."}, nil
	}
	p, tr := ctvProcessor(t, s, llm)
	var out strings.Builder
	p.Reporter = report.New(report.Options{Total: 1, Out: &out})
	p.ProcessCTVApp(context.Background(), item.CTVWorkItem{AppName: "Netflix", BundleID: "54092", Publisher: "Netflix Inc", SSP: "X"})

	if len(llm.ctvResearch) != 1 || len(llm.ctvClassify) != 1 {
		t.Fatal("both steps must run once")
	}
	// The platform detected from the bundle ID reaches both steps.
	if llm.ctvResearch[0].Platform != "Roku" || llm.ctvClassify[0].Platform != "Roku" ||
		llm.ctvResearch[0].Publisher != "Netflix Inc" || llm.ctvClassify[0].Publisher != "Netflix Inc" ||
		llm.ctvResearch[0].Model != "perplexity/sonar" || llm.ctvClassify[0].MaxTokens != 1500 {
		t.Fatalf("calls = %+v %+v", llm.ctvResearch[0], llm.ctvClassify[0])
	}
	row := s.rows(t)[0]
	if row["Type"] != "CTV" || row["Platform"] != "Roku" || row["Quality"] != "Premium" || row["SSP"] != "X" ||
		row["Research_Summary"] != "Netflix\nNetflix is a major streaming service." || row["Content_Type"] != "On-Demand Streaming" ||
		row["Classification_Model"] != "~openai/gpt-luna-latest" {
		t.Fatalf("row = %v", row)
	}
	if !tr.IsProcessed("54092") {
		t.Fatal("not processed under the bundle ID")
	}
	if !strings.Contains(out.String(), "✅ Success CTV:Netflix (200)") {
		t.Fatalf("reporter output = %q", out.String())
	}
}

func TestProcessCTVAppResearchFailure(t *testing.T) {
	s := newSink(t, config.CTVCSVFieldnames)
	llm := newFakeLLM()
	llm.researchCTV = func(CTVResearchInput) (Research, error) {
		return Research{Error: "API rate limit exceeded"}, nil
	}
	p, tr := ctvProcessor(t, s, llm)
	p.ProcessCTVApp(context.Background(), item.CTVWorkItem{AppName: "Netflix", Platform: "Roku"})
	if len(llm.ctvClassify) != 0 {
		t.Fatal("classified after a research failure")
	}
	row := s.rows(t)[0]
	if row["Quality"] != "Failed" || row["Justification"] != "Research failed: Research failed: API rate limit exceeded" ||
		row["Target_Audience"] != "N/A" || row["Platform"] != "Roku" {
		t.Fatalf("row = %v", row)
	}
	if !slices.Equal(tr.Failed(), []string{"Netflix"}) {
		t.Fatal("not failed under the app name")
	}
}

func TestProcessCTVAppClassificationFailure(t *testing.T) {
	s := newSink(t, config.CTVCSVFieldnames)
	llm := newFakeLLM()
	llm.classifyCTV = func(CTVClassifyInput) (Classification, error) { return Classification{Error: "Model timeout"}, nil }
	p, _ := ctvProcessor(t, s, llm)
	p.ProcessCTVApp(context.Background(), item.CTVWorkItem{AppName: "Netflix", Platform: "Roku"})
	row := s.rows(t)[0]
	if row["Quality"] != "Failed" || !strings.HasPrefix(row["Justification"], "Classification failed") ||
		row["Research_Summary"] != "Netflix is a major streaming service." {
		t.Fatalf("row = %v", row)
	}
}

func TestCTVMissingResearchNeverClassifies(t *testing.T) {
	spec := testSpec(t)
	for _, evidence := range []string{"", "   ", "INSUFFICIENT INFORMATION", "  insufficient information\n"} {
		s := newSink(t, append(slices.Clone(config.CTVCSVFieldnames), categories.Columns(spec)...))
		llm := newFakeLLM()
		llm.researchCTV = func(CTVResearchInput) (Research, error) {
			return Research{Success: true, ResearchContent: evidence}, nil
		}
		custom := &fakeCategories{}
		p, _ := ctvProcessor(t, s, llm)
		p.Categories = custom
		p.ProcessCTVApp(context.Background(), item.CTVWorkItem{AppName: "Unknown TV"})
		if len(llm.ctvClassify) != 0 || len(custom.calls) != 0 {
			t.Fatalf("%q: classified without evidence", evidence)
		}
		row := s.rows(t)[0]
		if row["Decision_Status"] != "skipped" || row["Quality"] != "Failed" || !strings.Contains(row["Justification"], "No meaningful research evidence") {
			t.Fatalf("%q: row = %v", evidence, row)
		}
	}
}

func TestCTVRateLimitSpacesRequests(t *testing.T) {
	s := newSink(t, config.CTVCSVFieldnames)
	p, _ := ctvProcessor(t, s, newFakeLLM())
	p.RequestDelay = 20e6 // 20ms
	start := time.Now()
	p.ProcessCTVApp(context.Background(), item.CTVWorkItem{AppName: "A"})
	p.ProcessCTVApp(context.Background(), item.CTVWorkItem{AppName: "B"})
	// Four requests; three gaps.
	if elapsed := time.Since(start); elapsed < 60e6 {
		t.Fatalf("requests not spaced: %v", elapsed)
	}
}

func TestDetectCTVPlatform(t *testing.T) {
	cases := map[string]string{
		"54092": "Roku", "999999": "Roku", "B091RFCS5V": "Fire TV", "b0lowercase": "Fire TV",
		"com.foxsports.videogo": "Android TV", "COM.UPPERCASE.APP": "Android TV",
		"vizio.app.streaming": "Vizio", "VIZIO.UPPERCASE": "Vizio", "tv.streaming.app": "Apple TV",
		"TV.UPPERCASE.APP": "Apple TV", "G00009197740": "Samsung TV", "g99999": "Samsung TV",
		"9wzdncrfjv7w": "Xbox", "9ABCDEF123": "Xbox", "crunchyroll.roku": "Roku",
		"xbox.crunchyroll": "Xbox", "playstation.crunchyroll": "PlayStation", "randomstring": "Unknown",
		"abc123xyz": "Unknown", "netflix": "Unknown", "": "Unknown", "   ": "Unknown",
		"  54092  ": "Roku", " B091RFCS5V ": "Fire TV", "  com.test.app  ": "Android TV",
		"Google": "Unknown", "G": "Unknown", "GabcDEF": "Unknown", "9abc-def": "Unknown", "9test.app": "Unknown",
	}
	for in, want := range cases {
		if got := DetectCTVPlatform(in); got != want {
			t.Errorf("DetectCTVPlatform(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStripMarkdown(t *testing.T) {
	cases := map[string]string{
		"":                             "",
		"# Header 1":                   "Header 1",
		"#### Header 4":                "Header 4",
		"**bold text**":                "bold text",
		"__bold text__":                "bold text",
		"Some **bold** word":           "Some bold word",
		"*italic text*":                "italic text",
		"_italic text_":                "italic text",
		"Some *italic* word":           "Some italic word",
		"`code`":                       "code",
		"Use `print()` function":       "Use print() function",
		"[Google](https://google.com)": "Google",
		"Visit [our site](https://example.com) today":     "Visit our site today",
		"[Link](url) and [Another](url2)":                 "Link and Another",
		"Text with citation[1]":                           "Text with citation",
		"Multiple[1] citations[2] here[3]":                "Multiple citations here",
		"Reference [12] number":                           "Reference  number",
		"Line 1\n\n\n\nLine 2":                            "Line 1\n\nLine 2",
		"A\n\n\n\n\nB\n\n\n\nC":                           "A\n\nB\n\nC",
		"This is plain text with no Markdown formatting.": "This is plain text with no Markdown formatting.",
		"a ***b*** c":                                     "a b c",
		"snake_case_name":                                 "snakecasename",
		"2 * 3 * 4":                                       "2  3  4",
		"  indented\n\tline":                              "indented\nline",
	}
	for in, want := range cases {
		if got := StripMarkdown(in); got != want {
			t.Errorf("StripMarkdown(%q) = %q, want %q", in, got, want)
		}
	}
	combined := "# Research Summary\n\n**Netflix** is a major streaming platform[1]. It offers *premium content* and has a\n" +
		"[comprehensive library](https://netflix.com) of movies and TV shows.\n\n## Key Features\n\n" +
		"The service includes `4K streaming` and multiple profiles[2][3].\n"
	got := StripMarkdown(combined)
	for _, gone := range []string{"# Research", "## Key", "**", "*premium", "[comprehensive library]", "[1]", "[2]", "[3]", "`"} {
		if strings.Contains(got, gone) {
			t.Errorf("%q still contains %q", got, gone)
		}
	}
	for _, kept := range []string{"Netflix", "premium content", "comprehensive library", "4K streaming"} {
		if !strings.Contains(got, kept) {
			t.Errorf("%q lost %q", got, kept)
		}
	}
}

func TestTruncateResearch(t *testing.T) {
	if got := TruncateResearch("This is short text.", 1000); got != "This is short text." {
		t.Fatal(got)
	}
	got := TruncateResearch(strings.Repeat("First sentence. ", 50)+strings.Repeat("Second sentence. ", 50), 100)
	if !strings.HasSuffix(got, ".") || strings.Contains(got, "...") || len(got) > 100 {
		t.Fatalf("sentence boundary: %q", got)
	}
	got = TruncateResearch("Short. Medium sentence here. This is a much longer sentence that goes on and on. Final.", 60)
	if !strings.HasSuffix(got, ".") {
		t.Fatalf("last sentence: %q", got)
	}
	if got := TruncateResearch(strings.Repeat("A", 1500), 1000); !strings.HasSuffix(got, "...") || utf8.RuneCountInString(got) != 1003 {
		t.Fatalf("ellipsis: %d", len(got))
	}
	if got := TruncateResearch(strings.Repeat("Wow! ", 100), 100); !strings.HasSuffix(got, "!") {
		t.Fatalf("exclamation: %q", got)
	}
	if got := TruncateResearch(strings.Repeat("How does this work? ", 100), 100); !strings.HasSuffix(got, "?") {
		t.Fatalf("question: %q", got)
	}
	// A boundary in the first half is not worth keeping.
	if got := TruncateResearch("Hi. "+strings.Repeat("x", 200), 100); got != "Hi. "+strings.Repeat("x", 96)+"..." {
		t.Fatalf("early boundary: %q", got)
	}
	// Characters, not bytes.
	if got := TruncateResearch(strings.Repeat("é", 20), 10); got != strings.Repeat("é", 10)+"..." {
		t.Fatalf("runes: %q", got)
	}
}

// ---- custom categories in every pipeline (tests/test_custom_categories.py) ----

func testSpec(t *testing.T) *categories.Spec {
	t.Helper()
	spec, err := categories.Parse(`{"categories": [
		{"name": "Brand safe", "type": "boolean", "question": "Would a mainstream brand be comfortable appearing beside this content?"},
		{"name": "Purpose", "type": "choice", "question": "What is its primary purpose?", "options": ["Education", "Entertainment", "Unknown"]}
	]}`)
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

func decisionSuccess() categories.Result {
	return categories.Result{
		"Custom: Brand safe": "No", "P(yes/choice): Brand safe": 0.1,
		"Custom: Purpose": "Education", "P(yes/choice): Purpose": 0.95, "Confidence: Purpose": 0.9,
		"P: Purpose / Education": 0.95, "P: Purpose / Entertainment": 0.03, "P: Purpose / Unknown": 0.02,
		"Decision_Status": "success", "Decision_Error": "", "Decision_Model": "typesafe/jev-1.13-20260917",
		"Decision_Provider": "TypeSafe", "Decision_Answers": `{"c0": {"type": "noul", "noul": 0.1}}`,
	}
}

func TestAllPipelinesUseOriginalEvidenceAndKeepPrimaryResults(t *testing.T) {
	spec := testSpec(t)
	questions := []string{"q1", "q2 Options: a, b"}
	for _, kind := range []string{"website", "research", "ios", "android", "ctv"} {
		for _, customError := range []bool{false, true} {
			columns := append(slices.Clone(config.CSVFieldnames), categories.Columns(spec)...)
			if kind == "ctv" {
				columns = append(slices.Clone(config.CTVCSVFieldnames), categories.Columns(spec)...)
			}
			s := newSink(t, columns)
			custom := &fakeCategories{questions: questions, result: decisionSuccess()}
			if customError {
				custom.result = categories.Result{"Decision_Status": "error", "Decision_Error": "OpenRouter HTTP 401"}
			}
			evidence := strings.Repeat("Original source evidence. ", 30)
			if kind == "research" || kind == "ctv" {
				evidence = "INSUFFICIENT INFORMATION about one custom question. " + evidence
			}
			llm := newFakeLLM()
			llm.researchSite = func(string, ResearchOptions) (Research, error) {
				return Research{Success: true, ResearchContent: evidence}, nil
			}
			llm.researchCTV = func(CTVResearchInput) (Research, error) {
				return Research{Success: true, ResearchContent: evidence}, nil
			}
			tr := newTracker(t)
			ctx := context.Background()
			switch kind {
			case "website", "research":
				scraper := &fakeScraper{results: map[string]ScrapeResult{"example.com": {Success: true, Content: evidence, Mode: "direct"}}}
				p := &DomainProcessor{Config: config.Default(), Progress: tr, Scraper: scraper, Classifier: llm, Writer: s.writer, Categories: custom}
				if kind == "website" {
					p.ProcessDomain(ctx, "example.com")
				} else {
					p.ProcessDomainResearch(ctx, "example.com")
				}
			case "ctv":
				p, _ := ctvProcessor(t, s, llm)
				p.Progress, p.Categories = tr, custom
				p.ProcessCTVApp(ctx, item.CTVWorkItem{AppName: "Demo TV", Publisher: "Demo Network"})
				if llm.ctvResearch[0].Publisher != "Demo Network" || llm.ctvClassify[0].Publisher != "Demo Network" {
					t.Fatal("publisher not passed to both CTV steps")
				}
			default:
				store := &fakeStore{meta: AppMetadata{Success: true, ContentForLLM: evidence, AppName: "Demo"}}
				p := &AppProcessor{Progress: tr, IOS: store, Android: store, Classifier: llm, Writer: s.writer, Categories: custom}
				it := item.WorkItem{Identifier: "123", Type: item.IOSApp}
				if kind == "android" {
					it = item.WorkItem{Identifier: "com.demo.app", Type: item.AndroidApp}
				}
				if err := p.ProcessApp(ctx, it); err != nil {
					t.Fatal(err)
				}
			}
			rows := s.rows(t)
			wantStatus := "success"
			if customError {
				wantStatus = "error"
			}
			if len(rows) != 1 || rows[0]["Quality"] != "Premium" || rows[0]["Decision_Status"] != wantStatus {
				t.Fatalf("%s/%v: rows = %v", kind, customError, rows)
			}
			if !customError && (rows[0]["Custom: Purpose"] != "Education" || rows[0]["P: Purpose / Entertainment"] != "0.03") {
				t.Fatalf("%s: custom cells = %v", kind, rows[0])
			}
			if len(custom.calls) != 1 || custom.calls[0].content != evidence {
				t.Fatalf("%s: custom classify calls = %+v", kind, custom.calls)
			}
			wantSource := map[string]string{"website": "direct", "research": "research", "ctv": "ctv_research", "ios": "ios_store", "android": "android_store"}[kind]
			if custom.calls[0].source != wantSource {
				t.Fatalf("%s: source = %q", kind, custom.calls[0].source)
			}
			if tr.Summary().Successful != 1 {
				t.Fatalf("%s: not successful", kind)
			}
			switch kind {
			case "research":
				if !slices.Equal(llm.researchOpts[0].CustomQuestions, questions) {
					t.Fatal("custom questions did not reach website research")
				}
			case "ctv":
				if !slices.Equal(llm.ctvResearch[0].CustomQuestions, questions) {
					t.Fatal("custom questions did not reach CTV research")
				}
			}
		}
	}
}

func TestFailedPrimaryAnalysisHasNoCustomCall(t *testing.T) {
	spec := testSpec(t)
	s := newSink(t, append(slices.Clone(config.CSVFieldnames), categories.Columns(spec)...))
	llm := newFakeLLM()
	llm.researchSite = func(string, ResearchOptions) (Research, error) { return Research{Error: "No evidence"}, nil }
	custom := &fakeCategories{}
	p := &DomainProcessor{Config: config.Default(), Progress: newTracker(t), Classifier: llm, Writer: s.writer, Categories: custom}
	p.ProcessDomainResearch(context.Background(), "example.com")
	if len(custom.calls) != 0 {
		t.Fatal("custom categories evaluated after a failed primary analysis")
	}
	row := s.rows(t)[0]
	if row["Decision_Status"] != "skipped" || row["Decision_Error"] != "Primary analysis failed; custom categories not evaluated" || row["Custom: Brand safe"] != "" {
		t.Fatalf("row = %v", row)
	}
}

// ---- cost (tests/test_cost_tracking.py) ----

func TestRowsCarryTheItemCost(t *testing.T) {
	s := newSink(t, []string{"Domain", "Cost_USD"})
	run := cost.NewMeter()
	ctx := cost.WithItem(cost.WithRun(context.Background(), run))
	c := 0.0123
	cost.Record(ctx, &c, "openai/gpt-6-luna")
	rec := NewRecord()
	rec.Set("Domain", "example.com")
	if err := writeResult(ctx, s.writer, rec, false); err != nil {
		t.Fatal(err)
	}
	if got := s.buf.String(); got != "example.com,0.012300\r\n" {
		t.Fatalf("row = %q", got)
	}
	if !slices.Contains(config.CSVFieldnames, "Cost_USD") || !slices.Contains(config.CTVCSVFieldnames, "Cost_USD") {
		t.Fatal("Cost_USD missing from a schema")
	}
}

func TestProcessorsOpenTheirOwnItemMeter(t *testing.T) {
	s := newSink(t, config.CSVFieldnames)
	run := cost.NewMeter()
	ctx := cost.WithRun(context.Background(), run)
	llm := newFakeLLM()
	llm.researchSite = func(string, ResearchOptions) (Research, error) {
		return Research{Success: true, ResearchContent: "Plenty of research text."}, nil
	}
	p := &DomainProcessor{Config: config.Default(), Progress: newTracker(t), Classifier: costingLLM{fakeLLM: llm, ctxCost: 0.25}, Writer: s.writer}
	p.ProcessDomainResearch(ctx, "a.com")
	p.ProcessDomainResearch(ctx, "b.com")
	rows := s.rows(t)
	// Two billed calls per item (research + classify), each item alone.
	if rows[0]["Cost_USD"] != "0.500000" || rows[1]["Cost_USD"] != "0.500000" || run.USD() != 1.0 {
		t.Fatalf("rows = %v, run = %v", rows, run.USD())
	}
}

// costingLLM records a fixed cost per call, like the real client does.
type costingLLM struct {
	*fakeLLM
	ctxCost float64
}

func (c costingLLM) ClassifySite(ctx context.Context, in SiteInput) (Classification, error) {
	v := c.ctxCost
	cost.Record(ctx, &v, "m")
	return c.fakeLLM.ClassifySite(ctx, in)
}

func (c costingLLM) ResearchWebsite(ctx context.Context, d string, o ResearchOptions) (Research, error) {
	v := c.ctxCost
	cost.Record(ctx, &v, "m")
	return c.fakeLLM.ResearchWebsite(ctx, d, o)
}

// ---- record / CSV formatting ----

func TestCSVRowsMatchPythonCSVModule(t *testing.T) {
	s := newSink(t, []string{"a", "b", "c", "d", "e", "f"})
	rec := NewRecord()
	rec.Set("a", "plain")
	rec.Set("b", " leading space")
	rec.Set("c", `has "quotes", and comma`)
	rec.Set("d", "two\nlines")
	rec.Set("e", 0.5)
	rec.Set("f", 12)
	if err := s.writer.WriteRow(rec); err != nil {
		t.Fatal(err)
	}
	want := "plain, leading space,\"has \"\"quotes\"\", and comma\",\"two\nlines\",0.5,12\r\n"
	if got := s.buf.String(); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	one := newSink(t, []string{"x"})
	if err := one.writer.WriteRow(NewRecord()); err != nil {
		t.Fatal(err)
	}
	if one.buf.String() != "\"\"\r\n" {
		t.Fatalf("single empty field = %q", one.buf.String())
	}
}

func TestFormattingMatchesPython(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{{1.0, "1.0"}, {0.004, "0.0"}, {0.005, "0.01"}, {1.234, "1.23"}, {12.5, "12.5"}, {2.675, "2.67"}}
	for _, c := range cases {
		if got := FormatCell(pyRound2(c.in)); got != c.want {
			t.Errorf("round(%v, 2) = %q, want %q", c.in, got, c.want)
		}
	}
	if FormatCell(nil) != "" || FormatCell(int64(1)) != "1" || FormatCell(true) != "True" {
		t.Fatal("cell formatting")
	}
	rec := NewRecord()
	rec.Set("Domain", "x.com")
	rec.Set("Processing_Time", 1.0)
	rec.Set("Content_Length", 3)
	rec.Set("Decision_Status", "success")
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"Domain":"x.com","Processing_Time":1.0,"Content_Length":3,"Decision_Status":"success"}` {
		t.Fatalf("json = %s", raw)
	}
}
