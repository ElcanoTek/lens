// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package openrouter is Lens's OpenRouter client: website, mobile-app and
// CTV classification through function calling with the IAB content taxonomy
// as the permitted vocabulary, web-search research for sites no crawler can
// reach and for CTV apps, retries with exponential backoff, a fallback model,
// truncated-response retries, and post-classification CTV news routing.
//
// It speaks the chat-completions API directly over net/http (the Python
// release used the OpenAI SDK). One [Client] is shared by every worker
// goroutine of a run.
//
// Every response OpenRouter returns has been billed, including ones that are
// then retried or rejected, so each one is recorded with cost.Record on the
// request's context before anything else looks at it.
package openrouter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ElcanoTek/lens/internal/categories/pyjson"
	"github.com/ElcanoTek/lens/internal/config"
	"github.com/ElcanoTek/lens/internal/cost"
	"github.com/ElcanoTek/lens/internal/logx"
)

// DefaultBaseURL is OpenRouter's OpenAI-compatible API root.
const DefaultBaseURL = "https://openrouter.ai/api/v1"

// maxResponseBytes caps a chat-completions body. Real replies are a few KiB;
// research summaries are bounded by max_tokens.
const maxResponseBytes = 16 << 20

var log = logx.New("openrouter_client")

// Options configure [New]. Zero values take the Python release's defaults
// except Temperature, which is used as given (0 is a valid temperature);
// pass config's llm_temperature / ctv_classification_temperature.
type Options struct {
	APIKey string
	// Model is the classification model (default config.DefaultLLMModel).
	Model string
	// FallbackModel replaces Model for the rest of the run once Model proves
	// unavailable (deprecated, unknown, or permanently rejecting requests).
	FallbackModel string
	Temperature   float64
	// MaxTokens is the classification budget (default 1500); truncated
	// replies are retried with double the budget, twice.
	MaxTokens int
	// MaxRetries is the number of attempts per request (default 2; at least
	// 1). config's llm_request_max_retries.
	MaxRetries int
	// Timeout bounds one HTTP request (0: none). config's llm_request_timeout.
	Timeout time.Duration
	// BaseURL overrides DefaultBaseURL (tests).
	BaseURL string
	// HTTPClient overrides the HTTP client (tests).
	HTTPClient *http.Client
}

// Client is safe for concurrent use.
type Client struct {
	apiKey        string
	baseURL       string
	http          *http.Client
	primaryModel  string
	fallbackModel string
	temperature   float64
	maxTokens     int
	maxRetries    int
	timeout       time.Duration

	mu             sync.Mutex
	model          string
	fallbackActive bool
	requestCount   int
	totalTokens    int

	// sleep waits between retries; replaced in tests.
	sleep func(context.Context, time.Duration) error
	// jitter returns the SDK backoff's random multiplier, in (0.75, 1];
	// replaced in tests.
	jitter func() float64
	// baseDelay is the backoff base (2s, doubling per attempt).
	baseDelay time.Duration
}

// New returns a client. The API key is required.
func New(opts Options) (*Client, error) {
	if opts.APIKey == "" {
		return nil, errors.New("OpenRouter API key is required for classification")
	}
	c := &Client{
		apiKey:        opts.APIKey,
		baseURL:       strings.TrimRight(opts.BaseURL, "/"),
		http:          opts.HTTPClient,
		primaryModel:  opts.Model,
		fallbackModel: opts.FallbackModel,
		temperature:   opts.Temperature,
		maxTokens:     opts.MaxTokens,
		maxRetries:    opts.MaxRetries,
		timeout:       opts.Timeout,
		sleep:         sleepCtx,
		jitter:        func() float64 { return 1 - 0.25*rand.Float64() }, //nolint:gosec // backoff jitter, not security
		baseDelay:     2 * time.Second,
	}
	if c.primaryModel == "" {
		c.primaryModel = config.DefaultLLMModel
	}
	c.model = c.primaryModel
	if c.baseURL == "" {
		c.baseURL = DefaultBaseURL
	}
	if c.http == nil {
		c.http = &http.Client{}
	}
	if c.maxTokens <= 0 {
		c.maxTokens = 1500
	}
	switch {
	case c.maxRetries == 0:
		c.maxRetries = 2
	case c.maxRetries < 0:
		c.maxRetries = 1
	}
	return c, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Model returns the active classification model (the fallback once it has
// been activated).
func (c *Client) Model() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.model
}

// FallbackActive reports whether the fallback model has replaced the primary.
func (c *Client) FallbackActive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fallbackActive
}

