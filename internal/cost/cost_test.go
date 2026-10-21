// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package cost

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"sync"
	"testing"
)

func TestMeterSnapshotRestoresAndRejectsBadValues(t *testing.T) {
	m := NewMeter()
	a, b := 0.25, 0.5
	m.Add(&a, "openai/gpt-6-luna")
	m.Add(&b, "perplexity/sonar-pro")
	m.Add(nil, "typesafe")
	snap := m.Snapshot()
	want := Snapshot{USD: 0.75, Calls: 3, UnpricedCalls: 1, ByModel: map[string]float64{"openai/gpt-6-luna": 0.25, "perplexity/sonar-pro": 0.5}}
	if !reflect.DeepEqual(snap, want) {
		t.Fatalf("snapshot = %+v", snap)
	}
	raw, _ := json.Marshal(snap)
	if string(raw) != `{"usd":0.75,"calls":3,"unpriced_calls":1,"by_model":{"openai/gpt-6-luna":0.25,"perplexity/sonar-pro":0.5}}` {
		t.Fatalf("json = %s", raw)
	}
	var decoded any
	_ = json.Unmarshal(raw, &decoded)
	restored := Restore(decoded)
	restored.Add(&a, "openai/gpt-6-luna")
	if restored.Snapshot().USD != 1.0 || restored.Calls() != 4 || restored.UnpricedCalls() != 1 {
		t.Fatalf("restored = %+v", restored.Snapshot())
	}
	junk := Restore(map[string]any{"usd": "nan", "calls": -1.0, "by_model": map[string]any{"x": -2.0}})
	if !reflect.DeepEqual(junk.Snapshot(), Snapshot{ByModel: map[string]float64{}}) {
		t.Fatalf("junk = %+v", junk.Snapshot())
	}
	if Restore("not a dict").Calls() != 0 {
		t.Fatal("non-dict restore")
	}
}

func TestCostIsReadFromUsage(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	cases := []struct {
		usage any
		want  *float64
	}{
		{map[string]any{"cost": 0.0042}, f(0.0042)},
		{map[string]any{"cost": "0.5"}, f(0.5)},
		{map[string]any{"cost": json.Number("0.01")}, f(0.01)},
		{map[string]any{"cost": -1.0}, nil},
		{map[string]any{"cost": true}, nil},
		{map[string]any{"cost": math.NaN()}, nil},
		{map[string]any{"cost": math.Inf(1)}, nil},
		{map[string]any{}, nil},
		{nil, nil},
		{"0.1", nil},
	}
	for _, tc := range cases {
		got := FromUsage(tc.usage)
		if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
			t.Errorf("FromUsage(%v) = %v, want %v", tc.usage, got, tc.want)
		}
	}
}

func TestItemsAreIsolatedAndNestedEntryPointsShareAMeter(t *testing.T) {
	run := NewMeter()
	ctx := WithRun(context.Background(), run)
	research := func(ctx context.Context) {
		ctx = WithItem(ctx) // nested: adds to the open item, not a new meter
		c := 0.01
		Record(ctx, &c, "sonar")
	}
	var mu sync.Mutex
	seen := map[string]string{}
	var wg sync.WaitGroup
	for name, spend := range map[string]float64{"a": 0.1, "b": 0.2} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := WithItem(ctx)
			Record(ctx, &spend, "luna")
			research(ctx)
			mu.Lock()
			seen[name] = ItemUSD(ctx)
			mu.Unlock()
		}()
	}
	wg.Wait()
	if !reflect.DeepEqual(seen, map[string]string{"a": "0.110000", "b": "0.210000"}) {
		t.Fatalf("seen = %v", seen)
	}
	if math.Abs(run.Snapshot().USD-0.32) > 1e-9 {
		t.Fatalf("run = %v", run.Snapshot().USD)
	}
	if ItemUSD(ctx) != "" {
		t.Fatal("no item open outside the processors")
	}
}

func TestItemUSDBlankWhenEveryCallUnpriced(t *testing.T) {
	ctx := WithItem(context.Background())
	Record(ctx, nil, "typesafe")
	if ItemUSD(ctx) != "" {
		t.Fatalf("item = %q", ItemUSD(ctx))
	}
	c := 0.0123
	Record(ctx, &c, "openai/gpt-6-luna")
	if ItemUSD(ctx) != "0.012300" {
		t.Fatalf("item = %q", ItemUSD(ctx))
	}
	// An item with no calls at all reads as zero spend.
	if ItemUSD(WithItem(context.Background())) != "0.000000" {
		t.Fatal("empty item")
	}
}
