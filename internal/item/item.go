// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package item holds the work-item types every pipeline stage shares.
package item

import "strings"

// ContentType is what an input row turned out to be.
type ContentType string

// The content types Lens classifies. The string values are what progress
// files and logs have always carried.
const (
	IOSApp     ContentType = "ios_app"
	AndroidApp ContentType = "android_app"
	Website    ContentType = "website"
	CTVApp     ContentType = "ctv_app"
)

// antibotMarkers are failure-message fragments that mean "an anti-bot system
// blocked the request", not "this backend couldn't render the page". Shared
// by the orchestrator (to skip pointless deep-crawl retries) and the website
// processor (to derive the Bot_Protection column).
var antibotMarkers = []string{
	"anti-bot",
	"document_antibot",
	"block page detected",
	"http status 401",
	"http status 403",
	"http status 429",
}

// IsAntibotError reports whether a failure message carries an anti-bot or
// block-page signature.
func IsAntibotError(message string) bool {
	blob := strings.ToLower(message)
	for _, marker := range antibotMarkers {
		if strings.Contains(blob, marker) {
			return true
		}
	}
	return false
}

// WorkItem is one website, iOS app or Android app to classify.
type WorkItem struct {
	// Identifier is the domain, App Store ID or package name.
	Identifier string
	Type       ContentType
	// OriginalValue is the cell as it appeared in the input file.
	OriginalValue string
}

// IsApp reports whether the item is a mobile app (iOS or Android).
func (w WorkItem) IsApp() bool { return w.Type == IOSApp || w.Type == AndroidApp }

// CTVWorkItem is one connected-TV app or channel.
type CTVWorkItem struct {
	AppName   string
	BundleID  string
	SSP       string
	Publisher string
	Platform  string
	URL       string
	// OriginalRow is the input row keyed by column name, when available.
	OriginalRow map[string]string
}

// Identifier is the key progress is tracked under: the bundle ID when there
// is one, else the app name.
func (c CTVWorkItem) Identifier() string {
	if c.BundleID != "" {
		return c.BundleID
	}
	return c.AppName
}