// Mode is the Classifier_Mode column value.
func (c *Client) Mode() string { return "openrouter" }

// UsageStats summarises the client's requests.
type UsageStats struct {
	RequestsMade            int
	TotalTokensUsed         int
	Model                   string
	AverageTokensPerRequest float64
}

// Usage returns request and token counters.
func (c *Client) Usage() UsageStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := UsageStats{RequestsMade: c.requestCount, TotalTokensUsed: c.totalTokens, Model: c.model}
	if c.requestCount > 0 {
		s.AverageTokensPerRequest = float64(c.totalTokens) / float64(c.requestCount)
	}
	return s
}

func (c *Client) countRequest(tokens int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requestCount++
	c.totalTokens += tokens
}

// APIError is a non-2xx HTTP response. Its text mirrors the OpenAI SDK's
// ("Error code: 404 - {...}") because the retry and fallback logic looks
// for status codes and provider phrases in it.
type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("Error code: %d - %s", e.StatusCode, e.Body)
}

// SDK-level retry policy. The Python release called OpenRouter through the
// OpenAI SDK built with max_retries=llm_request_max_retries, so every
// attempt of callWithRetry was itself retried by the SDK on transport
// failures, timeouts and 408/409/429/5xx replies (honouring Retry-After and
// x-should-retry). post keeps that inner layer; dropping it made rate-limit
// bursts fail items after a few seconds instead of riding them out.
const (
	sdkInitialRetryDelay = 500 * time.Millisecond
	sdkMaxRetryDelay     = 8 * time.Second
	sdkMaxRetryAfter     = 60 * time.Second
)

// post sends one chat-completions request and decodes the reply, retrying
// it up to maxRetries more times the way the OpenAI SDK did. A 2xx reply is
// recorded with the cost meters before it is inspected further.
func (c *Client) post(ctx context.Context, body pyjson.Object) (pyjson.Object, error) {
	payload := []byte(pyjson.DumpsUnicode(body))
	for retry := 0; ; retry++ {
		obj, retryAfter, retryable, err := c.postOnce(ctx, payload)
		if err == nil || !retryable || retry >= c.maxRetries || ctx.Err() != nil {
			return obj, err
		}
		delay := sdkRetryDelay(retry, retryAfter, c.jitter())
		log.Info(fmt.Sprintf("Retrying request to /chat/completions in %f seconds", delay.Seconds()))
		if err := c.sleep(ctx, delay); err != nil {
			return nil, err
		}
	}
}

// sdkRetryDelay is the OpenAI SDK's wait before retry number retry+1: the
// server's Retry-After when it asks for 0-60s, else 0.5s doubling up to 8s,
// shortened by up to 25% of jitter (jitter is the multiplier, in (0.75, 1]).
func sdkRetryDelay(retry int, retryAfter time.Duration, jitter float64) time.Duration {
	if retryAfter > 0 && retryAfter <= sdkMaxRetryAfter {
		return retryAfter
	}
	d := sdkInitialRetryDelay << min(retry, 16)
	if d > sdkMaxRetryDelay || d <= 0 {
		d = sdkMaxRetryDelay
	}
	return time.Duration(float64(d) * jitter)
}

// parseRetryAfter reads retry-after-ms, then retry-after (seconds or an
// HTTP date), as the SDK did; 0 when absent or unparseable.
func parseRetryAfter(h http.Header, now time.Time) time.Duration {
	if ms, err := strconv.ParseFloat(h.Get("Retry-After-Ms"), 64); err == nil {
		return time.Duration(ms * float64(time.Millisecond))
	}
	value := h.Get("Retry-After")
	if secs, err := strconv.ParseFloat(value, 64); err == nil {
		return time.Duration(secs * float64(time.Second))
	}
	if when, err := http.ParseTime(value); err == nil {
		return when.Sub(now)
	}
	return 0
}

