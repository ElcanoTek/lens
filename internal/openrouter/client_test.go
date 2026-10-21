// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package openrouter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ElcanoTek/lens/internal/categories/pyjson"
	"github.com/ElcanoTek/lens/internal/cost"
	"github.com/ElcanoTek/lens/internal/logx"
)

type reply struct {
	status int
	body   string
}

type fakeAPI struct {
	mu      sync.Mutex
	replies []reply
	bodies  []map[string]any
	headers []http.Header
	paths   []string
	srv     *httptest.Server
}

// newFakeAPI serves replies in order, repeating the last one.
func newFakeAPI(t *testing.T, replies ...reply) *fakeAPI {
	t.Helper()
	f := &fakeAPI{replies: replies}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("request body is not JSON: %v", err)
		}
		f.mu.Lock()
		f.bodies = append(f.bodies, body)
		f.headers = append(f.headers, r.Header.Clone())
		f.paths = append(f.paths, r.URL.Path)
		rep := f.replies[min(len(f.bodies)-1, len(f.replies)-1)]
		f.mu.Unlock()
		if rep.status == 0 {
			rep.status = 200
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rep.status)
		_, _ = io.WriteString(w, rep.body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

func (f *fakeAPI) body(i int) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[i]
}

type sleeps struct {
	mu sync.Mutex
	d  []time.Duration
}

func (s *sleeps) sleep(_ context.Context, d time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.d = append(s.d, d)
	return nil
}

func newTestClient(t *testing.T, f *fakeAPI, opts Options) (*Client, *sleeps) {
	t.Helper()
	opts.BaseURL = f.srv.URL + "/api/v1"
	if opts.APIKey == "" {
		opts.APIKey = "test-key"
	}
	c, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	s := &sleeps{}
	c.sleep = s.sleep
	return c, s
}

var defaultArgs = map[string]any{
	"quality":           "Premium",
	"justification":     "test",
	"vertical":          "Games",
	"description":       "test",
	"confidence":        "High",
	"language":          "English",
	"political_leaning": "Non-Political",
	"audience_size":     "M",
}

func toolReply(name string, args map[string]any, extra string) string {
	encoded, _ := json.Marshal(args)
	argText, _ := json.Marshal(string(encoded))
	return `{"model": "openai/gpt-6-luna", "choices": [{"finish_reason": "stop", "message": {"tool_calls": [{"function": {"name": "` +
		name + `", "arguments": ` + string(argText) + `}}]}}], "usage": {"total_tokens": 10` + extra + `}}`
}

const truncatedReply = `{"choices": [{"finish_reason": "length", "message": {"content": null, "tool_calls": null}}], "usage": {"total_tokens": 1500}}`

func contentReply(content string) string {
	text, _ := json.Marshal(content)
	return `{"model": "perplexity/sonar", "choices": [{"message": {"content": ` + string(text) + `}}], "usage": {"total_tokens": 10}}`
}

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	logx.SetOutput(&buf, slog.LevelDebug)
	t.Cleanup(func() { logx.SetOutput(io.Discard, slog.LevelWarn) })
	return &buf
}

