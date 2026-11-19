// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package jobs

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/ElcanoTek/lens/internal/cost"
)

// DeepRetrySecondsPerItem is the per-item cost projected for the retry queue
// in ETAs: single-session headless Chrome averages ~15s per site with
// container startup amortized.
const DeepRetrySecondsPerItem = 15.0

const (
	progressCacheVersion    = 1
	maxProgressCacheEntries = 512
	progressStreamBufBytes  = 256 * 1024
)

type memProgressEntry struct {
	mtimeNano int64
	size      int64
	summary   map[string]any
}

var (
	progressMemMu sync.RWMutex
	progressMem   = map[string]memProgressEntry{}
)

// diskProgressCache is the tiny sidecar persisted under
// <outputs>/.progress-cache/<filename>.json so cold starts and restarts
// never re-scan multi-hundred-MB *_progress.json files.
type diskProgressCache struct {
	Version int            `json:"v"`
	MtimeNs int64          `json:"mtime_ns"`
	Size    int64          `json:"size"`
	Summary map[string]any `json:"summary"`
	// IntKeys names keys in Summary whose Go type must be restored to int
	// after JSON unmarshaling (which decodes all numbers as float64).
	IntKeys []string `json:"int_keys,omitempty"`
}

func progressSidecarPath(path string) string {
	clean := filepath.Clean(path)
	return filepath.Join(filepath.Dir(clean), ProgressCacheDirname, filepath.Base(clean)+".json")
}

// RemoveProgressCache evicts path from the in-memory progress summary cache
// and best-effort removes its on-disk sidecar.
func RemoveProgressCache(path string) {
	if path == "" {
		return
	}
	clean := filepath.Clean(path)
	progressMemMu.Lock()
	delete(progressMem, clean)
	progressMemMu.Unlock()
	_ = os.Remove(progressSidecarPath(clean))
}

func getMemProgress(clean string, mtimeNano, size int64) (map[string]any, bool) {
	progressMemMu.RLock()
	e, ok := progressMem[clean]
	progressMemMu.RUnlock()
	if !ok || e.mtimeNano != mtimeNano || e.size != size {
		return nil, false
	}
	return cloneProgressMap(e.summary), true
}

func putMemProgress(clean string, mtimeNano, size int64, summary map[string]any) {
	cloned := cloneProgressMap(summary)
	progressMemMu.Lock()
	if len(progressMem) >= maxProgressCacheEntries {
		if _, exists := progressMem[clean]; !exists {
			progressMem = make(map[string]memProgressEntry, maxProgressCacheEntries/2)
		}
	}
	progressMem[clean] = memProgressEntry{mtimeNano: mtimeNano, size: size, summary: cloned}
	progressMemMu.Unlock()
}

func cloneProgressMap(src map[string]any) map[string]any {
	out := make(map[string]any, len(src))
	for k, v := range src {
		out[k] = cloneProgressValue(v)
	}
	return out
}

func cloneProgressValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return cloneProgressMap(x)
	case []any:
		cp := make([]any, len(x))
		for i, elem := range x {
			cp[i] = cloneProgressValue(elem)
		}
		return cp
	default:
		return v
	}
}

func loadDiskProgressCache(clean string, mtimeNano, size int64) (map[string]any, bool) {
	raw, err := os.ReadFile(progressSidecarPath(clean))
	if err != nil {
		return nil, false
	}
	var entry diskProgressCache
	if json.Unmarshal(raw, &entry) != nil || entry.Version != progressCacheVersion ||
		entry.MtimeNs != mtimeNano || entry.Size != size || entry.Summary == nil {
		return nil, false
	}
	for _, k := range entry.IntKeys {
		if f, ok := entry.Summary[k].(float64); ok && f == math.Trunc(f) {
			entry.Summary[k] = int(f)
		}
	}
	return entry.Summary, true
}

func saveDiskProgressCache(clean string, mtimeNano, size int64, summary map[string]any) {
	sidecar := progressSidecarPath(clean)
	cacheDir := filepath.Dir(sidecar)
	if err := os.MkdirAll(cacheDir, 0o755); err != nil { //nolint:gosec // cache dir beside outputs, matching .breakdowns
		return
	}
	intKeys := make([]string, 0, 6)
	for k, v := range summary {
		if _, ok := v.(int); ok {
			intKeys = append(intKeys, k)
		}
	}
	raw, err := json.Marshal(diskProgressCache{
		Version: progressCacheVersion,
		MtimeNs: mtimeNano,
		Size:    size,
		Summary: summary,
		IntKeys: intKeys,
	})
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(cacheDir, ".tmp-*")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return
	}
	if err := os.Rename(tmpName, sidecar); err != nil {
		_ = os.Remove(tmpName)
	}
}