// shouldRetryStatus is the SDK's rule for an error reply: x-should-retry
// wins, else 408, 409, 429 and 5xx are retried.
func shouldRetryStatus(resp *http.Response) bool {
	switch resp.Header.Get("X-Should-Retry") {
	case "true":
		return true
	case "false":
		return false
	}
	code := resp.StatusCode
	return code == http.StatusRequestTimeout || code == http.StatusConflict ||
		code == http.StatusTooManyRequests || code >= 500
}

// postOnce makes one HTTP request. retryable reports whether the SDK would
// have retried the failure; retryAfter is the server's requested wait.
func (c *Client) postOnce(ctx context.Context, payload []byte) (obj pyjson.Object, retryAfter time.Duration, retryable bool, err error) {
	parent := ctx
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, 0, false, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	for k, v := range config.OpenRouterHeaders {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		if parent.Err() != nil {
			return nil, 0, false, fmt.Errorf("Connection error: %w", err) //nolint:staticcheck // "connection" marks the error retryable
		}
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() != nil {
			return nil, 0, true, errors.New("Request timed out.") //nolint:revive,staticcheck // the OpenAI SDK's wording, which the retry classifier sees
		}
		return nil, 0, true, fmt.Errorf("Connection error: %w", err) //nolint:staticcheck // "connection" marks the error retryable
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() != nil && parent.Err() == nil {
			return nil, 0, true, errors.New("Request timed out.") //nolint:revive,staticcheck // the OpenAI SDK's wording, which the retry classifier sees
		}
		return nil, 0, parent.Err() == nil, fmt.Errorf("Connection error: reading response: %w", err) //nolint:staticcheck // "connection" marks the error retryable
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		text, _ := truncateRunes(strings.TrimSpace(string(raw)), 500)
		return nil, parseRetryAfter(resp.Header, time.Now()), shouldRetryStatus(resp), &APIError{StatusCode: resp.StatusCode, Body: text}
	}
	decoded, err := pyjson.Decode(raw)
	if err != nil {
		return nil, 0, false, fmt.Errorf("invalid JSON in OpenRouter response: %w", err)
	}
	o, ok := decoded.(pyjson.Object)
	if !ok {
		return nil, 0, false, errors.New("invalid OpenRouter response: not a JSON object")
	}
	usage, _ := o.Get("usage")
	modelValue, _ := o.Get("model")
	model, _ := modelValue.(string)
	cost.Record(ctx, cost.FromUsage(pyjson.Plain(usage)), model)
	return o, 0, false, nil
}

// bodyError digs out the upstream provider failure OpenRouter sometimes
// wraps inside an HTTP-200 body (a top-level "error" object with every other
// field null).
func bodyError(resp pyjson.Object) string {
	e, _ := resp.Get("error")
	if !truthy(e) {
		return ""
	}
	if obj, ok := e.(pyjson.Object); ok {
		msg, _ := obj.Get("message")
		text := "unknown error"
		if truthy(msg) {
			text = pyStr(msg)
		}
		if code, ok := obj.Get("code"); ok && code != nil {
			return fmt.Sprintf("OpenRouter body error (code=%s): %s", pyStr(code), text)
		}
		return "OpenRouter body error: " + text
	}
	return "OpenRouter body error: " + pyStr(e)
}

// isModelUnavailable detects errors meaning the configured model is gone
// (deprecated, removed, unknown to the provider) or permanently rejects our
// payload. Both call for the fallback model rather than a normal retry.
func isModelUnavailable(errText string) bool {
	lowered := strings.ToLower(errText)
	for _, sig := range []string{"deprecated", "not found", "no endpoints", "no allowed providers"} {
		if strings.Contains(lowered, sig) {
			return true
		}
	}
	// OpenRouter wraps upstream provider rejections as a 502-coded body
	// error ("Invalid arguments passed to the model.") on an HTTP 200, so
	// retries don't help; only a model swap will.
	return strings.Contains(lowered, "openrouter body error") &&
		(strings.Contains(lowered, "invalid arguments") || strings.Contains(lowered, "code=502") ||
			strings.Contains(lowered, "code=400"))
}

var retryableKeywords = []string{
	"connection", "timeout", "rate limit", "429", "503", "502", "500",
	"network", "reset", "refused", "unavailable", "overloaded",
}

