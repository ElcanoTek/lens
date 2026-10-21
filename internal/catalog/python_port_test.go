// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package catalog

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// Ported from tests/test_model_catalog.py.

func model(id string, prompt, completion any, params []string, name string) json.RawMessage {
	if params == nil {
		params = []string{"tools", "temperature"}
	}
	if name == "" {
		name = id
	}
	raw, _ := json.Marshal(map[string]any{
		"id": id, "name": name,
		"pricing":              map[string]any{"prompt": fmt.Sprint(prompt), "completion": fmt.Sprint(completion)},
		"supported_parameters": params,
	})
	return raw
}

func ids(opts []Option) []string {
	out := make([]string, 0, len(opts))
	for _, o := range opts {
		out = append(out, o.ID)
	}
	return out
}

func TestClassifyCatalogFiltersByPriceCapAndToolSupport(t *testing.T) {
	c := Build([]json.RawMessage{
		model(RecommendedModel, 1.5e-6, 9e-6, nil, ""),
		model("cheap/model", 1e-7, 4e-7, nil, ""),
		model("frontier/model", 5e-6, 2.5e-5, nil, ""),    // over both caps
		model("pricey-output/model", 1e-6, 2e-5, nil, ""), // over completion cap
		model("no-tools/model", 1e-7, 4e-7, []string{"temperature"}, ""),
		model("free/model:free", 0, 0, nil, ""), // rate-limited free tier
	})
	if got := ids(c.Classify); !reflect.DeepEqual(got, []string{RecommendedModel, "cheap/model"}) {
		t.Fatalf("classify = %v", got)
	}
}

func TestResearchCatalogRequiresWebSearchSupport(t *testing.T) {
	web := []string{"web_search_options", "temperature"}
	c := Build([]json.RawMessage{
		model(RecommendedResearchModel, 3e-6, 1.5e-5, web, ""),
		model("perplexity/sonar-pro", 1e-6, 1e-6, web, ""),
		model("no-search/model", 1e-7, 4e-7, nil, ""),
		model("pricey-search/model", 5e-6, 2.5e-5, web, ""),
		model("requires-explicit-search/model", 1e-6, 1e-6, web, ""),
	})
	if got := ids(c.Research); !reflect.DeepEqual(got, []string{RecommendedResearchModel, "perplexity/sonar-pro"}) {
		t.Fatalf("research = %v", got)
	}
	// A web-search-only model must not leak into the classification list.
	if got := ids(c.Classify); !reflect.DeepEqual(got, []string{"no-search/model"}) {
		t.Fatalf("classify = %v", got)
	}
}

func TestCatalogOrdersRecommendedThenAliasesThenAlpha(t *testing.T) {
	c := Build([]json.RawMessage{
		model("zzz/model", 1e-7, 4e-7, nil, ""),
		model("~vendor/other-latest", 1e-6, 5e-6, nil, ""),
		model(RecommendedModel, 1.5e-6, 9e-6, nil, ""),
		model("aaa/model", 1e-7, 4e-7, nil, ""),
	})
	want := []string{RecommendedModel, "~vendor/other-latest", "aaa/model", "zzz/model"}
	if got := ids(c.Classify); !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v", got)
	}
}

func TestCatalogLabelsIncludePricing(t *testing.T) {
	label := Build([]json.RawMessage{model(RecommendedModel, 1.5e-6, 9e-6, nil, "Gemini Flash Latest")}).Classify[0].Label
	for _, want := range []string{"Gemini Flash Latest", "$1.5/M in", "$9/M out"} {
		if !strings.Contains(label, want) {
			t.Errorf("label %q lacks %q", label, want)
		}
	}
}

func TestResearchPricesShowSearchFees(t *testing.T) {
	build := func(webSearch any) string {
		pricing := map[string]any{"prompt": "1e-06", "completion": "1e-06"}
		if webSearch != nil {
			pricing["web_search"] = webSearch
		}
		raw, _ := json.Marshal(map[string]any{"id": "perplexity/sonar", "name": "Sonar", "pricing": pricing, "supported_parameters": []string{"web_search_options"}})
		return Build([]json.RawMessage{raw}).Research[0].Label
	}
	for _, tc := range []struct {
		webSearch any
		want      string
	}{{nil, "search fees extra"}, {"", "search fees extra"}, {"0.005", "search from $5/1K requests"}} {
		if got := build(tc.webSearch); !strings.Contains(got, tc.want) {
			t.Errorf("web_search %v: label %q lacks %q", tc.webSearch, got, tc.want)
		}
	}
}

func TestCatalogToleratesMalformedEntries(t *testing.T) {
	c := Build([]json.RawMessage{
		json.RawMessage(`{"id": "", "pricing": {}}`),
		json.RawMessage(`{"id": "bad/pricing", "pricing": {"prompt": "n/a", "completion": null}}`),
		json.RawMessage(`{"no_id": true}`),
		json.RawMessage(`not json`),
		model("ok/model", 1e-7, 4e-7, nil, ""),
	})
	if got := ids(c.Classify); !reflect.DeepEqual(got, []string{"ok/model"}) {
		t.Fatalf("classify = %v", got)
	}
}
