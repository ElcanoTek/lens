// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package config loads Lens's settings: built-in defaults, then config.json,
// then the environment (secrets only). Unlike the Python release it never
// fails at load time for a missing OPENROUTER_API_KEY; the commands that need
// the key ask for it with RequireAPIKey.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"reflect"
	"strings"

	"github.com/ElcanoTek/lens/internal/dotenv"
)

// DefaultUserAgent looks like a current browser: an ancient Chrome version is
// itself a bot signal to basic filters.
const DefaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/137.0.0.0 Safari/537.36"

// legacyUserAgents are the exact defaults older releases shipped. A persisted
// config.json still carrying one meant "the default", not a deliberate
// choice, so it is upgraded.
var legacyUserAgents = map[string]bool{
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36": true,
}

// deadLLMModels are model IDs OpenRouter now permanently 404s. A config.json
// pinning one would burn a failed request per call and silently fall back;
// upgrade the pin instead. Only add an ID once it is dead for good.
var deadLLMModels = map[string]bool{"x-ai/grok-4.1-fast": true}

// Model defaults. DefaultLLMModel is OpenRouter's moving alias for the GPT
// Luna family.
const (
	DefaultLLMModel = "~openai/gpt-luna-latest"
	// DefaultDecisionModel is OpenRouter's moving alias for TypeSafe's Jev.
	DefaultDecisionModel = "~typesafe/jev-latest"
	// DefaultResearchModel matched Sonar Pro's quality tiers on 96% of a
	// production CTV sample at about a third of the cost; see
	// docs/RESEARCH_MODELS.md.
	DefaultResearchModel = "perplexity/sonar"
	DefaultFallbackModel = "~openai/gpt-mini-latest"
)

// PreviousDefaultLLMModels are exact strings older releases wrote as the
// shipped default. A config.json still naming one accepted the default; it
// did not pick a model.
var PreviousDefaultLLMModels = map[string]bool{"~google/gemini-flash-latest": true}

// Config is every tunable. JSON names match config.json keys.
type Config struct {
	InputCSVPath     string `json:"input_csv_path"`
	OutputCSVPath    string `json:"output_csv_path"`
	ProgressFilePath string `json:"progress_file_path"`
	LogFilePath      string `json:"log_file_path"`

	ConcurrentSessions  int     `json:"concurrent_sessions"`
	LLMModel            string  `json:"llm_model"`
	LLMFallbackModel    string  `json:"llm_fallback_model"`
	LLMTemperature      float64 `json:"llm_temperature"`
	LLMMaxTokens        int     `json:"llm_max_tokens"`
	LLMRequestRetries   int     `json:"llm_request_max_retries"`
	LLMRequestTimeout   float64 `json:"llm_request_timeout"`
	RequestTimeout      float64 `json:"request_timeout"`
	MaxRetries          int     `json:"max_retries"`
	RetryDelay          float64 `json:"retry_delay"`
	SessionTimeout      float64 `json:"session_timeout"`
	UserAgent           string  `json:"user_agent"`
	MinContentLength    int     `json:"min_content_length"`
	ScrapeMode          string  `json:"scrape_mode"`
	RetryFollowRedirect bool    `json:"retry_follow_redirects"`
	SkipDeepRetryOnBlk  bool    `json:"skip_deep_retry_on_block"`
	DeepRetryEnabled    bool    `json:"deep_retry_enabled"`

	ResearchFallbackEnabled bool    `json:"research_fallback_enabled"`
	ResearchModel           string  `json:"research_model"`
	DecisionModel           string  `json:"decision_model"`
	ResearchTemperature     float64 `json:"research_temperature"`
	ResearchMaxTokens       int     `json:"research_max_tokens"`
	ResearchMaxConcurrent   int     `json:"research_max_concurrent"`

	DeepScrapePodmanBinary  string   `json:"deep_scrape_podman_binary"`
	DeepScrapeImage         string   `json:"deep_scrape_image"`
	DeepScrapeContainerName string   `json:"deep_scrape_container_name"`
	DeepScrapePort          int      `json:"deep_scrape_port"`
	DeepScrapeVNCPort       int      `json:"deep_scrape_vnc_port"`
	DeepScrapeExtraArgs     []string `json:"deep_scrape_extra_args"`
	DeepScrapeStartup       float64  `json:"deep_scrape_startup_timeout"`
	DeepScrapeWaitAfterLoad float64  `json:"deep_scrape_wait_after_load"`
	DeepScrapeKeepContainer bool     `json:"deep_scrape_keep_container"`
	DeepScrapePullPolicy    string   `json:"deep_scrape_pull_policy"`

	// RejectRedirects fails URLs that redirect to a different host.
	RejectRedirects bool `json:"reject_redirects"`

	IOSRequestTimeout float64 `json:"ios_request_timeout"`
	IOSRequestDelay   float64 `json:"ios_request_delay"`
	IOSMaxConcurrent  int     `json:"ios_max_concurrent"`
	IOSMaxRetries     int     `json:"ios_max_retries"`
	IOSRetryDelay     float64 `json:"ios_retry_delay"`

	AndroidRequestTimeout float64 `json:"android_request_timeout"`
	AndroidRequestDelay   float64 `json:"android_request_delay"`
	AndroidMaxConcurrent  int     `json:"android_max_concurrent"`
	AndroidMaxRetries     int     `json:"android_max_retries"`
	AndroidRetryDelay     float64 `json:"android_retry_delay"`

	CTVResearchModel           string  `json:"ctv_research_model"`
	CTVResearchTemperature     float64 `json:"ctv_research_temperature"`
	CTVResearchMaxTokens       int     `json:"ctv_research_max_tokens"`
	CTVClassificationModel     string  `json:"ctv_classification_model"`
	CTVClassificationFallback  string  `json:"ctv_classification_fallback_model"`
	CTVClassificationTemp      float64 `json:"ctv_classification_temperature"`
	CTVClassificationMaxTokens int     `json:"ctv_classification_max_tokens"`
	CTVMaxConcurrent           int     `json:"ctv_max_concurrent"`
	CTVRequestDelay            float64 `json:"ctv_request_delay"`
	CTVMaxRetries              int     `json:"ctv_max_retries"`
	CTVRetryDelay              float64 `json:"ctv_retry_delay"`

	// OpenRouterAPIKey comes from the environment only, never config.json.
	OpenRouterAPIKey string `json:"-"`
	// Warnings collects what loading changed or ignored (printed by callers).
	Warnings []string `json:"-"`
}

