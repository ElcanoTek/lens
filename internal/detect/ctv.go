// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package detect

import (
	"fmt"
	"strings"

	"github.com/ElcanoTek/lens/internal/item"
	"github.com/ElcanoTek/lens/internal/tabular"
)

// strongCTVIndicators are column-name fragments unlikely to appear in a
// generic app/website list. ("app name" and "publisher" are too generic:
// they also appear in mobile app lists.)
var strongCTVIndicators = []string{
	"bundle_id", "bundleid", "bundle id", "ssp", "ctv_app", "ctvapp", "ctv app",
	"channel_name", "channelname", "channel name", "roku_id", "rokuid", "fire_tv_id",
	"firetvid", "apple_tv_id", "appletvid",
	"app, account or network name", // the CTV_Master.csv format
}

// ctvPlatformColumns are column names that carry the CTV platform.
var ctvPlatformColumns = []string{"platform", "ctv_platform", "streaming_platform", "device", "device_type"}

// normalizedColumn lowercases a header and treats underscores as spaces,
// collapsing whitespace runs.
func normalizedColumn(name string) string {
	return strings.Join(strings.FieldsFunc(strings.ReplaceAll(strings.ToLower(name), "_", " "), tabular.IsSpace), " ")
}

// IsCTVInputFile reports whether the frame's headers say it is a CTV
// channel/app list (CTV is recognised per file, never per row).
func IsCTVInputFile(f *tabular.Frame) bool {
	columns := f.Columns()
	lower := make([]string, len(columns))
	for i, c := range columns {
		lower[i] = tabular.Strip(strings.ToLower(c))
	}
	anyContains := func(fragments ...string) bool {
		for _, col := range lower {
			all := true
			for _, frag := range fragments {
				if !strings.Contains(col, frag) {
					all = false
					break
				}
			}
			if all {
				return true
			}
		}
		return false
	}

	strong := 0
	for _, ind := range strongCTVIndicators {
		if anyContains(ind) {
			strong++
		}
	}
	if strong >= 1 {
		log.Info(fmt.Sprintf("Detected CTV input file (found %d strong CTV indicators)", strong))
		return true
	}
	if anyContains("ctv") || anyContains("connected") {
		log.Info("Detected CTV input file (found 'ctv' or 'connected' in column names)")
		return true
	}
	// SSP bundle exports often use a single "App Bundle" / "app_bundle"
	// column of Roku ids, Fire TV ASINs and CTV package names. That is not a
	// mobile list.
	for _, c := range columns {
		if normalizedColumn(c) == "app bundle" {
			log.Info("Detected CTV input file (app bundle column)")
			return true
		}
	}
	// The exact CTV_Master.csv shape: SSP + Bundle ID + App/Network Name.
	if anyContains("ssp") && anyContains("bundle", "id") && anyContains("app", "network") {
		log.Info("Detected CTV input file (SSP + Bundle ID + App/Network Name columns)")
		return true
	}
	return false
}

// CTVColumns names the frame's columns for each CTV field ("" = absent).
type CTVColumns struct {
	AppName   string
	BundleID  string
	SSP       string
	Publisher string
	Platform  string
	URL       string
}

func firstPresent(ix lowerIndex, candidates ...string) string {
	for _, c := range candidates {
		if col, ok := ix.get(c); ok {
			return col
		}
	}
	return ""
}

// DetectCTVColumns maps CTV fields to the frame's columns by
// (case-insensitive) name.
func DetectCTVColumns(f *tabular.Frame) CTVColumns {
	columns := f.Columns()
	ix := newLowerIndex(columns, true)
	var out CTVColumns

	out.AppName = firstPresent(ix, "app_name", "appname", "app name", "app", "name", "channel_name",
		"channelname", "channel name", "ctv_app", "ctvapp", "ctv app", "app, account or network name")
	if out.AppName == "" {
		// A name that contains "app" and "name" but is not an id column.
		for _, k := range ix.keys {
			if strings.Contains(k, "app") && strings.Contains(k, "name") && !strings.Contains(k, "id") {
				out.AppName = ix.m[k]
				break
			}
		}
	}

	out.BundleID = firstPresent(ix, "bundle_id", "bundleid", "bundle id", "bundle", "app_id", "appid",
		"app id", "roku_id", "rokuid", "fire_tv_id", "firetvid", "apple_tv_id", "appletvid")
	if out.BundleID == "" {
		// "App Bundle" and "app_bundle" are the bundle id, not the name.
		for _, c := range columns {
			if normalizedColumn(c) == "app bundle" {
				out.BundleID = c
				break
			}
		}
	}
	out.Platform = firstPresent(ix, ctvPlatformColumns...)
	out.SSP = firstPresent(ix, "ssp", "supply_side_platform", "supply side platform")
	out.Publisher = firstPresent(ix, "publisher", "pub", "publisher_name", "publisher name")
	out.URL = firstPresent(ix, "url", "website", "site", "link", "homepage")

	log.Debug(fmt.Sprintf("Detected CTV columns: %+v", out))
	return out
}