// ReadProgress returns a job progress file normalised to the summary keys
// the dashboard JS and API expect (total_domains, processed, successful,
// errors, retrying, remaining, completion_percentage, start_time,
// last_update, cost_* and retry_seconds_per_item). Missing or corrupt files
// give an empty map.
//
// Results are cached in memory and in a tiny sidecar under .progress-cache/
// keyed by (mtime_ns, size), and cache misses stream-parse the file so a
// 100+ MB progress file with tens of thousands of processed_domains entries
// never allocates a giant map in memory.
func ReadProgress(path string) map[string]any {
	clean := filepath.Clean(path)
	st, err := os.Stat(clean)
	if err != nil || !st.Mode().IsRegular() {
		progressMemMu.Lock()
		delete(progressMem, clean)
		progressMemMu.Unlock()
		return map[string]any{}
	}
	mtimeNano, size := st.ModTime().UnixNano(), st.Size()
	if cached, ok := getMemProgress(clean, mtimeNano, size); ok {
		return cached
	}
	if cached, ok := loadDiskProgressCache(clean, mtimeNano, size); ok {
		putMemProgress(clean, mtimeNano, size, cached)
		return cloneProgressMap(cached)
	}

	// Open and stat the exact fd we parse so an atomic rename between os.Stat
	// and os.Open can never pair old metadata with new bytes.
	f, err := os.Open(clean)
	if err != nil {
		return map[string]any{}
	}
	defer f.Close()
	fst, err := f.Stat()
	if err != nil || !fst.Mode().IsRegular() {
		return map[string]any{}
	}
	summary, ok := parseProgressStream(bufio.NewReaderSize(f, progressStreamBufBytes))
	if !ok {
		return map[string]any{}
	}
	fdMtime, fdSize := fst.ModTime().UnixNano(), fst.Size()
	putMemProgress(clean, fdMtime, fdSize, summary)
	saveDiskProgressCache(clean, fdMtime, fdSize, summary)
	return cloneProgressMap(summary)
}

func parseProgressStream(r io.Reader) (map[string]any, bool) {
	dec := json.NewDecoder(r)
	tok, err := dec.Token()
	if err != nil {
		return nil, false
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil, false
	}

	payload := map[string]any{}
	hasEntries := false
	entryCount := 0
	retrying := 0

	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, false
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, false
		}
		switch key {
		case "processed_domains":
			cnt, ret, isObj, err := scanProcessedDomains(dec)
			if err != nil {
				return nil, false
			}
			hasEntries = isObj
			entryCount = cnt
			retrying = ret
		case "summary", "total_domains", "processed", "successful_count", "successful",
			"error_count", "errors", "start_time", "last_update", "cost":
			var val any
			if err := dec.Decode(&val); err != nil {
				return nil, false
			}
			payload[key] = val
		default:
			if err := skipJSONValue(dec); err != nil {
				return nil, false
			}
		}
	}
	endTok, err := dec.Token()
	if err != nil {
		return nil, false
	}
	if delim, ok := endTok.(json.Delim); !ok || delim != '}' {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
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
		return out, true
	}

	out := map[string]any{}
	total := payload["total_domains"]
	if total == nil {
		total = 0.0
	}
	out["total_domains"] = total

	// processed counts terminal entries only; retry_pending items are still
	// in flight (a later pass redoes them), so the bar must not count them.
	var processed any
	if hasEntries {
		processed = entryCount - retrying
	} else {
		retrying = 0
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
	return out, true
}

func scanProcessedDomains(dec *json.Decoder) (count, retrying int, isObj bool, err error) {
	tok, err := dec.Token()
	if err != nil {
		return 0, 0, false, err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return 0, 0, false, nil
	}
	if delim == '[' {
		return 0, 0, false, skipJSONContainer(dec)
	}
	if delim != '{' {
		return 0, 0, false, fmt.Errorf("unexpected delimiter %q", delim)
	}
	for dec.More() {
		if _, err := dec.Token(); err != nil {
			return 0, 0, false, err
		}
		valTok, err := dec.Token()
		if err != nil {
			return 0, 0, false, err
		}
		count++
		if d, ok := valTok.(json.Delim); ok {
			switch d {
			case '{':
				isRetry := false
				for dec.More() {
					fkTok, err := dec.Token()
					if err != nil {
						return 0, 0, false, err
					}
					fk, _ := fkTok.(string)
					if fk == "status" {
						var status any
						if err := dec.Decode(&status); err != nil {
							return 0, 0, false, err
						}
						s, ok := status.(string)
						isRetry = ok && s == "retry_pending"
					} else if err := skipJSONValue(dec); err != nil {
						return 0, 0, false, err
					}
				}
				if _, err := dec.Token(); err != nil {
					return 0, 0, false, err
				}
				if isRetry {
					retrying++
				}
			case '[':
				if err := skipJSONContainer(dec); err != nil {
					return 0, 0, false, err
				}
			default:
				return 0, 0, false, fmt.Errorf("unexpected delimiter %q", d)
			}
		}
	}
	if _, err := dec.Token(); err != nil {
		return 0, 0, false, err
	}
	return count, retrying, true, nil
}

func skipJSONValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	if delim != '{' && delim != '[' {
		return fmt.Errorf("unexpected delimiter %q", delim)
	}
	return skipJSONContainer(dec)
}

func skipJSONContainer(dec *json.Decoder) error {
	depth := 1
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
	}
	return nil
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