func TestNewRequiresKeyAndDefaults(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("missing key accepted")
	}
	c, err := New(Options{APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Model() != "~openai/gpt-luna-latest" || c.maxTokens != 1500 || c.maxRetries != 2 || c.baseURL != DefaultBaseURL {
		t.Fatalf("defaults: %+v", c)
	}
	if c.Mode() != "openrouter" {
		t.Fatal("mode")
	}
}

func TestRequestHeadersAndSiteBody(t *testing.T) {
	f := newFakeAPI(t, reply{body: toolReply("classify_website", defaultArgs, "")})
	c, _ := newTestClient(t, f, Options{APIKey: "sk-test", Model: "openai/x", Temperature: 0.1})
	result := c.ClassifySite(context.Background(), SiteInput{Domain: "example.com", Content: "some content"})
	if !result.Success || result.Quality != "Premium" || result.Source != "openrouter" || result.TokensUsed != 10 {
		t.Fatalf("result = %+v", result)
	}
	h := f.headers[0]
	if h.Get("Authorization") != "Bearer sk-test" || h.Get("HTTP-Referer") != "https://github.com/ElcanoTek/lens" ||
		h.Get("X-Title") != "Lens" || h.Get("Content-Type") != "application/json" {
		t.Fatalf("headers = %v", h)
	}
	if f.paths[0] != "/api/v1/chat/completions" {
		t.Fatalf("path = %s", f.paths[0])
	}
	body := f.body(0)
	if body["model"] != "openai/x" || body["max_tokens"] != 1500.0 || body["temperature"] != 0.1 ||
		body["top_p"] != 0.9 || body["frequency_penalty"] != 0.1 || body["presence_penalty"] != 0.1 {
		t.Fatalf("body = %v", body)
	}
	choice := body["tool_choice"].(map[string]any)
	if choice["function"].(map[string]any)["name"] != "classify_website" {
		t.Fatalf("tool_choice = %v", choice)
	}
	msgs := body["messages"].([]any)
	if !strings.Contains(msgs[1].(map[string]any)["content"].(string), "Domain: example.com") {
		t.Fatal("prompt missing domain")
	}
	tool := body["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if tool["name"] != "classify_website" {
		t.Fatalf("tool = %v", tool["name"])
	}
	if u := c.Usage(); u.RequestsMade != 1 || u.TotalTokensUsed != 10 || u.AverageTokensPerRequest != 10 {
		t.Fatalf("usage = %+v", u)
	}
}

func TestClassifySiteRetriesTruncatedResponseWithLargerBudget(t *testing.T) {
	f := newFakeAPI(t, reply{body: truncatedReply}, reply{body: toolReply("classify_website", defaultArgs, "")})
	c, _ := newTestClient(t, f, Options{MaxTokens: 1500})
	result := c.ClassifySite(context.Background(), SiteInput{Domain: "example.com", Content: "some content"})
	if !result.Success || result.Quality != "Premium" {
		t.Fatalf("result = %+v", result)
	}
	if f.calls() != 2 || f.body(0)["max_tokens"] != 1500.0 || f.body(1)["max_tokens"] != 3000.0 {
		t.Fatalf("budgets: %v %v", f.body(0)["max_tokens"], f.body(1)["max_tokens"])
	}
}

func TestClassifySiteFailsWhenEveryAttemptTruncates(t *testing.T) {
	f := newFakeAPI(t, reply{body: truncatedReply})
	c, _ := newTestClient(t, f, Options{MaxTokens: 1500})
	result := c.ClassifySite(context.Background(), SiteInput{Domain: "example.com", Content: "some content"})
	if result.Success || result.Quality != "Error" || !strings.Contains(result.Justification, "Classification failed") {
		t.Fatalf("result = %+v", result)
	}
	var budgets []any
	for i := 0; i < f.calls(); i++ {
		budgets = append(budgets, f.body(i)["max_tokens"])
	}
	// initial attempt + two truncation retries, each with a doubled budget
	if !reflect.DeepEqual(budgets, []any{1500.0, 3000.0, 6000.0}) {
		t.Fatalf("budgets = %v", budgets)
	}
	want := failedClassification(errors.New(result.Error), result.ProcessingTime)
	if result != want {
		t.Fatalf("failure shape = %+v", result)
	}
}

func TestClassificationReasoningEffortReachesAPI(t *testing.T) {
	for _, kind := range []string{"website", "app", "ctv"} {
		for _, model := range []string{"~google/gemini-flash-latest", "google/gemini-2.5-flash", "~openai/gpt-luna-latest", "other/model"} {
			name := map[string]string{"website": "classify_website", "app": "classify_app", "ctv": "classify_ctv_app"}[kind]
			f := newFakeAPI(t, reply{body: toolReply(name, defaultArgs, "")})
			c, _ := newTestClient(t, f, Options{Model: model})
			var result Classification
			switch kind {
			case "website":
				result = c.ClassifySite(context.Background(), SiteInput{Domain: "example.com", Content: "Source evidence"})
			case "app":
				result = c.ClassifyApp(context.Background(), AppInput{AppID: "123", AppName: "Example", ContentForLLM: "Source evidence"})
			default:
				result = c.ClassifyCTVApp(context.Background(), CTVApp{AppName: "Example"}, "Source evidence", CTVClassifyOptions{})
			}
			if !result.Success {
				t.Fatalf("%s/%s: %+v", kind, model, result)
			}
			reasoning, has := f.body(0)["reasoning"]
			gemini := strings.HasPrefix(strings.TrimLeft(model, "~"), "google/gemini-")
			if gemini && !reflect.DeepEqual(reasoning, map[string]any{"effort": "low"}) {
				t.Errorf("%s/%s: reasoning = %v", kind, model, reasoning)
			}
			if !gemini && has {
				t.Errorf("%s/%s: unexpected reasoning", kind, model)
			}
		}
	}
}

func TestResearchNeverGetsReasoningOrTools(t *testing.T) {
	f := newFakeAPI(t, reply{body: contentReply(strings.Repeat("Factual source evidence. ", 30))})
	c, _ := newTestClient(t, f, Options{Model: "google/gemini-2.5-flash"})
	r := c.ResearchWebsite(context.Background(), "example.com", ResearchOptions{Temperature: 0.2})
	if !r.Success || r.Model != "perplexity/sonar" {
		t.Fatalf("research = %+v", r)
	}
	body := f.body(0)
	for _, key := range []string{"reasoning", "tools", "tool_choice", "top_p"} {
		if _, ok := body[key]; ok {
			t.Errorf("research body has %s", key)
		}
	}
	if body["model"] != "perplexity/sonar" || body["max_tokens"] != 1500.0 || body["temperature"] != 0.2 {
		t.Errorf("body = %v", body)
	}
	r = c.ResearchCTVApp(context.Background(), CTVApp{AppName: "X"}, ResearchOptions{Model: "perplexity/sonar-pro", Temperature: 0.3})
	if !r.Success || f.body(1)["max_tokens"] != 2000.0 || f.body(1)["model"] != "perplexity/sonar-pro" || r.Model != "perplexity/sonar-pro" {
		t.Errorf("ctv research = %+v body = %v", r, f.body(1))
	}
}

func TestEveryBilledResponseIsRecordedIncludingRetriedOnes(t *testing.T) {
	f := newFakeAPI(t,
		reply{body: `{"model": "openai/gpt-6-luna", "error": {"message": "provider error", "code": 502}, "usage": {"cost": 0.002}}`},
		reply{body: toolReply("classify_website", defaultArgs, `, "cost": 0.003`)},
	)
	c, s := newTestClient(t, f, Options{MaxRetries: 2})
	run := cost.NewMeter()
	ctx := cost.WithItem(cost.WithRun(context.Background(), run))
	result := c.ClassifySite(ctx, SiteInput{Domain: "example.com", Content: "x"})
	if !result.Success {
		t.Fatalf("result = %+v", result)
	}
	snap := run.Snapshot()
	want := cost.Snapshot{USD: 0.005, Calls: 2, ByModel: map[string]float64{"openai/gpt-6-luna": 0.005}}
	if !reflect.DeepEqual(snap, want) {
		t.Fatalf("snapshot = %+v", snap)
	}
	if cost.ItemUSD(ctx) != "0.005000" {
		t.Fatalf("item = %s", cost.ItemUSD(ctx))
	}
	// "502" in the body error is retryable: exponential backoff from 2s.
	if !reflect.DeepEqual(s.d, []time.Duration{2 * time.Second}) {
		t.Fatalf("sleeps = %v", s.d)
	}
}

func TestRetryBackoffAndNonRetryableDelay(t *testing.T) {
	f := newFakeAPI(t, reply{status: 429, body: `{"error": {"message": "Rate limit exceeded"}}`})
	c, s := newTestClient(t, f, Options{MaxRetries: 3})
	result := c.ClassifySite(context.Background(), SiteInput{Domain: "example.com"})
	if result.Success || !strings.Contains(result.Error, "Error code: 429") || f.calls() != 3 {
		t.Fatalf("result = %+v calls = %d", result, f.calls())
	}
	if !reflect.DeepEqual(s.d, []time.Duration{2 * time.Second, 4 * time.Second}) {
		t.Fatalf("sleeps = %v", s.d)
	}
	// HTTP errors are not billed responses.
	f2 := newFakeAPI(t, reply{status: 400, body: `{"error": {"message": "bad request"}}`})
	c2, s2 := newTestClient(t, f2, Options{MaxRetries: 3})
	c2.ClassifySite(context.Background(), SiteInput{Domain: "example.com"})
	if !reflect.DeepEqual(s2.d, []time.Duration{2 * time.Second, 2 * time.Second}) || f2.calls() != 3 {
		t.Fatalf("non-retryable sleeps = %v", s2.d)
	}
}

func TestFallbackModelActivatesOnUnavailablePrimary(t *testing.T) {
	logs := captureLogs(t)
	f := newFakeAPI(t,
		reply{status: 404, body: `{"error": {"message": "No endpoints found for dead/model.", "code": 404}}`},
		reply{body: toolReply("classify_ctv_app", defaultArgs, "")},
	)
	c, s := newTestClient(t, f, Options{Model: "dead/model", FallbackModel: "good/model"})
	result := c.ClassifyCTVApp(context.Background(), CTVApp{AppName: "Example"}, "Research", CTVClassifyOptions{Temperature: 0.1, MaxTokens: 1500})
	if !result.Success || result.Model != "good/model" {
		t.Fatalf("result = %+v", result)
	}
	if f.body(0)["model"] != "dead/model" || f.body(1)["model"] != "good/model" || len(s.d) != 0 {
		t.Fatalf("models = %v, %v sleeps=%v", f.body(0)["model"], f.body(1)["model"], s.d)
	}
	if !c.FallbackActive() || c.Model() != "good/model" {
		t.Fatal("fallback not active")
	}
	if !strings.Contains(logs.String(), "Activating OpenRouter fallback model 'good/model' (primary 'dead/model' failed") {
		t.Errorf("logs = %s", logs.String())
	}
	// Later requests stay on the fallback.
	c.ClassifySite(context.Background(), SiteInput{Domain: "x"})
	if f.body(2)["model"] != "good/model" {
		t.Fatalf("model after swap = %v", f.body(2)["model"])
	}
}

func TestFallbackActivatesOnceUnderConcurrency(t *testing.T) {
	logs := captureLogs(t)
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		defer mu.Unlock()
		if body["model"] == "dead/model" {
			w.WriteHeader(404)
			_, _ = io.WriteString(w, `{"error": {"message": "model not found"}}`)
			return
		}
		_, _ = io.WriteString(w, toolReply("classify_website", defaultArgs, ""))
	}))
	t.Cleanup(srv.Close)
	c, err := New(Options{APIKey: "k", BaseURL: srv.URL, Model: "dead/model", FallbackModel: "good/model", MaxRetries: 3})
	if err != nil {
		t.Fatal(err)
	}
	c.sleep = func(context.Context, time.Duration) error { return nil }
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r := c.ClassifySite(context.Background(), SiteInput{Domain: "example.com"}); !r.Success {
				t.Errorf("result = %+v", r)
			}
		}()
	}
	wg.Wait()
	if n := strings.Count(logs.String(), "Activating OpenRouter fallback model"); n != 1 {
		t.Fatalf("fallback activated %d times", n)
	}
}

