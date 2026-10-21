// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package detect

import (
	"reflect"
	"testing"

	"github.com/ElcanoTek/lens/internal/tabular"
)

// Ported from tests/test_ctv_functionality.py (CTVProcessor._detect_ctv_platform).
func TestCTVPlatform(t *testing.T) {
	cases := map[string]string{
		"54092": "Roku", "12345": "Roku", "999999": "Roku",
		"B091RFCS5V": "Fire TV", "B0ABCD1234": "Fire TV", "b0lowercase": "Fire TV",
		"com.foxsports.videogo": "Android TV", "com.netflix.app": "Android TV", "COM.UPPERCASE.APP": "Android TV",
		"vizio.app.streaming": "Vizio", "VIZIO.UPPERCASE": "Vizio",
		"tv.streaming.app": "Apple TV", "TV.UPPERCASE.APP": "Apple TV",
		"G00009197740": "Samsung TV", "G12345": "Samsung TV", "g99999": "Samsung TV",
		"9wzdncrfjv7w": "Xbox", "9ABCDEF123": "Xbox",
		"crunchyroll.roku": "Roku", "xbox.crunchyroll": "Xbox", "playstation.crunchyroll": "PlayStation",
		"randomstring": "Unknown", "abc123xyz": "Unknown", "netflix": "Unknown",
		"": "Unknown", "   ": "Unknown",
		"  54092  ": "Roku", " B091RFCS5V ": "Fire TV", "  com.test.app  ": "Android TV",
		"Google": "Unknown", "G": "Unknown", "GabcDEF": "Unknown",
		"9abc-def": "Unknown", "9test.app": "Unknown",
	}
	for in, want := range cases {
		if got := CTVPlatform(in); got != want {
			t.Errorf("CTVPlatform(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsCTVInputFile(t *testing.T) {
	cases := []struct {
		cols []string
		want bool
	}{
		{[]string{"app_name", "bundle_id", "platform"}, true},
		{[]string{"bundle_id", "name"}, true},
		{[]string{"ctv_channel", "viewers"}, true},
		{[]string{"channel_name", "network"}, true},
		{[]string{"domain", "category"}, false},
		{[]string{"App Bundle"}, true},
		{[]string{"app_bundle"}, true},
		{[]string{"pageURL", "status"}, false},
		{[]string{"SSP", "Publisher", "App, Account or Network Name", "Bundle ID"}, true},
		{[]string{"Connected Devices"}, true},
		{[]string{"App Name", "Publisher"}, false}, // too generic on their own
	}
	for _, tc := range cases {
		if got := IsCTVInputFile(tabular.New(tc.cols, nil)); got != tc.want {
			t.Errorf("IsCTVInputFile(%q) = %v", tc.cols, got)
		}
	}
}

func TestCTVColumns(t *testing.T) {
	cases := []struct {
		cols []string
		want CTVFieldColumns
	}{
		{[]string{"app_name", "bundle_id", "platform", "url"}, CTVFieldColumns{AppName: "app_name", BundleID: "bundle_id", Platform: "platform", URL: "url"}},
		{[]string{"channel_name", "roku_id", "device", "website"}, CTVFieldColumns{AppName: "channel_name", BundleID: "roku_id", Platform: "device", URL: "website"}},
		{[]string{"App_Name", "BUNDLE_ID", "Platform"}, CTVFieldColumns{AppName: "App_Name", BundleID: "BUNDLE_ID", Platform: "Platform"}},
		{[]string{"app_name", "random_column"}, CTVFieldColumns{AppName: "app_name"}},
		{[]string{"SSP", "Publisher", "App, Account or Network Name", "Bundle ID"},
			CTVFieldColumns{AppName: "App, Account or Network Name", BundleID: "Bundle ID", SSP: "SSP", Publisher: "Publisher"}},
		{[]string{"App Bundle"}, CTVFieldColumns{BundleID: "App Bundle"}},
		// Partial app/name match, but never an id column.
		{[]string{"App ID Name", "My App Title Name"}, CTVFieldColumns{AppName: "My App Title Name"}},
	}
	for _, tc := range cases {
		if got := CTVColumns(tabular.New(tc.cols, nil)); got != tc.want {
			t.Errorf("CTVColumns(%q) = %+v, want %+v", tc.cols, got, tc.want)
		}
	}
}

func TestParseCTVWorkItems(t *testing.T) {
	// basic
	items := ParseCTVWorkItems(frame([]string{"app_name", "platform"}, []string{"Netflix", "Hulu", "Disney+"}, []string{"Roku", "Fire TV", "Apple TV"}))
	if len(items) != 3 || items[0].AppName != "Netflix" || items[0].Platform != "Roku" || items[1].AppName != "Hulu" || items[2].AppName != "Disney+" {
		t.Errorf("basic: %+v", items)
	}
	// full
	items = ParseCTVWorkItems(frame([]string{"app_name", "bundle_id", "platform", "url"},
		[]string{"ESPN"}, []string{"com.espn.score"}, []string{"Roku"}, []string{"https://espn.com"}))
	if len(items) != 1 || items[0].BundleID != "com.espn.score" || items[0].Platform != "Roku" || items[0].URL != "https://espn.com" {
		t.Errorf("full: %+v", items)
	}
	if want := map[string]string{"app_name": "ESPN", "bundle_id": "com.espn.score", "platform": "Roku", "url": "https://espn.com"}; !reflect.DeepEqual(items[0].OriginalRow, want) {
		t.Errorf("original row: %v", items[0].OriginalRow)
	}
	// empty app names skipped
	items = ParseCTVWorkItems(frame([]string{"app_name", "platform"}, []string{"Netflix", "", "   ", "Hulu"}, []string{"Roku", "Fire TV", "Apple TV", "Roku"}))
	if len(items) != 2 || items[0].AppName != "Netflix" || items[1].AppName != "Hulu" {
		t.Errorf("skip empty: %+v", items)
	}
	// no app_name column: "name" is an exact candidate
	items = ParseCTVWorkItems(frame([]string{"name", "platform"}, []string{"Netflix", "Hulu"}, []string{"Roku", "Fire TV"}))
	if len(items) != 2 || items[0].AppName != "Netflix" {
		t.Errorf("name column: %+v", items)
	}
	// truly no app name column: first column
	items = ParseCTVWorkItems(frame([]string{"title", "platform"}, []string{"Netflix"}, []string{"Roku"}))
	if len(items) != 1 || items[0].AppName != "Netflix" {
		t.Errorf("first column: %+v", items)
	}
	if ParseCTVWorkItems(tabular.New(nil, nil)) != nil {
		t.Error("no columns should give nil")
	}
	// one-column bundle export: ids kept, ".0" dropped, blanks skipped, platform named
	items = ParseCTVWorkItems(frame([]string{"App Bundle"}, []string{"54092", "54092.0", "", "B091RFCS5V", "com.plexapp.ctv", "crunchyroll.roku"}))
	bundles, platforms := make([]string, 0, len(items)), make([]string, 0, len(items))
	for _, it := range items {
		bundles = append(bundles, it.BundleID)
		platforms = append(platforms, it.Platform)
	}
	if !reflect.DeepEqual(bundles, []string{"54092", "54092", "B091RFCS5V", "com.plexapp.ctv", "crunchyroll.roku"}) {
		t.Errorf("bundles = %q", bundles)
	}
	if !reflect.DeepEqual(platforms, []string{"Roku", "Roku", "Fire TV", "Android TV", "Roku"}) {
		t.Errorf("platforms = %q", platforms)
	}
	if items[0].AppName != "54092" {
		t.Errorf("app name = %q", items[0].AppName)
	}
	// whitespace stripped
	items = ParseCTVWorkItems(frame([]string{"app_name", "platform"}, []string{"  Netflix  ", " Hulu "}, []string{" Roku ", "  Fire TV  "}))
	if items[0].AppName != "Netflix" || items[0].Platform != "Roku" || items[1].AppName != "Hulu" || items[1].Platform != "Fire TV" {
		t.Errorf("strip: %+v", items)
	}
}

func TestParseCTVInput(t *testing.T) {
	f := frame([]string{"SSP", "Publisher", "App, Account or Network Name", "Bundle ID"},
		[]string{"A", "B", "nan", "C", "D"},
		[]string{"P1", "NA", "P3", "", "P5"},
		[]string{"One", "One", "Two", "None", " Three "},
		[]string{"512001.0", "x", "", "y", "NaN"})
	items := ParseCTVInput(f)
	got := ctvRows(items, false)
	want := [][]string{{"One", "512001", "A", "P1"}, {"Two", "", "", "P3"}, {"Three", "", "D", "P5"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q", got)
	}
	if ParseCTVInput(tabular.New([]string{"bundle"}, [][]string{{"x"}})) != nil {
		t.Error("no app column should give nil")
	}
}
