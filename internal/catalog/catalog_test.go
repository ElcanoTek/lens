// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package catalog

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

const payload = `{"data": [
 {"id": "~openai/gpt-luna-latest", "name": "OpenAI: GPT Luna Latest", "pricing": {"prompt": "0.0000002", "completion": "0.0000012"}, "supported_parameters": ["tools"]},
 {"id": "b/model", "name": "B Model", "pricing": {"prompt": "0.000001", "completion": "0.000002"}, "supported_parameters": ["tools"]},
 {"id": "~a/alias-latest", "name": "Z Alias", "pricing": {"prompt": "0.000001", "completion": "0.000002"}, "supported_parameters": ["tools"]},
 {"id": "free/model", "name": "Free", "pricing": {"prompt": "0", "completion": "0"}, "supported_parameters": ["tools"]},
 {"id": "pricey/model", "name": "Pricey", "pricing": {"prompt": "0.00001", "completion": "0.00003"}, "supported_parameters": ["tools"]},
 {"id": "notools/model", "name": "No tools", "pricing": {"prompt": "0.000001", "completion": "0.000001"}, "supported_parameters": []},
 {"id": "perplexity/sonar", "name": "Perplexity: Sonar", "pricing": {"prompt": "0.000001", "completion": "0.000001", "web_search": "0.005"}, "supported_parameters": ["web_search_options"]},
 {"id": "perplexity/sonar-pro", "name": "Perplexity: Sonar Pro", "pricing": {"prompt": "0.000003", "completion": "0.000015"}, "supported_parameters": ["web_search_options"]},
 {"id": "other/search", "name": "Other search", "pricing": {"prompt": "0.000001", "completion": "0.000001"}, "supported_parameters": ["web_search_options"]},
 {"id": "~typesafe/jev-latest", "name": "TypeSafe: Jev", "pricing": {"prompt": "0.0000001"}, "architecture": {"output_modalities": ["decisions"]}}
]}`

func TestBuild(t *testing.T) {
	var doc struct{ Data []json.RawMessage }
	if err := json.Unmarshal([]byte(payload), &doc); err != nil {
		t.Fatal(err)
	}
	c := Build(doc.Data)
	ids := func(opts []Option) (out []string) {
		for _, o := range opts {
			out = append(out, o.ID)
		}
		return
	}
	got := ids(c.Classify)
	want := []string{"~openai/gpt-luna-latest", "~a/alias-latest", "b/model"}
	if len(got) != len(want) {
		t.Fatalf("classify = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("classify = %v, want %v", got, want)
		}
	}
	if c.Classify[0].Label != "OpenAI: GPT Luna Latest · $0.2/M in · $1.2/M out" {
		t.Errorf("label = %q", c.Classify[0].Label)
	}
	if r := ids(c.Research); len(r) != 2 || r[0] != "perplexity/sonar" {
		t.Fatalf("research = %v", r)
	}
	if c.Research[0].Label != "Perplexity: Sonar · $1/M in · $1/M out · search from $5/1K requests" || c.Research[1].Label != "Perplexity: Sonar Pro · $3/M in · $15/M out · search fees extra" {
		t.Errorf("research labels = %+v", c.Research)
	}
	if d := ids(c.Decision); len(d) != 1 || c.Decision[0].Label != "TypeSafe: Jev · $0.1/M in" {
		t.Fatalf("decision = %+v", c.Decision)
	}
}

func TestSourceCachesAndBacksOff(t *testing.T) {
	var hits atomic.Int32
	fail := atomic.Bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		if fail.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()
	now := time.Unix(1000, 0)
	s := NewSource("")
	s.URL, s.Now = srv.URL, func() time.Time { return now }
	if len(s.Get(context.Background()).Classify) != 3 || hits.Load() != 1 {
		t.Fatal("first fetch")
	}
	s.Get(context.Background())
	if hits.Load() != 1 {
		t.Fatal("cache not used")
	}
	if !s.Allowed("classify", "b/model") || s.Allowed("classify", "pricey/model") || !s.Allowed("research", RecommendedResearchModel) {
		t.Fatal("Allowed")
	}
	fail.Store(true)
	now = now.Add(2 * time.Hour)
	if len(s.Get(context.Background()).Classify) != 3 {
		t.Fatal("stale catalog not served on failure")
	}

	empty := NewSource("")
	empty.URL, empty.Now = srv.URL, func() time.Time { return now }
	before := hits.Load()
	empty.Get(context.Background())
	empty.Get(context.Background())
	if hits.Load()-before != 1 {
		t.Fatal("failed fetch retried within a minute")
	}
}

// A fetch triggered by a request whose client went away still completes and
// fills the cache.
func TestSourceFetchSurvivesCancelledRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()
	s := NewSource("")
	s.URL = srv.URL
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if len(s.Get(ctx).Classify) != 3 || len(s.Cached().Classify) != 3 {
		t.Fatal("a cancelled request context aborted the shared catalog fetch")
	}
}