func TestConnectionCheckIsPartOfTheRunTotal(t *testing.T) {
	f := newFakeAPI(t, reply{body: `{"model": "openai/gpt-6-luna", "choices": [{"message": {"content": "OK"}}], "usage": {"cost": 0.000004}}`})
	c, _ := newTestClient(t, f, Options{})
	run := cost.NewMeter()
	if !c.TestConnection(cost.WithRun(context.Background(), run)) {
		t.Fatal("connection test failed")
	}
	if snap := run.Snapshot(); snap.Calls != 1 || snap.USD != 0.000004 {
		t.Fatalf("snapshot = %+v", snap)
	}
	body := f.body(0)
	if body["max_tokens"] != 16.0 || body["temperature"] != 0.0 {
		t.Fatalf("body = %v", body)
	}
}

func TestConnectionCheckFallsBackAndFails(t *testing.T) {
	f := newFakeAPI(t,
		reply{status: 404, body: `{"error": {"message": "This model is deprecated"}}`},
		reply{body: `{"choices": [{"message": {"content": "OK"}}]}`},
	)
	c, _ := newTestClient(t, f, Options{Model: "old/model", FallbackModel: "new/model"})
	if !c.TestConnection(context.Background()) || f.body(1)["model"] != "new/model" {
		t.Fatal("fallback connection test")
	}
	empty := newFakeAPI(t, reply{body: `{"choices": []}`})
	c2, _ := newTestClient(t, empty, Options{})
	if c2.TestConnection(context.Background()) {
		t.Fatal("empty choices accepted")
	}
	bodyErr := newFakeAPI(t, reply{body: `{"error": {"message": "Invalid arguments passed to the model.", "code": 502}}`})
	c3, _ := newTestClient(t, bodyErr, Options{})
	if c3.TestConnection(context.Background()) {
		t.Fatal("body error accepted")
	}
}

