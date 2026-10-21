// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package cost records OpenRouter spend per item and per run, exactly as
// OpenRouter reports it.
//
// Every OpenRouter response carries usage.cost in US dollars, including
// search fees and retries that were billed. Lens records that figure; it
// never estimates from token counts and prices.
//
// Two meters ride on the context so concurrent items never mix:
//
//   - the run meter is installed once by the orchestrator and inherited by
//     every item; it is persisted in the progress file, so a resumed run
//     keeps counting from its previous total;
//   - an item meter is opened by each processor entry point (WithItem) and
//     read when the item's CSV row is written (ItemUSD).
//
// Calls whose response carries no cost (for example TypeSafe's own endpoint)
// are counted as unpriced rather than silently treated as free.
package cost

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Meter accumulates spend. It is safe for concurrent use.
type Meter struct {
	mu            sync.Mutex
	usd           float64
	calls         int
	unpricedCalls int
	byModel       map[string]float64
}

// NewMeter returns an empty meter.
func NewMeter() *Meter { return &Meter{byModel: map[string]float64{}} }

// Add records one billed call. A nil cost counts as unpriced.
func (m *Meter) Add(cost *float64, model string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if cost == nil {
		m.unpricedCalls++
		return
	}
	m.usd += *cost
	if model == "" {
		model = "unknown"
	}
	m.byModel[model] += *cost
}

// USD is the total priced spend.
func (m *Meter) USD() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.usd
}

// Calls is the number of recorded calls, priced or not.
func (m *Meter) Calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// UnpricedCalls is the number of calls whose response carried no cost.
func (m *Meter) UnpricedCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.unpricedCalls
}

// Snapshot is the JSON shape saved under the progress file's "cost" key.
type Snapshot struct {
	USD           float64            `json:"usd"`
	Calls         int                `json:"calls"`
	UnpricedCalls int                `json:"unpriced_calls"`
	ByModel       map[string]float64 `json:"by_model"`
}

func round6(v float64) float64 { return math.Round(v*1e6) / 1e6 }

// Snapshot returns the meter's persisted form, rounded to micro-dollars.
func (m *Meter) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	by := make(map[string]float64, len(m.byModel))
	keys := make([]string, 0, len(m.byModel))
	for k := range m.byModel {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		by[k] = round6(m.byModel[k])
	}
	return Snapshot{USD: round6(m.usd), Calls: m.calls, UnpricedCalls: m.unpricedCalls, ByModel: by}
}

// Restore rebuilds a run meter from a saved snapshot (any JSON-decoded value),
// tolerating missing or malformed data.
func Restore(data any) *Meter {
	m := NewMeter()
	obj, ok := data.(map[string]any)
	if !ok {
		return m
	}
	if usd, ok := Parse(obj["usd"]); ok {
		m.usd = usd
	}
	if n, ok := nonNegativeInt(obj["calls"]); ok {
		m.calls = n
	}
	if n, ok := nonNegativeInt(obj["unpriced_calls"]); ok {
		m.unpricedCalls = n
	}
	if by, ok := obj["by_model"].(map[string]any); ok {
		for model, v := range by {
			if c, ok := Parse(v); ok {
				m.byModel[model] = c
			}
		}
	}
	return m
}

func nonNegativeInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		if n >= 0 && n == math.Trunc(n) {
			return int(n), true
		}
	case int:
		if n >= 0 {
			return n, true
		}
	case json.Number:
		if i, err := n.Int64(); err == nil && i >= 0 {
			return int(i), true
		}
	}
	return 0, false
}

// Parse returns a finite, non-negative dollar amount from a JSON value
// (number or numeric string). Booleans and everything else are rejected.
func Parse(v any) (float64, bool) {
	var f float64
	switch n := v.(type) {
	case float64:
		f = n
	case float32:
		f = float64(n)
	case int:
		f = float64(n)
	case int64:
		f = float64(n)
	case json.Number:
		var err error
		if f, err = n.Float64(); err != nil {
			return 0, false
		}
	case string:
		var err error
		if f, err = strconv.ParseFloat(strings.TrimSpace(n), 64); err != nil {
			return 0, false
		}
	default:
		return 0, false
	}
	if math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
		return 0, false
	}
	return f, true
}

// FromUsage extracts usage.cost from a decoded response "usage" object.
// It returns nil when the response reported no usable cost.
func FromUsage(usage any) *float64 {
	obj, ok := usage.(map[string]any)
	if !ok {
		return nil
	}
	c, ok := Parse(obj["cost"])
	if !ok {
		return nil
	}
	return &c
}

type ctxKey int

const (
	runKey ctxKey = iota
	itemKey
)

// WithRun installs the run meter on ctx.
func WithRun(ctx context.Context, m *Meter) context.Context {
	return context.WithValue(ctx, runKey, m)
}

// WithItem gives ctx its own item meter unless one is already open. Nested
// entry points (research inside a website item) keep adding to the outer
// item's meter.
func WithItem(ctx context.Context) context.Context {
	if _, ok := ctx.Value(itemKey).(*Meter); ok {
		return ctx
	}
	return context.WithValue(ctx, itemKey, NewMeter())
}

// Record adds one billed call to the run and item meters on ctx.
func Record(ctx context.Context, cost *float64, model string) {
	for _, key := range []ctxKey{runKey, itemKey} {
		if m, ok := ctx.Value(key).(*Meter); ok && m != nil {
			m.Add(cost, model)
		}
	}
}

// ItemUSD is the open item's spend formatted for its CSV row; blank outside
// an item, or when every call it made was unpriced.
func ItemUSD(ctx context.Context) string {
	m, ok := ctx.Value(itemKey).(*Meter)
	if !ok || m == nil {
		return ""
	}
	calls, unpriced, usd := m.Calls(), m.UnpricedCalls(), m.USD()
	if calls > 0 && unpriced == calls {
		return ""
	}
	return fmt.Sprintf("%.6f", usd)
}