// Default returns the built-in configuration.
func Default() *Config {
	return &Config{
		InputCSVPath:     "input.csv",
		OutputCSVPath:    "output.csv",
		ProgressFilePath: "progress.json",
		LogFilePath:      "site_analysis.log",

		ConcurrentSessions: 5,
		LLMModel:           DefaultLLMModel,
		LLMFallbackModel:   DefaultFallbackModel,
		LLMTemperature:     0.1,
		LLMMaxTokens:       1500,
		LLMRequestRetries:  3,
		LLMRequestTimeout:  60,
		RequestTimeout:     30,
		MaxRetries:         3,
		RetryDelay:         2,
		SessionTimeout:     300,
		UserAgent:          DefaultUserAgent,
		MinContentLength:   500,
		// Full rescue ladder by default (direct -> deep -> research). The deep
		// rung is probed at runtime and skipped when Podman isn't available.
		ScrapeMode: "auto",
		// The deep retry pass exists to rescue hard sites, so it follows
		// cross-host redirects the fast direct pass rejects (brand domain ->
		// parent publisher). Formerly firecrawl_follow_redirects.
		RetryFollowRedirect: true,
		// Don't escalate a site the direct pass proved is blocking this IP:
		// the deep pass renders from the same datacenter IP.
		SkipDeepRetryOnBlk: true,
		DeepRetryEnabled:   false,

		ResearchFallbackEnabled: true,
		ResearchModel:           DefaultResearchModel,
		DecisionModel:           DefaultDecisionModel,
		ResearchTemperature:     0.2,
		ResearchMaxTokens:       1500,
		// Research is API-bound (no crawling from this host), so it runs
		// independently of concurrent_sessions.
		ResearchMaxConcurrent: 12,

		DeepScrapePodmanBinary:  "podman",
		DeepScrapeImage:         "docker.io/selenium/standalone-chrome:latest",
		DeepScrapeContainerName: "elcano-lens-chromedriver",
		DeepScrapePort:          4444,
		DeepScrapeVNCPort:       7900,
		DeepScrapeExtraArgs:     []string{},
		DeepScrapeStartup:       45,
		DeepScrapeWaitAfterLoad: 1.0,
		DeepScrapeKeepContainer: false,
		DeepScrapePullPolicy:    "always",

		RejectRedirects: true,

		IOSRequestTimeout: 30,
		IOSRequestDelay:   5.0,
		IOSMaxConcurrent:  1,
		IOSMaxRetries:     3,
		IOSRetryDelay:     3.0,

		AndroidRequestTimeout: 30,
		AndroidRequestDelay:   10.0,
		AndroidMaxConcurrent:  1,
		AndroidMaxRetries:     3,
		AndroidRetryDelay:     5.0,

		CTVResearchModel:           DefaultResearchModel,
		CTVResearchTemperature:     0.3,
		CTVResearchMaxTokens:       2000,
		CTVClassificationModel:     DefaultLLMModel,
		CTVClassificationFallback:  DefaultFallbackModel,
		CTVClassificationTemp:      0.1,
		CTVClassificationMaxTokens: 1500,
		CTVMaxConcurrent:           5,
		CTVRequestDelay:            1.0,
		CTVMaxRetries:              3,
		CTVRetryDelay:              2.0,
	}
}