func TestResearchWebsiteDetectsInsufficientInformation(t *testing.T) {
	publisher := strings.Repeat("An established daily news publisher. ", 10)
	cases := []struct {
		content string
		missing bool
	}{
		{"INSUFFICIENT INFORMATION", true},
		{"  insufficient information\n", true},
		{"Too short to be meaningful.", true},
		{"INSUFFICIENT INFORMATION about the custom question. " + publisher, false},
		{publisher + "INSUFFICIENT INFORMATION about its video policy.", false},
	}
	for _, tc := range cases {
		f := newFakeAPI(t, reply{body: contentReply(tc.content)})
		c, _ := newTestClient(t, f, Options{})
		r := c.ResearchWebsite(context.Background(), "nobody-knows-this.example", ResearchOptions{})
		want := tc.content
		if tc.missing {
			want = ""
		}
		if !r.Success || r.Content != want {
			t.Errorf("%q: research = %+v", tc.content, r)
		}
	}
}

func TestResearchFailureShape(t *testing.T) {
	f := newFakeAPI(t, reply{status: 401, body: `{"error": "unauthorized"}`})
	c, _ := newTestClient(t, f, Options{MaxRetries: 1})
	r := c.ResearchWebsite(context.Background(), "x.com", ResearchOptions{Model: "perplexity/sonar-pro"})
	if r.Success || r.Content != "" || r.Model != "perplexity/sonar-pro" || !strings.Contains(r.Error, "401") {
		t.Fatalf("research = %+v", r)
	}
}

