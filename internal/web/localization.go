// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package web

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ElcanoTek/lens/internal/i18n"
	"github.com/ElcanoTek/lens/internal/jobs"
)

func selectedLanguage(languages []string) string {
	if len(languages) > 0 && i18n.Normalize(languages[0]) != "" {
		return i18n.Normalize(languages[0])
	}
	return "en"
}
func displayTimestamp(value time.Time, language string) string {
	if language == "en" {
		return value.Format(displayLayout)
	}
	return value.Format("02/01/2006 15:04 UTC")
}
func displayUploaded(raw string, value time.Time, language string) string {
	if language == "en" {
		return raw
	}
	for _, prefix := range []string{"today ", "yesterday "} {
		if tail, ok := strings.CutPrefix(raw, prefix); ok {
			return i18n.T(language, prefix+"{0}", tail)
		}
	}
	return value.Format("02/01/2006")
}
func displaySize(raw, language string) string {
	if language == "en" {
		return raw
	}
	return strings.Replace(raw, ".", ",", 1)
}
func displayBreakdown(chips []jobs.Chip, language string) string {
	labels := make([]string, 0, len(chips))
	for _, chip := range chips {
		labels = append(labels, i18n.T(language, "{0} {1}", chip.Count, i18n.T(language, chip.Label)))
	}
	return strings.Join(labels, " · ")
}

var costTextPattern = regexp.MustCompile(`^(<)?\$([0-9,]+)(?:\.([0-9]+))?(\+)?$`)

func displayCost(raw, language string) string {
	if language == "en" {
		return raw
	}
	match := costTextPattern.FindStringSubmatch(raw)
	if match == nil {
		return raw
	}
	whole, err := strconv.ParseInt(strings.ReplaceAll(match[2], ",", ""), 10, 64)
	if err != nil {
		return raw
	}
	result := match[1] + i18n.Number(whole, language)
	if match[3] != "" {
		result += "," + match[3]
	}
	return result + " US$" + match[4]
}

// Model names and IDs are provider data; only Lens's appended price labels
// are presentation text. Catalog/API values remain unchanged.
var modelLabelPattern = regexp.MustCompile(`^(.*) · (\$[0-9.]+)/M in(?: · (\$[0-9.]+)/M out)?(?: · search from (\$[0-9.]+)/1K requests)?$`)

func displayModelLabel(raw, language string) string {
	if language == "en" {
		return raw
	}
	match := modelLabelPattern.FindStringSubmatch(raw)
	if match == nil {
		return raw
	}
	label := i18n.T(language, "{0} · {1}/M input", match[1], displayCost(match[2], language))
	if match[3] != "" {
		label = i18n.T(language, "{0} · {1}/M input · {2}/M output", match[1], displayCost(match[2], language), displayCost(match[3], language))
	}
	if match[4] != "" {
		label += i18n.T(language, " · search from {0}/1000 requests", displayCost(match[4], language))
	}
	return label
}
