// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package catalog builds the dashboard's (and API's) model dropdowns from
// OpenRouter's live /models listing, cached for an hour.
//
// Classification is high-volume and structurally simple, so its list is
// capped at workhorse pricing and needs tool calling; research needs
// built-in web search and is limited to the models verified to search on a
// plain chat request; custom categories need a decision model.
package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ElcanoTek/lens/internal/config"
)

// Recommended defaults, in lockstep with config's defaults.
const (
	RecommendedModel         = config.DefaultLLMModel
	RecommendedResearchModel = config.DefaultResearchModel
	RecommendedDecisionModel = config.DefaultDecisionModel
)

// Browsers that saved an old recommended slug follow the new default once;
// an explicit later pick still sticks (see main.js).
var (
	PreviousRecommendedModels         = []string{"~google/gemini-flash-latest"}
	PreviousRecommendedResearchModels = []string{"perplexity/sonar-pro"}
)

// ResearchModels are the bounded, non-reasoning models that search
// automatically in our call path. Advertising web_search_options alone does
// not mean a plain chat request searches.
var ResearchModels = map[string]bool{"perplexity/sonar": true, "perplexity/sonar-pro": true}

// Price caps, per token (OpenRouter's native unit).
const (
	ModelPromptPriceCap        = 2.0e-6
	ModelCompletionPriceCap    = 10.0e-6
	ResearchPromptPriceCap     = 3.0e-6
	ResearchCompletionPriceCap = 15.0e-6
	DecisionPromptPriceCap     = 1.0e-6
)

// ModelsURL asks for both modalities: the default listing leaves decision
// models out.
const ModelsURL = "https://openrouter.ai/api/v1/models?output_modalities=text,decisions"

const (
	ttl = time.Hour
	// After a failed fetch don't retry for a minute, or a box that can't
	// reach OpenRouter stalls every dashboard load on the fetch timeout.
	retryAfter = time.Minute
)

// Option is one dropdown entry.
type Option struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// Catalog holds every dropdown.
type Catalog struct {
	Classify []Option `json:"classify"`
	Research []Option `json:"research"`
	Decision []Option `json:"decision"`
}

func (c Catalog) empty() bool {
	return len(c.Classify) == 0 && len(c.Research) == 0 && len(c.Decision) == 0
}

// Recommended returns the recommended id for a kind ("classify",
// "research", "decision").
func Recommended(kind string) string {
	switch kind {
	case "research":
		return RecommendedResearchModel
	case "decision":
		return RecommendedDecisionModel
	}
	return RecommendedModel
}

// Options returns one kind's entries.
func (c Catalog) Options(kind string) []Option {
	switch kind {
	case "research":
		return c.Research
	case "decision":
		return c.Decision
	}
	return c.Classify
}

// Label returns the label of id in kind, or "".
func (c Catalog) Label(kind, id string) string {
	for _, o := range c.Options(kind) {
		if o.ID == id {
			return o.Label
		}
	}
	return ""
}

// FormatPricePerMillion renders a per-token price per million tokens.
func FormatPricePerMillion(perToken float64) string {
	perMillion := perToken * 1_000_000
	s := fmt.Sprintf("%.3f", perMillion)
	if perMillion >= 1 {
		s = fmt.Sprintf("%.2f", perMillion)
	}
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	return "$" + s
}

type rawModel struct {
	ID                  string         `json:"id"`
	Name                string         `json:"name"`
	Pricing             map[string]any `json:"pricing"`
	SupportedParameters []string       `json:"supported_parameters"`
	Architecture        struct {
		OutputModalities []string `json:"output_modalities"`
	} `json:"architecture"`
}