func TestCustomQuestionsReachResearchAPI(t *testing.T) {
	questions := []string{
		"Would a mainstream brand be comfortable appearing beside this content?",
		"What is its primary purpose? Options: Education, Entertainment, Unknown",
	}
	for _, ctv := range []bool{false, true} {
		f := newFakeAPI(t, reply{body: contentReply(strings.Repeat("Factual source evidence. ", 30))})
		c, _ := newTestClient(t, f, Options{})
		opts := ResearchOptions{CustomQuestions: questions}
		if ctv {
			c.ResearchCTVApp(context.Background(), CTVApp{AppName: "example.com"}, opts)
		} else {
			c.ResearchWebsite(context.Background(), "example.com", opts)
		}
		msgs := f.body(0)["messages"].([]any)
		prompt := msgs[len(msgs)-1].(map[string]any)["content"].(string)
		for _, want := range []string{questions[0], "Education, Entertainment, Unknown", "Do not assign labels"} {
			if !strings.Contains(prompt, want) {
				t.Errorf("ctv=%v: prompt lacks %q", ctv, want)
			}
		}
	}
}

func TestCTVPublisherReachesBothPrompts(t *testing.T) {
	f := newFakeAPI(t,
		reply{body: contentReply("Demo Network runs this channel.")},
		reply{body: toolReply("classify_ctv_app", defaultArgs, "")},
	)
	c, _ := newTestClient(t, f, Options{})
	app := CTVApp{AppName: "Demo TV", Publisher: "Demo Network"}
	r := c.ResearchCTVApp(context.Background(), app, ResearchOptions{})
	// CTV research is returned as is; the processor rejects thin evidence.
	if !r.Success || r.Content != "Demo Network runs this channel." {
		t.Fatalf("research = %+v", r)
	}
	c.ClassifyCTVApp(context.Background(), app, r.Content, CTVClassifyOptions{})
	research := f.body(0)["messages"].([]any)[1].(map[string]any)["content"].(string)
	classify := f.body(1)["messages"].([]any)[1].(map[string]any)["content"].(string)
	if !strings.Contains(research, "Publisher/Network: Demo Network") || !strings.Contains(classify, "Publisher: Demo Network") {
		t.Fatal("publisher missing from prompts")
	}
	if _, ok := f.body(1)["top_p"]; ok {
		t.Fatal("CTV classification sends sampling parameters")
	}
}

