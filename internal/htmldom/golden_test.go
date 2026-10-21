// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package htmldom_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/ElcanoTek/lens/internal/htmldom"
	"github.com/ElcanoTek/lens/internal/scrape"
	"github.com/ElcanoTek/lens/internal/stores"
)

// testdata/golden.json was recorded from the fixtures while the extractors
// still parsed with BeautifulSoup 4.15.0 (html.parser backend). Every
// extractor is held to that output: the website scraper's title / meta
// description / text, the Play Store parse result, and each Play Store
// extractor on its own (so a fallback branch the full parse never reaches,
// because an earlier extractor already answered, is still pinned).

func androidRecord(a *stores.AndroidApp) map[string]any {
	if !a.Success {
		return map[string]any{
			"error": a.Error, "fetched_at": a.FetchedAt, "package_name": a.PackageName, "success": false,
		}
	}
	return map[string]any{
		"app_name":        a.AppName,
		"category":        a.Category,
		"content_for_llm": a.ContentForLLM,
		"content_rating":  a.ContentRating,
		"description":     a.Description,
		"developer":       a.Developer,
		"downloads":       a.Downloads,
		"fetched_at":      a.FetchedAt,
		"last_updated":    a.LastUpdated,
		"package_name":    a.PackageName,
		"platform":        a.Platform,
		"play_store_url":  a.PlayStoreURL,
		"rating":          a.Rating,
		"rating_count":    a.RatingCount,
		"success":         true,
	}
}

func extractEverything(html string) map[string]any {
	page := scrape.ParseHTML("https://example.test/", html)
	doc := func() *htmldom.Node { return htmldom.Parse(html) }
	rating, count := stores.ExtractRating(doc())
	return map[string]any{
		"site": map[string]any{
			"content": page.Content, "meta_description": page.MetaDescription, "title": page.Title, "url": page.URL,
		},
		"android":                 androidRecord(stores.ParsePlayStorePage("com.example.app", html, "2026-01-01T00:00:00")),
		"_extract_app_name":       stores.ExtractAppName(doc()),
		"_extract_developer":      stores.ExtractDeveloper(doc()),
		"_extract_category":       stores.ExtractCategory(doc()),
		"_extract_rating":         []any{rating, count},
		"_extract_downloads":      stores.ExtractDownloads(doc()),
		"_extract_description":    stores.ExtractDescription(doc()),
		"_extract_last_updated":   stores.ExtractLastUpdated(doc()),
		"_extract_content_rating": stores.ExtractContentRating(doc()),
	}
}

// normalise round-trips v through JSON so numbers compare as float64.
func normalise(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestExtractorsMatchTheRecordedOutput(t *testing.T) {
	raw, err := os.ReadFile("testdata/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden map[string]any
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	paths, _ := filepath.Glob("testdata/*.html")
	var names []string
	for _, p := range paths {
		names = append(names, filepath.Base(p))
	}
	var recorded []string
	for name := range golden {
		recorded = append(recorded, name)
	}
	sort.Strings(names)
	sort.Strings(recorded)
	if !reflect.DeepEqual(names, recorded) {
		t.Fatalf("fixtures %v, golden records %v", names, recorded)
	}
	for _, name := range names {
		html, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		got := normalise(t, extractEverything(string(html))).(map[string]any)
		want := golden[name].(map[string]any)
		for key, w := range want {
			if !reflect.DeepEqual(got[key], w) {
				gj, _ := json.MarshalIndent(got[key], "", " ")
				wj, _ := json.MarshalIndent(w, "", " ")
				t.Errorf("%s %s:\n got  %s\n want %s", name, key, gj, wj)
			}
		}
		for key := range got {
			if _, ok := want[key]; !ok {
				t.Errorf("%s: unexpected key %s", name, key)
			}
		}
	}
}

func TestEveryFixtureIsUTF8(t *testing.T) {
	paths, _ := filepath.Glob("testdata/*.html")
	for _, p := range paths {
		raw, _ := os.ReadFile(p)
		if strings.ToValidUTF8(string(raw), "") != string(raw) {
			t.Errorf("%s is not UTF-8", p)
		}
	}
}