func price(v any) (float64, bool) {
	switch n := v.(type) {
	case nil:
		return 0, true
	case float64:
		return n, true
	case string:
		if n == "" {
			return 0, true
		}
		f, err := strconv.ParseFloat(n, 64)
		return f, err == nil
	}
	return 0, false
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func sortOptions(opts []Option, recommended string) {
	// The recommended entry first, then ~latest aliases (they self-update),
	// then everything else A-Z.
	sort.SliceStable(opts, func(a, b int) bool {
		ka := [2]bool{opts[a].ID != recommended, !strings.HasPrefix(opts[a].ID, "~")}
		kb := [2]bool{opts[b].ID != recommended, !strings.HasPrefix(opts[b].ID, "~")}
		if ka != kb {
			if ka[0] != kb[0] {
				return !ka[0]
			}
			return !ka[1]
		}
		return strings.ToLower(opts[a].Label) < strings.ToLower(opts[b].Label)
	})
}

func filterModels(models []rawModel, required string, promptCap, completionCap float64, recommended string) []Option {
	opts := []Option{}
	for _, m := range models {
		if m.ID == "" || !contains(m.SupportedParameters, required) {
			continue
		}
		p, ok1 := price(m.Pricing["prompt"])
		c, ok2 := price(m.Pricing["completion"])
		// Free variants are rate-limited and stall batch jobs.
		if !ok1 || !ok2 || p <= 0 || p > promptCap || c > completionCap {
			continue
		}
		name := m.Name
		if name == "" {
			name = m.ID
		}
		label := fmt.Sprintf("%s · %s/M in · %s/M out", name, FormatPricePerMillion(p), FormatPricePerMillion(c))
		if required == "web_search_options" {
			if s, ok := m.Pricing["web_search"]; ok {
				// float(pricing["web_search"]): None and "" do not parse.
				if f, ok := price(s); ok && s != nil && s != "" {
					label += fmt.Sprintf(" · search from $%s/1K requests", strconv.FormatFloat(f*1000, 'g', 6, 64))
				} else {
					label += " · search fees extra"
				}
			} else {
				label += " · search fees extra"
			}
		}
		opts = append(opts, Option{ID: m.ID, Label: label})
	}
	sortOptions(opts, recommended)
	return opts
}

func filterDecision(models []rawModel) []Option {
	opts := []Option{}
	for _, m := range models {
		if m.ID == "" || !contains(m.Architecture.OutputModalities, "decisions") {
			continue
		}
		p, ok := price(m.Pricing["prompt"])
		if !ok || p <= 0 || p > DecisionPromptPriceCap {
			continue
		}
		name := m.Name
		if name == "" {
			name = m.ID
		}
		opts = append(opts, Option{ID: m.ID, Label: fmt.Sprintf("%s · %s/M in", name, FormatPricePerMillion(p))})
	}
	sortOptions(opts, RecommendedDecisionModel)
	return opts
}

// Build derives every dropdown from one /models payload's "data" list.
func Build(data []json.RawMessage) Catalog {
	models := make([]rawModel, 0, len(data))
	for _, raw := range data {
		var m rawModel
		if json.Unmarshal(raw, &m) == nil {
			models = append(models, m)
		}
	}
	var research []rawModel
	for _, m := range models {
		if ResearchModels[m.ID] {
			research = append(research, m)
		}
	}
	return Catalog{
		Classify: filterModels(models, "tools", ModelPromptPriceCap, ModelCompletionPriceCap, RecommendedModel),
		Research: filterModels(research, "web_search_options", ResearchPromptPriceCap, ResearchCompletionPriceCap, RecommendedResearchModel),
		Decision: filterDecision(models),
	}
}

// Source fetches and caches the catalog. Safe for concurrent use.
type Source struct {
	URL    string
	APIKey string
	Client *http.Client
	Now    func() time.Time

	mu       sync.Mutex
	cached   Catalog
	cachedAt time.Time
	failedAt time.Time
}

// NewSource returns a Source against OpenRouter.
func NewSource(apiKey string) *Source {
	return &Source{URL: ModelsURL, APIKey: apiKey, Client: &http.Client{Timeout: 10 * time.Second}, Now: time.Now}
}

// Get returns the catalog, fetching when the cache is stale. It returns an
// empty catalog when OpenRouter is unreachable and nothing is cached; the UI
// then offers only the recommended defaults.
func (s *Source) Get(ctx context.Context) Catalog {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.Now()
	has := !s.cached.empty()
	if has && now.Sub(s.cachedAt) < ttl {
		return s.cached
	}
	if !has && !s.failedAt.IsZero() && now.Sub(s.failedAt) < retryAfter {
		return Catalog{}
	}
	cat, err := s.fetch(ctx)
	if err != nil {
		if has {
			s.cachedAt = now // keep serving the stale catalog
			return s.cached
		}
		s.failedAt = now
		return Catalog{}
	}
	if !cat.empty() {
		s.cached, s.cachedAt = cat, now
	}
	return cat
}

// Cached returns the last good catalog without fetching.
func (s *Source) Cached() Catalog {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cached
}

// Allowed reports whether id may be picked for kind: the recommended model
// always, otherwise only ids in the cached catalog (call Get first to warm).
func (s *Source) Allowed(kind, id string) bool {
	if id == Recommended(kind) {
		return true
	}
	for _, o := range s.Cached().Options(kind) {
		if o.ID == id {
			return true
		}
	}
	return false
}

func (s *Source) fetch(ctx context.Context) (Catalog, error) {
	// The fetch serves every waiting request and the cache, not only the
	// request that triggered it: a browser that navigates away must not
	// abort it and mark the catalog failed (no dropdowns, non-default
	// models refused) for the retry window.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL, nil)
	if err != nil {
		return Catalog{}, err
	}
	if s.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+s.APIKey)
	}
	resp, err := s.Client.Do(req)
	if err != nil {
		return Catalog{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Catalog{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&payload); err != nil {
		return Catalog{}, err
	}
	return Build(payload.Data), nil
}