func TestClassifyCTVAppAppliesNewsRouting(t *testing.T) {
	args := map[string]any{}
	for k, v := range defaultArgs {
		args[k] = v
	}
	args["content_type"] = "News"
	args["vertical"] = "Television"
	args["target_audience"] = "Local viewers"
	args["network_affiliation"] = "NBC"
	f := newFakeAPI(t, reply{body: toolReply("classify_ctv_app", args, "")})
	c, _ := newTestClient(t, f, Options{Model: "m"})
	research := "This is a local news station providing local news coverage, breaking news, and headlines."
	r := c.ClassifyCTVApp(context.Background(), CTVApp{AppName: "KARE 11 News"}, research, CTVClassifyOptions{})
	if !r.Success || r.Vertical != "Politics > Civic affairs" || r.VerticalTier1 != "Politics" ||
		r.PoliticalLeaning != "Center" || r.NetworkAffiliation != "NBC" || r.TargetAudience != "Local viewers" || r.Model != "m" {
		t.Fatalf("result = %+v", r)
	}
}

func TestCTVFailureShape(t *testing.T) {
	f := newFakeAPI(t, reply{body: `{"choices": [{"message": {"content": "no json"}}]}`})
	c, _ := newTestClient(t, f, Options{Model: "m"})
	r := c.ClassifyCTVApp(context.Background(), CTVApp{AppName: "X"}, "research", CTVClassifyOptions{})
	if r.Success || r.Quality != "Error" || r.TargetAudience != "Unknown" || r.ContentType != "Unknown" ||
		r.Confidence != "Low" || r.Model != "m" || r.Description != "N/A" || r.AudienceSize != "Unknown" {
		t.Fatalf("result = %+v", r)
	}
	if !strings.HasPrefix(r.Justification, "Classification failed: Unparseable CTV classification response") {
		t.Fatalf("justification = %q", r.Justification)
	}
}

func TestToolNameMismatchWarns(t *testing.T) {
	logs := captureLogs(t)
	f := newFakeAPI(t, reply{body: toolReply("classify_app", defaultArgs, "")})
	c, _ := newTestClient(t, f, Options{})
	if r := c.ClassifyApp(context.Background(), AppInput{AppID: "1"}); !r.Success {
		t.Fatalf("result = %+v", r)
	}
	if strings.Contains(logs.String(), "did not match expected") {
		t.Fatal("matching name warned")
	}
	if r := c.ClassifySite(context.Background(), SiteInput{Domain: "x"}); !r.Success || r.Quality != "Premium" {
		t.Fatalf("result = %+v", r)
	}
	if !strings.Contains(logs.String(), "Function call name 'classify_app' did not match expected 'classify_website'") {
		t.Fatalf("logs = %s", logs.String())
	}
}

func TestInlineJSONContentIsRecovered(t *testing.T) {
	args, _ := json.Marshal(defaultArgs)
	f := newFakeAPI(t, reply{body: contentReply("```json\n" + string(args) + "\n```")})
	c, _ := newTestClient(t, f, Options{})
	if r := c.ClassifySite(context.Background(), SiteInput{Domain: "x"}); !r.Success || r.Quality != "Premium" {
		t.Fatalf("result = %+v", r)
	}
}

func TestCancelledContextStopsRetrying(t *testing.T) {
	f := newFakeAPI(t, reply{status: 503, body: "busy"})
	c, err := New(Options{APIKey: "k", BaseURL: f.srv.URL, MaxRetries: 5})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.sleep = func(context.Context, time.Duration) error { cancel(); return context.Canceled }
	r := c.ClassifySite(ctx, SiteInput{Domain: "x"})
	if r.Success || !strings.Contains(r.Error, "context canceled") || f.calls() != 1 {
		t.Fatalf("result = %+v calls = %d", r, f.calls())
	}
}