// LoadDotenv loads the nearest .env (cwd upward) into the environment without
// overriding variables that are already set. An unreadable file is ignored:
// systemd already injected it via EnvironmentFile, and a root-owned 0600 .env
// must not kill a job before it can log.
func LoadDotenv() {
	if path := dotenv.Find(); path != "" {
		_ = dotenv.Load(path, false)
	}
}

// Load returns the defaults overlaid with path (when it exists) and the
// OpenRouter key from the environment. A malformed config.json is reported
// in Warnings and otherwise ignored, as the Python release did.
func Load(path string) *Config {
	cfg := Default()
	raw, err := os.ReadFile(path) //nolint:gosec // operator-chosen path
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		cfg.warnf("Could not load config file %s: %v", path, err)
	default:
		if err := cfg.apply(raw); err != nil {
			cfg = Default()
			cfg.warnf("Could not load config file %s: %v", path, err)
		}
	}
	cfg.OpenRouterAPIKey = strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY"))
	return cfg
}

func (c *Config) warnf(format string, args ...any) {
	c.Warnings = append(c.Warnings, fmt.Sprintf(format, args...))
}

// RequireAPIKey errors when OPENROUTER_API_KEY is missing.
func (c *Config) RequireAPIKey() error {
	if c.OpenRouterAPIKey == "" {
		return errors.New("OPENROUTER_API_KEY environment variable is required for classification")
	}
	return nil
}

// apply overlays a config.json document. Values are converted leniently
// (a float where an int is expected is accepted when whole) so a file that
// worked with the Python release keeps working; unknown keys are ignored.
func (c *Config) apply(raw []byte) error {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	if doc == nil {
		return errors.New("config file must be a JSON object")
	}

	// Firecrawl was removed: keep old config.json files working.
	if legacy, ok := doc["firecrawl_follow_redirects"]; ok {
		if _, explicit := doc["retry_follow_redirects"]; !explicit {
			doc["retry_follow_redirects"] = legacy
		}
	}
	for key := range doc {
		if strings.HasPrefix(key, "firecrawl_") {
			delete(doc, key)
		}
	}
	if mode, _ := doc["scrape_mode"].(string); mode == "firecrawl" {
		c.warnf("scrape_mode 'firecrawl' was removed; using 'auto' instead")
		doc["scrape_mode"] = "auto"
	}

	v := reflect.ValueOf(c).Elem()
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		value, ok := doc[name]
		if !ok {
			continue
		}
		if value == nil {
			// null means "off" for the optional settings the Python release
			// read as None (no fallback model, no VNC port); for every other
			// key it keeps the default.
			if nullDisables[name] {
				v.Field(i).SetZero()
			}
			continue
		}
		if err := assign(v.Field(i), value); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}

	if legacyUserAgents[c.UserAgent] {
		c.UserAgent = DefaultUserAgent
	}
	for _, field := range []struct {
		key string
		ptr *string
	}{{"llm_model", &c.LLMModel}, {"ctv_classification_model", &c.CTVClassificationModel}} {
		configured := *field.ptr
		if deadLLMModels[configured] || PreviousDefaultLLMModels[configured] {
			reason := "is a previous shipped default"
			if deadLLMModels[configured] {
				reason = "is deprecated on OpenRouter"
			}
			c.warnf("configured %s %q %s; using %q instead", field.key, configured, reason, DefaultLLMModel)
			*field.ptr = DefaultLLMModel
		}
	}
	return nil
}