// CTVPlatform detects a CTV platform from bundle-id conventions, or
// "Unknown". It lives here (not with the CTV processor) because input
// parsing fills in the platform of rows that do not state one.
func CTVPlatform(bundleID string) string {
	b := tabular.Strip(bundleID)
	if b == "" {
		return "Unknown"
	}
	lower, upper := strings.ToLower(b), strings.ToUpper(b)
	switch {
	case isDigits(b): // 54092
		return "Roku"
	case strings.HasPrefix(upper, "B0"): // B091RFCS5V (an Amazon ASIN)
		return "Fire TV"
	case strings.HasPrefix(lower, "com."): // com.foxsports.videogo
		return "Android TV"
	case strings.HasPrefix(lower, "vizio."):
		return "Vizio"
	case strings.HasPrefix(lower, "tv."):
		return "Apple TV"
	case strings.HasPrefix(upper, "G") && len(b) > 1 && isDigits(b[1:]): // G00009197740
		return "Samsung TV"
	case strings.HasPrefix(b, "9") && isAlnum(b): // 9wzdncrfjv7w (Microsoft Store id)
		return "Xbox"
	case strings.HasSuffix(lower, ".roku"):
		return "Roku"
	case strings.HasPrefix(lower, "xbox."):
		return "Xbox"
	case strings.HasPrefix(lower, "playstation."):
		return "PlayStation"
	}
	return "Unknown"
}

// ParseCTVWorkItems turns a CTV list into work items (the CTV workflow's
// parse_ctv_work_items). Rows without an app name are skipped; with no app
// name column the first column is used. A one-column bundle list (App
// Bundle) has no separate title, so the bundle id doubles as the name the
// research step can quote. Rows without a platform get one from the bundle
// id when its shape gives it away.
func ParseCTVWorkItems(f *tabular.Frame) []item.CTVWorkItem {
	cols := DetectCTVColumns(f)
	if cols.AppName == "" {
		log.Warn("No app_name column found in CTV input file")
		if f.NumColumns() == 0 {
			return nil
		}
		cols.AppName = f.Columns()[0]
		log.Info(fmt.Sprintf("Using first column '%s' as app_name", cols.AppName))
	}
	index := func(name string) int {
		if name == "" {
			return -1
		}
		return f.Index(name)
	}
	appIdx, bundleIdx, platIdx := index(cols.AppName), index(cols.BundleID), index(cols.Platform)
	urlIdx, sspIdx, pubIdx := index(cols.URL), index(cols.SSP), index(cols.Publisher)
	cell := func(r, c int) string {
		if c < 0 {
			return ""
		}
		return SpreadsheetCell(f.Cell(r, c))
	}

	items := make([]item.CTVWorkItem, 0, f.NumRows())
	for r := range f.NumRows() {
		appName := cell(r, appIdx)
		if appName == "" {
			continue
		}
		bundleID := cell(r, bundleIdx)
		if bundleID != "" && cols.AppName == cols.BundleID {
			appName = bundleID
		}
		platform := cell(r, platIdx)
		if platform == "" && bundleID != "" {
			if detected := CTVPlatform(bundleID); detected != "Unknown" {
				platform = detected
			}
		}
		items = append(items, item.CTVWorkItem{
			AppName:     appName,
			BundleID:    bundleID,
			SSP:         cell(r, sspIdx),
			Publisher:   cell(r, pubIdx),
			Platform:    platform,
			URL:         cell(r, urlIdx),
			OriginalRow: f.RowMap(r),
		})
	}
	log.Info(fmt.Sprintf("Parsed %d CTV work items from input file", len(items)))
	return items
}

// findCTVColumn returns the first column whose lowered name contains key.
func findCTVColumn(f *tabular.Frame, key string) string {
	for _, c := range f.Columns() {
		if strings.Contains(strings.ToLower(c), key) {
			return c
		}
	}
	return ""
}

func pyNone(s string) string {
	if s == "" {
		return "None"
	}
	return s
}

// ParseCTVInput parses a CTV list in the CTV_Master.csv style (SSP,
// Publisher, "App, Account or Network Name", Bundle ID) into one work item
// per distinct app name: the --ctv path of the main pipeline.
func ParseCTVInput(f *tabular.Frame) []item.CTVWorkItem {
	sspCol := findCTVColumn(f, "ssp")
	pubCol := findCTVColumn(f, "publisher")
	appCol := findCTVColumn(f, "app")
	if appCol == "" {
		appCol = findCTVColumn(f, "account")
	}
	if appCol == "" {
		appCol = findCTVColumn(f, "network")
	}
	bundleCol := findCTVColumn(f, "bundle")
	if appCol == "" {
		log.Error("Could not find app name column in CTV input")
		return nil
	}
	log.Info(fmt.Sprintf("CTV column mapping: ssp=%s, publisher=%s, app_name=%s, bundle_id=%s",
		pyNone(sspCol), pyNone(pubCol), pyNone(appCol), pyNone(bundleCol)))

	// text is str(cell).strip() with pandas' NaN (and a literal "nan")
	// cleared, as the Python cleaned them up.
	text := func(r int, col string) string {
		if col == "" {
			return ""
		}
		raw := f.Cell(r, f.Index(col))
		v := tabular.Strip(raw)
		if tabular.IsNA(raw) || strings.EqualFold(v, "nan") {
			return ""
		}
		// Whole-number ids exported as floats ("512001.0") keep their
		// digits only, so a Roku id stays a Roku id.
		if col == bundleCol {
			v = SpreadsheetCell(v)
		}
		return v
	}

	seen := make(map[string]bool)
	items := make([]item.CTVWorkItem, 0, f.NumRows())
	for r := range f.NumRows() {
		appName := text(r, appCol)
		if appName == "" || strings.EqualFold(appName, "none") || seen[appName] {
			continue
		}
		seen[appName] = true
		items = append(items, item.CTVWorkItem{
			AppName:   appName,
			BundleID:  text(r, bundleCol),
			SSP:       text(r, sspCol),
			Publisher: text(r, pubCol),
		})
	}
	log.Info(fmt.Sprintf("Parsed %d unique CTV apps from input", len(items)))
	return items
}