// activateFallback swaps the active model to the fallback (idempotent). It
// reports whether the swap happened on this call.
func (c *Client) activateFallback(reason string) bool {
	if c.fallbackModel == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fallbackActive {
		return false
	}
	log.Warn(fmt.Sprintf("Activating OpenRouter fallback model %s (primary %s failed: %s)",
		pyRepr(c.fallbackModel), pyRepr(c.primaryModel), reason))
	c.model = c.fallbackModel
	c.fallbackActive = true
	return true
}

// callWithRetry runs build+post up to maxRetries times with exponential
// backoff. build is called per attempt, so a fallback swap mid-retry takes
// effect on the next attempt.
func (c *Client) callWithRetry(ctx context.Context, build func() pyjson.Object, op string) (pyjson.Object, error) {
	var last error
	for attempt := 0; attempt < c.maxRetries; attempt++ {
		resp, err := c.post(ctx, build())
		if err == nil {
			if msg := bodyError(resp); msg != "" {
				// Surface the body-embedded provider error so it is retried
				// or triggers the fallback like any other failure.
				err = errors.New(msg)
			} else {
				return resp, nil
			}
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		last = err
		errText, _ := truncateRunes(err.Error(), 150)

		// A model that is unavailable gets swapped for the fallback at once.
		if isModelUnavailable(errText) && !c.FallbackActive() && c.activateFallback(errText) {
			log.Warn(fmt.Sprintf("%s retrying with fallback model after primary unavailable: %s", op, errText))
			continue
		}

		lowered := strings.ToLower(errText)
		retryable := false
		for _, kw := range retryableKeywords {
			if strings.Contains(lowered, kw) {
				retryable = true
				break
			}
		}
		switch {
		case attempt < c.maxRetries-1 && retryable:
			delay := c.baseDelay * time.Duration(1<<attempt)
			log.Warn(fmt.Sprintf("%s attempt %d/%d failed: %s. Retrying in %.1fs...",
				op, attempt+1, c.maxRetries, errText, delay.Seconds()))
			if err := c.sleep(ctx, delay); err != nil {
				return nil, err
			}
		case attempt < c.maxRetries-1:
			// Non-retryable, but still try once more after a short delay.
			log.Warn(fmt.Sprintf("%s attempt %d/%d failed (non-retryable): %s. Trying once more in %.1fs...",
				op, attempt+1, c.maxRetries, errText, c.baseDelay.Seconds()))
			if err := c.sleep(ctx, c.baseDelay); err != nil {
				return nil, err
			}
		default:
			log.Error(fmt.Sprintf("%s failed after %d attempts: %s", op, c.maxRetries, errText))
		}
	}
	if last == nil {
		return nil, fmt.Errorf("%s failed without executing any attempts (max_retries=%d)", op, c.maxRetries)
	}
	return nil, last
}

// parseError is an unparseable model reply: retried with a larger budget,
// then surfaced so the item is recorded as Failed (never a guessed default).
type parseError struct{ msg string }

func (e *parseError) Error() string { return e.msg }

// requestAndParse calls the API and parses the structured reply, doubling
// max_tokens and retrying (truncationRetries times) when the reply is
// truncated or unparseable. Reasoning models can spend the whole budget
// "thinking" and be cut off (finish_reason "length") before emitting the
// tool call, so a larger budget is the reliable fix.
func (c *Client) requestAndParse(ctx context.Context, build func(budget int) pyjson.Object,
	parse func(pyjson.Object) (Classification, error), op string, initialBudget int,
) (Classification, pyjson.Object, error) {
	const truncationRetries = 2
	budget := initialBudget
	if budget <= 0 {
		budget = c.maxTokens
	}
	var last error
	for attempt := 0; attempt <= truncationRetries; attempt++ {
		resp, err := c.callWithRetry(ctx, func() pyjson.Object { return build(budget) }, op)
		if err != nil {
			return Classification{}, nil, err
		}
		c.countRequest(usageTokens(resp))
		result, err := parse(resp)
		if err == nil {
			return result, resp, nil
		}
		var pe *parseError
		if !errors.As(err, &pe) {
			return Classification{}, nil, err
		}
		last = err
		reason := finishReason(resp)
		if attempt < truncationRetries {
			budget *= 2
			log.Warn(fmt.Sprintf("%s returned an unparseable response (finish_reason=%s): %v; retrying with max_tokens=%d",
				op, reason, err, budget))
		} else {
			log.Error(fmt.Sprintf("%s produced no parseable response after %d attempts (last finish_reason=%s)",
				op, truncationRetries+1, reason))
		}
	}
	return Classification{}, nil, last
}

// classificationOptions reserves output capacity on Gemini classifiers: low
// reasoning effort leaves room for the structured answer (merely hiding
// reasoning text would still bill the same tokens). Evaluated per request
// against the active model, so a fallback model keeps its own defaults.
// Research requests never get it.
func classificationOptions(model string) []pyjson.Member {
	if strings.HasPrefix(strings.TrimLeft(model, "~"), "google/gemini-") {
		return []pyjson.Member{{Key: "reasoning", Value: pyjson.Object{{Key: "effort", Value: "low"}}}}
	}
	return nil
}

func messages(system, user string) []any {
	return []any{
		pyjson.Object{{Key: "role", Value: "system"}, {Key: "content", Value: system}},
		pyjson.Object{{Key: "role", Value: "user"}, {Key: "content", Value: user}},
	}
}

func toolChoice(name string) pyjson.Object {
	return pyjson.Object{
		{Key: "type", Value: "function"},
		{Key: "function", Value: pyjson.Object{{Key: "name", Value: name}}},
	}
}

// TestConnection sends a tiny request with the active model. When the
// primary model is unavailable and a fallback is configured, it swaps to
// the fallback and tries once more.
func (c *Client) TestConnection(ctx context.Context) bool {
	attempt := func() (bool, error) {
		model := c.Model()
		resp, err := c.post(ctx, pyjson.Object{
			{Key: "model", Value: model},
			{Key: "messages", Value: messages("You are a professional website quality analyst.",
				"Respond with 'OK' if you can read this message.")},
			{Key: "temperature", Value: 0.0},
			{Key: "max_tokens", Value: 16},
		})
		if err != nil {
			return false, err
		}
		if msg := bodyError(resp); msg != "" {
			return false, errors.New(msg)
		}
		if choices, _ := resp.Get("choices"); truthy(choices) {
			log.Info(fmt.Sprintf("OpenRouter API connection test successful (model=%s)", model))
			return true, nil
		}
		log.Error("OpenRouter API connection test failed: Invalid response format")
		return false, nil
	}
	ok, err := attempt()
	if err == nil {
		return ok
	}
	log.Error(fmt.Sprintf("OpenRouter API connection test failed for model %s: %v", pyRepr(c.Model()), err))
	if isModelUnavailable(err.Error()) && !c.FallbackActive() && c.activateFallback(err.Error()) {
		ok, err = attempt()
		if err == nil {
			return ok
		}
		log.Error(fmt.Sprintf("OpenRouter API fallback connection test failed for model %s: %v", pyRepr(c.Model()), err))
	}
	return false
}

// ---- small helpers mirroring Python semantics ----

func truncateRunes(s string, n int) (string, bool) {
	if utf8.RuneCountInString(s) <= n {
		return s, false
	}
	i := 0
	for count := 0; count < n; count++ {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return s[:i], true
}

// truthy is Python's bool() for decoded JSON values.
func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case int64:
		return t != 0
	case float64:
		return t != 0
	case []any:
		return len(t) > 0
	case pyjson.Object:
		return len(t) > 0
	}
	return true
}

// pyStr is Python's str() for decoded JSON values.
func pyStr(v any) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case string:
		return t
	case bool:
		if t {
			return "True"
		}
		return "False"
	case int64:
		return fmt.Sprint(t)
	case float64:
		return pyjson.FormatFloat(t)
	}
	return pyjson.DumpsUnicode(v)
}

// pyRepr is Python's %r for a string.
func pyRepr(s string) string {
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		return `"` + s + `"`
	}
	return "'" + strings.ReplaceAll(s, "'", `\'`) + "'"
}

// truncatedDump is json.dumps(response, default=str)[:1500] for logs.
func truncatedDump(v any) string {
	s, _ := truncateRunes(pyjson.Dumps(v), 1500)
	return s
}