// nullDisables are the keys whose JSON null turns the feature off, as None
// did in the Python release: the classification fallback models and the
// deep crawler's VNC port.
var nullDisables = map[string]bool{
	"llm_fallback_model":                true,
	"ctv_classification_fallback_model": true,
	"deep_scrape_vnc_port":              true,
}

func assign(dst reflect.Value, value any) error {
	switch dst.Kind() {
	case reflect.String:
		s, ok := value.(string)
		if !ok {
			return fmt.Errorf("want a string, got %T", value)
		}
		dst.SetString(s)
	case reflect.Bool:
		b, ok := value.(bool)
		if !ok {
			return fmt.Errorf("want true or false, got %T", value)
		}
		dst.SetBool(b)
	case reflect.Int:
		f, ok := value.(float64)
		if !ok || f != math.Trunc(f) {
			return fmt.Errorf("want a whole number, got %v", value)
		}
		dst.SetInt(int64(f))
	case reflect.Float64:
		f, ok := value.(float64)
		if !ok {
			return fmt.Errorf("want a number, got %T", value)
		}
		dst.SetFloat(f)
	case reflect.Slice:
		items, ok := value.([]any)
		if !ok {
			return fmt.Errorf("want a list, got %T", value)
		}
		out := make([]string, 0, len(items))
		for _, it := range items {
			s, ok := it.(string)
			if !ok {
				return fmt.Errorf("want a list of strings, got %T", it)
			}
			out = append(out, s)
		}
		dst.Set(reflect.ValueOf(out))
	default:
		return fmt.Errorf("unsupported field kind %s", dst.Kind())
	}
	return nil
}

// WriteJSON writes the effective configuration (never the API key) as
// indented JSON, for `lens manage setup` and diagnostics.
func (c *Config) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(c)
}

// CSVFieldnames is the unified output schema for websites and apps.
var CSVFieldnames = []string{
	"Domain",            // identifier: domain, app ID or package name
	"Type",              // WEBSITE, IOS or ANDROID
	"App_Name",          // empty for websites
	"Developer",         // empty for websites
	"Store_Category",    // empty for websites
	"Rating",            // empty for websites
	"Rating_Count",      // empty for websites
	"Downloads",         // Android only
	"Quality",           // Premium, Standard, Long Tail, Failed
	"Justification",     // why that tier
	"IAB Tier 1",        // IAB category tier 1
	"IAB Tier 2",        // IAB category tier 2
	"IAB Tier 3",        // IAB category tier 3
	"Description",       // one sentence
	"Language",          // primary language of the content
	"Political_Leaning", // Far Left ... Far Right, Non-Political
	"Audience_Size",     // XS, S, M, L, XL
	"Bot_Protection",    // websites only: None Detected, Moderate, Aggressive, Unknown
	"Content_Length",    // characters
	"Processing_Time",   // seconds
	"Scrape_Mode",       // direct, deep, research, ios_api, android_scrape
	"Classifier_Mode",   // openrouter
	"Scraped_At",        // ISO timestamp of the fetch
	"Cost_USD",          // OpenRouter-reported spend for this row
}

// CTVCSVFieldnames is the CTV output schema.
var CTVCSVFieldnames = []string{
	"App_Name",
	"Type",
	"Bundle_ID",
	"SSP",
	"Publisher",
	"Platform",
	"URL",
	"Quality",
	"Justification",
	"IAB Tier 1",
	"IAB Tier 2",
	"IAB Tier 3",
	"Description",
	"Target_Audience",
	"Content_Type",
	"Language",
	"Political_Leaning",
	"Network_Affiliation",
	"Audience_Size",
	"Research_Summary",
	"Processing_Time",
	"Research_Model",
	"Classification_Model",
	"Processed_At",
	"Cost_USD",
}

// OpenRouterHeaders are the attribution headers every OpenRouter call sends
// besides Authorization.
var OpenRouterHeaders = map[string]string{
	"HTTP-Referer": "https://github.com/ElcanoTek/lens",
	"X-Title":      "Lens",
}
