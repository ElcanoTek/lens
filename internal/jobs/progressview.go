// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package jobs

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/ElcanoTek/lens/internal/cost"
)

// DeepRetrySecondsPerItem is the per-item cost projected for the retry queue
// in ETAs: single-session headless Chrome averages ~15s per site with
// container startup amortized.
const DeepRetrySecondsPerItem = 15.0

// ReadProgress returns a job progress file normalised to the summary keys
// the dashboard JS and API expect (total_domains, processed, successful,
// errors, retrying, remaining, completion_percentage, start_time,
// last_update, cost_* and retry_seconds_per_item). Missing or corrupt files
// give an empty map.
func ReadProgress(path string) map[string]any {
	raw, err := os.ReadFile(path) //nolint:gosec // path built from a validated job filename
	if err != nil {
		return map[string]any{}
	}
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil || payload == nil {
		return map[string]any{}
	}
	if summary, ok := payload["summary"].(map[string]any); ok {
		out := map[string]any{}
		for k, v := range summary {
			out[k] = v
		}
		for k, v := range CostFields(payload) {
			out[k] = v
		}
		out["retry_seconds_per_item"] = DeepRetrySecondsPerItem
		return out
	}

	out := map[string]any{}
	total := payload["total_domains"]
	if total == nil {
		total = 0.0
	}
	out["total_domains"] = total

	// processed counts terminal entries only; retry_pending items are still
	// in flight (a later pass redoes them), so the bar must not count them.
	retrying := 0
	var processed any
	if entries, ok := payload["processed_domains"].(map[string]any); ok {
		for _, e := range entries {
			if m, ok := e.(map[string]any); ok && m["status"] == "retry_pending" {
				retrying++
			}
		}
		processed = len(entries) - retrying
	} else {
		processed = payload["processed"]
		if processed == nil {
			processed = 0
		}
	}
	out["processed"] = processed
	out["retrying"] = retrying
	out["successful"] = firstPresent(payload, "successful_count", "successful")
	out["errors"] = firstPresent(payload, "error_count", "errors")

	totalN, okT := AsInt(total)
	processedN, okP := AsInt(processed)
	if !okT || !okP {
		totalN, processedN = 0, 0
	}
	out["remaining"] = max(0, totalN-processedN)
	pct := 0.0
	if totalN > 0 {
		pct = float64(processedN) / float64(totalN) * 100
	}
	out["completion_percentage"] = pct
	if v, ok := payload["start_time"]; ok {
		out["start_time"] = v
	}
	if v, ok := payload["last_update"]; ok {
		out["last_update"] = v
	}
	for k, v := range CostFields(payload) {
		out[k] = v
	}
	out["retry_seconds_per_item"] = DeepRetrySecondsPerItem
	return out
}

func firstPresent(m map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			return v
		}
	}
	return 0
}

// AsInt reads a JSON-ish count (number or numeric string), like Python's
// int(str(value)).
func AsInt(v any) (int, bool) {
	switch n := v.(type) {
	case nil:
		return 0, true
	case int:
		return n, true
	case float64:
		if n != math.Trunc(n) {
			return 0, false
		}
		return int(n), true
	case string:
		i, err := strconv.Atoi(strings.TrimSpace(n))
		return i, err == nil
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	}
	return 0, false
}

// AsFloat reads a JSON-ish number.
func AsFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case float64:
		return n, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		return f, err == nil
	}
	return 0, false
}

// FormatCost renders run spend for the Runs table.
func FormatCost(usd float64) string {
	switch {
	case usd <= 0:
		return "$0"
	case usd < 0.001:
		return "<$0.001"
	case usd < 1:
		return fmt.Sprintf("$%.3f", usd)
	}
	return "$" + commaFloat(usd)
}

func commaFloat(v float64) string {
	s := fmt.Sprintf("%.2f", v)
	intPart, frac, _ := strings.Cut(s, ".")
	var b strings.Builder
	for i, c := range intPart {
		if i > 0 && (len(intPart)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String() + "." + frac
}

// CostFields derives the Runs-table spend fields from a progress payload's
// "cost" snapshot (OpenRouter-reported).
func CostFields(payload map[string]any) map[string]any {
	c, ok := payload["cost"].(map[string]any)
	if !ok {
		return map[string]any{"cost_text": "", "cost_title": ""}
	}
	usd, _ := cost.Parse(c["usd"])
	calls := wholeOrZero(c["calls"])
	unpriced := wholeOrZero(c["unpriced_calls"])
	title := fmt.Sprintf("OpenRouter spend reported for this run: $%.4f over %d model calls", usd, calls)
	if unpriced > 0 {
		title += fmt.Sprintf("; %d without a reported cost (not included)", unpriced)
	}
	text := FormatCost(usd)
	if unpriced > 0 {
		text += "+"
	}
	return map[string]any{
		"cost_usd":            usd,
		"cost_calls":          calls,
		"cost_unpriced_calls": unpriced,
		"cost_text":           text,
		"cost_title":          title,
	}
}

func wholeOrZero(v any) int {
	if f, ok := v.(float64); ok && f == math.Trunc(f) && f >= 0 {
		return int(f)
	}
	return 0
}

// TailLines returns up to n trailing lines of a text file, reading at most
// the last 256 KiB so a huge log can't stall a request.
func TailLines(path string, n int) []string {
	f, err := os.Open(path) //nolint:gosec // validated job filename
	if err != nil {
		return []string{}
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return []string{}
	}
	const window = 256 * 1024
	offset := max(0, st.Size()-window)
	buf := make([]byte, st.Size()-offset)
	if _, err := f.ReadAt(buf, offset); err != nil && len(buf) == 0 {
		return []string{}
	}
	text := strings.ToValidUTF8(string(buf), "")
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if offset > 0 && len(lines) > 0 {
		lines = lines[1:] // partial first line
	}
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}