func TestRequestTimeout(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-block }))
	t.Cleanup(func() { close(block); srv.Close() })
	c, err := New(Options{APIKey: "k", BaseURL: srv.URL, MaxRetries: 1, Timeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	r := c.ClassifySite(context.Background(), SiteInput{Domain: "x"})
	if r.Success || r.Error != "Request timed out." {
		t.Fatalf("result = %+v", r)
	}
}

func TestBodyErrorAndUnavailableDetection(t *testing.T) {
	cases := map[string]string{
		`{"error": {"message": "Invalid arguments passed to the model.", "code": 502}}`: "OpenRouter body error (code=502): Invalid arguments passed to the model.",
		`{"error": {"code": "x"}}`:    "OpenRouter body error (code=x): unknown error",
		`{"error": {"message": "m"}}`: "OpenRouter body error: m",
		`{"error": "boom"}`:           "OpenRouter body error: boom",
		`{"error": null}`:             "",
		`{"error": {}}`:               "",
		`{"choices": []}`:             "",
	}
	for body, want := range cases {
		f := newFakeAPI(t, reply{body: body})
		c, _ := newTestClient(t, f, Options{})
		resp, err := c.post(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := bodyError(resp); got != want {
			t.Errorf("bodyError(%s) = %q, want %q", body, got, want)
		}
	}
	for text, want := range map[string]bool{
		"Error code: 404 - model not found":                   true,
		"This model is deprecated":                            true,
		"No endpoints found for x":                            true,
		"No allowed providers are available":                  true,
		"OpenRouter body error (code=502): Invalid arguments": true,
		"OpenRouter body error (code=400): bad":               true,
		"OpenRouter body error (code=429): Rate limited":      false,
		"Error code: 429 - rate limit":                        false,
		"Connection error: dial tcp: connection refused":      false,
	} {
		if got := isModelUnavailable(text); got != want {
			t.Errorf("isModelUnavailable(%q) = %v", text, got)
		}
	}
}

func TestIsInsufficientResearch(t *testing.T) {
	for text, want := range map[string]bool{
		"": true, "   ": true, "INSUFFICIENT INFORMATION": true, "  insufficient information\n": true,
		"INSUFFICIENT INFORMATION about one custom question.": false, "CTV source text": false,
	} {
		if got := IsInsufficientResearch(text); got != want {
			t.Errorf("IsInsufficientResearch(%q) = %v", text, got)
		}
	}
}

func TestCTVSchemaStructure(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal([]byte(pyjson.Dumps(ctvSchema())), &schema); err != nil {
		t.Fatal(err)
	}
	fn := schema["function"].(map[string]any)
	if schema["type"] != "function" || fn["name"] != "classify_ctv_app" {
		t.Fatalf("schema = %v", schema)
	}
	params := fn["parameters"].(map[string]any)
	props := params["properties"].(map[string]any)
	for _, key := range []string{"quality", "justification", "vertical", "description", "target_audience", "content_type",
		"network_affiliation", "confidence", "language", "political_leaning", "audience_size"} {
		if _, ok := props[key]; !ok {
			t.Errorf("schema lacks %s", key)
		}
	}
	enum := props["content_type"].(map[string]any)["enum"].([]any)
	for _, v := range []string{"Live TV", "On-Demand Streaming", "Live Sports", "News", "Mixed Content"} {
		found := false
		for _, e := range enum {
			found = found || e == v
		}
		if !found {
			t.Errorf("content_type enum lacks %s", v)
		}
	}
	if len(params["required"].([]any)) != 11 {
		t.Errorf("required = %v", params["required"])
	}
}

func TestTaxonomyPromptAndRoutingTargetsExist(t *testing.T) {
	list := TaxonomyPrompt()
	for _, want := range []string{"• Politics", "  - Civic affairs", "  - Elections", "• Crime", "• Disasters", "  - Weather", "• Sports"} {
		if !strings.Contains(list, want) {
			t.Errorf("taxonomy prompt lacks %q", want)
		}
	}
	if !reflect.DeepEqual(VerticalTiers("Baseball"), []string{"Sports", "Baseball"}) {
		t.Errorf("Baseball = %v", VerticalTiers("Baseball"))
	}
}

func TestLiveOpenRouterConnection(t *testing.T) {
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" || key == "test-key" || os.Getenv("LENS_LIVE_TESTS") != "1" {
		t.Skip("set OPENROUTER_API_KEY and LENS_LIVE_TESTS=1 to run the live OpenRouter test")
	}
	c, err := New(Options{APIKey: key, Model: "google/gemini-2.5-flash-lite", MaxTokens: 16, Timeout: 60 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if !c.TestConnection(context.Background()) {
		t.Skip("OpenRouter API is unreachable in this environment")
	}
	if c.Mode() != "openrouter" {
		t.Fatal("mode")
	}
}
