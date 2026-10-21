// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package categories

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
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ElcanoTek/lens/internal/categories/pyjson"
	"github.com/ElcanoTek/lens/internal/cost"
	"github.com/ElcanoTek/lens/internal/logx"
)

const specJSON = `{"categories": [
	{"name": "Brand safe", "type": "boolean",
	 "question": "Would a mainstream brand be comfortable appearing beside this content?"},
	{"name": "Purpose", "type": "choice", "question": "What is its primary purpose?",
	 "options": ["Education", "Entertainment", "Unknown"]}]}`

const answerJSON = `{"model": "typesafe/jev-1.13-20260917", "provider": "TypeSafe", "answers": {
	"c0": {"type": "noul", "noul": 0.1},
	"c1": {"type": "choice", "choice": "Education", "confidence": 0.9,
	       "probabilities": {"Education": 0.95, "Entertainment": 0.03, "Unknown": 0.02}}}}`

func testSpec(t *testing.T) *Spec {
	t.Helper()
	spec, err := Parse(specJSON)
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

// reply is one scripted HTTP response.
type reply struct {
	status int
	body   string
}

type recorded struct {
	url     string
	auth    string
	payload map[string]any
	raw     []byte
}

// server serves replies in order and records every request.
type server struct {
	mu      sync.Mutex
	replies []reply
	calls   []recorded
	srv     *httptest.Server
}

func newServer(t *testing.T, replies ...reply) *server {
	t.Helper()
	s := &server{replies: replies}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(raw, &payload)
		s.mu.Lock()
		s.calls = append(s.calls, recorded{url: r.URL.Path, auth: r.Header.Get("Authorization"), payload: payload, raw: raw})
		var rep reply
		if len(s.replies) > 0 {
			rep, s.replies = s.replies[0], s.replies[1:]
		} else {
			rep = reply{status: 599, body: "no scripted reply"}
		}
		s.mu.Unlock()
		if rep.status == 0 {
			rep.status = 200
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rep.status)
		_, _ = io.WriteString(w, rep.body)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *server) client(t *testing.T, model string, opts Options) *DecisionCategories {
	t.Helper()
	opts.OpenRouterURL = s.srv.URL + "/api/alpha/decisions"
	opts.TypesafeURL = s.srv.URL + "/v1/systemone"
	if opts.APIKey == "" {
		opts.APIKey = "never-log-this-secret"
	}
	d, err := New(testSpec(t), model, opts)
	if err != nil {
		t.Fatal(err)
	}
	d.sleep = func(context.Context, time.Duration) error { return nil }
	return d
}

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	logx.SetOutput(&buf, slog.LevelDebug)
	t.Cleanup(func() { logx.SetOutput(io.Discard, slog.LevelWarn) })
	return &buf
}

func noTypesafeEnv(t *testing.T) {
	t.Helper()
	// A developer's real TypeSafe key must not change which requests tests expect.
	t.Setenv("TYPESAFE_API_KEY", "")
}

func TestBatchAPIContractPreservesRawProbabilitiesAndBoundsContent(t *testing.T) {
	noTypesafeEnv(t)
	s := newServer(t, reply{body: answerJSON})
	d := s.client(t, "", Options{})
	result, err := d.Classify(context.Background(), "example.com", strings.Repeat("x", 30000), "direct", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.calls) != 1 {
		t.Fatalf("calls = %d", len(s.calls))
	}
	call := s.calls[0]
	if call.url != "/api/alpha/decisions" || call.auth != "Bearer never-log-this-secret" {
		t.Fatalf("call = %+v", call)
	}
	payload := call.payload
	if payload["model"] != "~typesafe/jev-latest" {
		t.Fatalf("model = %v", payload["model"])
	}
	questions := payload["questions"].(map[string]any)
	c0 := questions["c0"].(map[string]any)
	if !reflect.DeepEqual(c0["criteria"], map[string]any{
		"true": "The answer to the question is yes.", "false": "The answer to the question is no.",
	}) || c0["type"] != "noul" {
		t.Fatalf("c0 = %v", c0)
	}
	state := payload["state"].(map[string]any)
	if len(state["content"].(string)) != MaxContentChars || state["truncated"] != true {
		t.Fatalf("state not bounded")
	}
	c1 := questions["c1"].(map[string]any)
	if !reflect.DeepEqual(c1["criteria"], map[string]any{"Education": nil, "Entertainment": nil, "Unknown": nil}) {
		t.Fatalf("c1 criteria = %v", c1["criteria"])
	}
	// Choice criteria keep the definition order (dict.fromkeys).
	if !bytes.Contains(call.raw, []byte(`"criteria": {"Education": null, "Entertainment": null, "Unknown": null}`)) {
		t.Fatalf("criteria order/format lost: %s", call.raw)
	}
	want := map[string]any{
		"Custom: Brand safe":         "No",
		"P(yes/choice): Brand safe":  0.1, // P(yes), not confidence in No
		"Custom: Purpose":            "Education",
		"P(yes/choice): Purpose":     0.95,
		"Confidence: Purpose":        0.9,
		"P: Purpose / Education":     0.95,
		"P: Purpose / Entertainment": 0.03,
		"P: Purpose / Unknown":       0.02,
		"Decision_Provider":          "TypeSafe",
		"Decision_Status":            "success",
		"Decision_Model":             "typesafe/jev-1.13-20260917",
	}
	for k, v := range want {
		if result[k] != v {
			t.Errorf("%s = %#v, want %#v", k, result[k], v)
		}
	}
	answers := result["Decision_Answers"].(string)
	wantAnswers := `{"Brand safe": {"type": "boolean", "value": "No", "probability_yes": 0.1}, ` +
		`"Purpose": {"type": "choice", "value": "Education", "probabilities": {"Education": 0.95, ` +
		`"Entertainment": 0.03, "Unknown": 0.02}, "confidence": 0.9}}`
	if answers != wantAnswers {
		t.Errorf("Decision_Answers =\n%s\nwant\n%s", answers, wantAnswers)
	}
	cols := Columns(testSpec(t))
	if !reflect.DeepEqual(cols[:2], []string{"Custom: Brand safe", "P(yes/choice): Brand safe"}) {
		t.Errorf("cols = %v", cols)
	}
	if !reflect.DeepEqual(cols[len(cols)-5:], []string{"Decision_Status", "Decision_Error", "Decision_Model", "Decision_Provider", "Decision_Answers"}) {
		t.Errorf("cols = %v", cols)
	}
	if !contains(cols, "P: Purpose / Education") {
		t.Errorf("cols = %v", cols)
	}
	// Every result column is a declared column.
	for k := range result {
		if !contains(cols, k) {
			t.Errorf("result key %q not in columns", k)
		}
	}
}

func TestBoundedRetriesAndSecretSafeErrors(t *testing.T) {
	noTypesafeEnv(t)
	cases := []struct {
		statuses []int
		calls    int
		success  bool
	}{
		{[]int{429, 529, 200}, 3, true},
		{[]int{503, 503, 503}, 3, false},
		{[]int{401}, 1, false},
		{[]int{422}, 1, false},
	}
	for _, tc := range cases {
		logs := captureLogs(t)
		replies := make([]reply, 0, len(tc.statuses))
		for _, s := range tc.statuses {
			if s == 200 {
				replies = append(replies, reply{body: answerJSON})
			} else {
				replies = append(replies, reply{status: s, body: `{"error": "never-log-this-secret"}`})
			}
		}
		s := newServer(t, replies...)
		d := s.client(t, "", Options{})
		var slept []time.Duration
		d.sleep = func(_ context.Context, dur time.Duration) error { slept = append(slept, dur); return nil }
		result, err := d.Classify(context.Background(), "x", "Evidence", "research", "")
		if err != nil {
			t.Fatal(err)
		}
		if len(s.calls) != tc.calls {
			t.Errorf("%v: calls = %d, want %d", tc.statuses, len(s.calls), tc.calls)
		}
		if (result["Decision_Status"] == "success") != tc.success {
			t.Errorf("%v: result = %v", tc.statuses, result)
		}
		if tc.calls == 3 && !reflect.DeepEqual(slept, []time.Duration{time.Second, 2 * time.Second}) {
			t.Errorf("%v: backoff = %v", tc.statuses, slept)
		}
		dump, _ := json.Marshal(result)
		if strings.Contains(string(dump)+logs.String(), "never-log-this-secret") {
			t.Errorf("%v: secret leaked", tc.statuses)
		}
	}
}

func TestMalformedResponseNeverBecomesAFalseLabel(t *testing.T) {
	noTypesafeEnv(t)
	mutations := []string{
		`{"model": "typesafe/jev-1", "answers": {"c1": {"type": "choice", "choice": "Education"}}}`,
		`{"model": "typesafe/jev-1", "answers": {"c0": {"type": "noul", "noul": NaN}, "c1": {"type": "choice", "choice": "Education"}}}`,
		`{"model": "typesafe/jev-1", "answers": {"c0": {"type": "noul", "noul": 1.1}, "c1": {"type": "choice", "choice": "Education"}}}`,
		`{"model": "typesafe/jev-1", "answers": {"c0": {"type": "noul", "noul": 0.1}, "c1": {"type": "choice", "choice": "Invented"}}}`,
		`{"model": "typesafe/jev-1", "answers": {"c0": {"type": "noul", "noul": 0.1}, "c1": {"type": "choice", "choice": "Education", "probabilities": {"Education": 0.9}}}}`,
		`{"model": "typesafe/jev-1", "answers": {"c0": {"type": "noul", "noul": 0.1}, "c1": {"type": "choice", "choice": "Education", "probabilities": ["Education", "Entertainment", "Unknown"]}}}`,
		`{"model": "typesafe/jev-1", "answers": {"c0": {"type": "noul", "noul": true}, "c1": {"type": "choice", "choice": "Education"}}}`,
		`{"model": "typesafe/jev-1", "answers": {"c0": {"type": "noul", "noul": 0.1}, "c1": {"type": "choice", "choice": "Education", "probabilities": {"Education": 0.5, "Entertainment": 0.1, "Unknown": 0.1}}}}`,
		`{"model": "bad model; rm", "answers": {"c0": {"type": "noul", "noul": 0.1}, "c1": {"type": "choice", "choice": "Education"}}}`,
		`{"answers": {"c0": {"type": "noul", "noul": 0.1}, "c1": {"type": "choice", "choice": "Education"}}}`,
		`not json`,
	}
	for _, body := range mutations {
		s := newServer(t, reply{body: body})
		result, err := s.client(t, "", Options{}).Classify(context.Background(), "x", "Evidence", "direct", "")
		if err != nil {
			t.Fatal(err)
		}
		want := Result{"Decision_Status": "error", "Decision_Error": "Invalid OpenRouter response"}
		if !reflect.DeepEqual(result, want) {
			t.Errorf("%s: result = %v", body, result)
		}
		if len(s.calls) != 1 {
			t.Errorf("%s: invalid responses must not retry (calls=%d)", body, len(s.calls))
		}
	}
}

func TestNoEvidenceSkipsNetworkAndCancellationPropagates(t *testing.T) {
	noTypesafeEnv(t)
	s := newServer(t)
	d := s.client(t, "", Options{})
	for _, content := range []string{"", "   ", "\n\t"} {
		result, err := d.Classify(context.Background(), "x", content, "direct", "")
		if err != nil || result["Decision_Status"] != "skipped" || result["Decision_Error"] != "No source content available" {
			t.Fatalf("result = %v, %v", result, err)
		}
	}
	if len(s.calls) != 0 {
		t.Fatalf("network used for empty evidence")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.Classify(ctx, "x", "Evidence", "direct", ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestInvalidSpecsRejected(t *testing.T) {
	long := `{"categories": [{"name": "Brand safe", "type": "boolean", "question": "Q?"}, {"name": "Brand safe", "type": "boolean", "question": "Q?"}]}`
	cases := []string{
		`{}`,
		`[]`,
		`{"categories": []}`,
		`{"categories": [], "extra": 1}`,
		long,
		`{"categories": [{"name": "=formula", "type": "boolean", "question": "Question?"}]}`,
		`{"categories": [{"name": "Test", "type": "choice", "question": "Question?", "options": ["A", "a"]}]}`,
		`{"categories": [{"name": "Test", "type": "choice", "question": "Question?", "options": ["A", "=1+1"]}]}`,
		`{"categories": [{"name": "Test", "type": "choice", "question": "Question?", "options": ["A"]}]}`,
		`{"categories": [{"name": "Test", "type": "choice", "question": "Question?", "options": ["A", "B\u0007"]}]}`,
		`{"categories": [{"name": "Test", "type": "boolean", "question": " "}]}`,
		`{"categories": [{"name": "Test", "type": "boolean", "question": "Q?", "options": ["A", "B"]}]}`,
		`{"categories": [{"name": "Test", "type": "number", "question": "Q?"}]}`,
		`{"categories": [{"name": "Test", "type": "boolean", "question": "Q?", "extra": 1}]}`,
		`{"categories": [{"name": "Test name that is far too long for the limit of 48 c", "type": "boolean", "question": "Q?"}]}`,
		`{"categories": ["not an object"]}`,
		`{"categories": [{"name": "Test", "type": "boolean", "question": "` + strings.Repeat("q", 1001) + `"}]}`,
	}
	for _, text := range cases {
		if _, err := Parse(text); err == nil {
			t.Errorf("accepted %s", text)
		} else {
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Errorf("%s: error %T is not a ValidationError", text, err)
			}
		}
	}
	// Thirteen categories.
	many := make([]string, 0, 13)
	for _, n := range "ABCDEFGHIJKLM" {
		many = append(many, `{"name": "`+string(n)+`", "type": "boolean", "question": "Q?"}`)
	}
	if _, err := Parse(`{"categories": [` + strings.Join(many, ",") + `]}`); err == nil ||
		err.Error() != "Define between 1 and 12 custom categories." {
		t.Errorf("13 categories: %v", err)
	}
}

func TestValidationMessagesAndNormalisation(t *testing.T) {
	_, err := Parse(`{"categories": [{"name": "A", "type": "boolean", "question": "Q?"}, {"name": "a ", "type": "boolean", "question": "Q?"}]}`)
	if err == nil || err.Error() != "Category names must be unique." {
		t.Errorf("err = %v", err)
	}
	_, err = Parse(`{"categories": [{"name": "A", "type": "boolean", "question": ""}]}`)
	if err == nil || err.Error() != "Each category needs a question of 1–1000 characters." {
		t.Errorf("err = %v", err)
	}
	spec, err := Parse(`{"categories": [{"options": [" Yes ", "No"], "question": "  Who?  ", "type": "choice", "name": "Name  "}]}`)
	if err != nil {
		t.Fatal(err)
	}
	want := &Spec{Categories: []Category{{Name: "Name", Type: "choice", Question: "Who?", Options: []string{"Yes", "No"}}}}
	if !Equal(spec, want) {
		t.Fatalf("spec = %+v", spec)
	}
	// Persisted shape, key order included, is the Python one.
	got, _ := json.Marshal(spec)
	if string(got) != `{"categories":[{"name":"Name","type":"choice","question":"Who?","options":["Yes","No"]}]}` {
		t.Errorf("json = %s", got)
	}
	if spec.PythonJSON(0) != `{"categories": [{"name": "Name", "type": "choice", "question": "Who?", "options": ["Yes", "No"]}]}` {
		t.Errorf("python json = %s", spec.PythonJSON(0))
	}
	indented := "{\n  \"categories\": [\n    {\n      \"name\": \"Name\",\n      \"type\": \"choice\",\n" +
		"      \"question\": \"Who?\",\n      \"options\": [\n        \"Yes\",\n        \"No\"\n      ]\n    }\n  ]\n}"
	if spec.PythonJSON(2) != indented {
		t.Errorf("indented =\n%s", spec.PythonJSON(2))
	}
	// A boolean has no "options" key at all.
	b, _ := Parse(`{"categories": [{"name": "B", "type": "boolean", "question": "Q?"}]}`)
	got, _ = json.Marshal(b)
	if string(got) != `{"categories":[{"name":"B","type":"boolean","question":"Q?"}]}` {
		t.Errorf("json = %s", got)
	}
	// Round trip through JSON, then re-validation, is stable.
	var decoded Spec
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatal(err)
	}
	again, err := Validate(&decoded)
	if err != nil || !Equal(again, b) {
		t.Fatalf("revalidate = %+v, %v", again, err)
	}
}

func TestEqual(t *testing.T) {
	a := testSpecFrom(t, specJSON)
	b := testSpecFrom(t, specJSON)
	if !Equal(a, b) || !Equal(nil, nil) || Equal(a, nil) || Equal(nil, b) {
		t.Fatal("Equal basics")
	}
	b.Categories[0].Question = "Different rubric?"
	if Equal(a, b) {
		t.Fatal("changed question compared equal")
	}
	c := testSpecFrom(t, specJSON)
	c.Categories[1].Options = []string{"Education", "Entertainment", "Other"}
	if Equal(a, c) {
		t.Fatal("changed options compared equal")
	}
}

func testSpecFrom(t *testing.T, text string) *Spec {
	t.Helper()
	s, err := Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestJSONLimitsAndKeyRequirement(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "")
	noTypesafeEnv(t)
	if _, err := New(testSpec(t), "", Options{}); err == nil || !strings.Contains(err.Error(), "OPENROUTER_API_KEY") {
		t.Errorf("err = %v", err)
	}
	if _, err := New(testSpec(t), "bad model; rm -rf", Options{APIKey: "k"}); err == nil || !strings.Contains(err.Error(), "model ID") {
		t.Errorf("err = %v", err)
	}
	if _, err := Parse(strings.Repeat(" ", 32769)); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("err = %v", err)
	}
	if _, err := Parse("{not json"); err == nil || err.Error() != "Custom categories must be valid JSON." {
		t.Errorf("err = %v", err)
	}
	// The serialized size limit counts json.dumps's ASCII escapes.
	cats := make([]string, 0, 12)
	for _, n := range "ABCDEFGHIJKL" {
		cats = append(cats, `{"name": "`+string(n)+`", "type": "boolean", "question": "`+strings.Repeat("é", 1000)+`"}`)
	}
	text := `{"categories": [` + strings.Join(cats, ",") + `]}`
	if len(text) > MaxSpecBytes {
		t.Fatalf("fixture too big: %d", len(text))
	}
	if _, err := Parse(text); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("escaped size not enforced: %v", err)
	}
	t.Setenv("OPENROUTER_API_KEY", " env-key ")
	d, err := New(testSpec(t), "", Options{})
	if err != nil || d.apiKey != "env-key" {
		t.Errorf("env key not used: %v", err)
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "labels.json")
	if err := os.WriteFile(path, []byte(specJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	spec, err := Load(path)
	if err != nil || len(spec.Categories) != 2 {
		t.Fatalf("Load = %v, %v", spec, err)
	}
	big := filepath.Join(dir, "big.json")
	if err := os.WriteFile(big, bytes.Repeat([]byte(" "), 100000), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(big); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("err = %v", err)
	}
	if _, err := Load(filepath.Join(dir, "missing.json")); err == nil {
		t.Error("missing file accepted")
	}
}

func TestChoiceWithoutDistributionLeavesProbabilityColumnsBlank(t *testing.T) {
	noTypesafeEnv(t)
	s := newServer(t, reply{body: `{"model": "openai/x", "provider": "OpenAI", "answers": {
		"c0": {"type": "noul", "noul": 1}, "c1": {"type": "choice", "choice": "Entertainment"}}}`})
	result, err := s.client(t, "", Options{}).Classify(context.Background(), "x", "Evidence", "direct", "")
	if err != nil {
		t.Fatal(err)
	}
	if result["Decision_Status"] != "success" || result["Decision_Provider"] != "OpenAI" ||
		result["Custom: Purpose"] != "Entertainment" || result["P(yes/choice): Purpose"] != "" ||
		result["Confidence: Purpose"] != "" || result["P: Purpose / Education"] != "" {
		t.Fatalf("result = %v", result)
	}
	// An integer probability stays an integer, as Python's json kept it.
	if result["P(yes/choice): Brand safe"] != int64(1) || result["Custom: Brand safe"] != "Yes" {
		t.Fatalf("boolean = %#v", result["P(yes/choice): Brand safe"])
	}
	if FormatCell(result["P(yes/choice): Brand safe"]) != "1" {
		t.Fatal("int cell")
	}
	answers, _ := pyjson.Decode([]byte(result["Decision_Answers"].(string)))
	purpose, _ := answers.(pyjson.Object).Get("Purpose")
	if pyjson.DumpsUnicode(purpose) != `{"type": "choice", "value": "Entertainment"}` {
		t.Fatalf("answers = %s", result["Decision_Answers"])
	}
}

func typesafeServer(t *testing.T, model string, replies ...reply) (*server, *DecisionCategories) {
	t.Helper()
	s := newServer(t, replies...)
	return s, s.client(t, model, Options{APIKey: "openrouter-secret", TypesafeAPIKey: "typesafe-secret"})
}

func TestJevFallsBackToTypesafeDirectlyWhenOpenRouterFails(t *testing.T) {
	logs := captureLogs(t)
	direct := `{"model": "jev-1.13.0", "answers": {"c0": {"type": "noul", "noul": 0.1},
		"c1": {"type": "choice", "choice": "Education"}}}`
	s, d := typesafeServer(t, "~typesafe/jev-latest", reply{status: 402}, reply{body: direct})
	result, err := d.Classify(context.Background(), "x", "Evidence", "direct", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.calls) != 2 {
		t.Fatalf("calls = %d", len(s.calls))
	}
	first, second := s.calls[0], s.calls[1]
	if first.url != "/api/alpha/decisions" || first.auth != "Bearer openrouter-secret" {
		t.Errorf("first = %+v", first)
	}
	if second.url != "/v1/systemone" || second.auth != "Bearer typesafe-secret" {
		t.Errorf("second = %+v", second)
	}
	// TypeSafe's own endpoint gets its own model name and the payload it accepted before.
	if second.payload["model"] != "jev-latest" {
		t.Errorf("model = %v", second.payload["model"])
	}
	if _, ok := second.payload["questions"].(map[string]any)["c0"].(map[string]any)["criteria"]; ok {
		t.Error("TypeSafe payload has noul criteria")
	}
	if !reflect.DeepEqual(second.payload["state"], first.payload["state"]) {
		t.Error("state differs")
	}
	if result["Decision_Status"] != "success" || result["Decision_Provider"] != "TypeSafe (direct)" || result["Decision_Model"] != "jev-1.13.0" {
		t.Errorf("result = %v", result)
	}
	if strings.Contains(logs.String(), "secret") {
		t.Errorf("secret logged: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "Custom categories: OpenRouter HTTP 402; retrying with TypeSafe directly") {
		t.Errorf("logs = %s", logs.String())
	}
}

func TestFallbackRunsOnlyAfterOpenRouterRetriesAreExhausted(t *testing.T) {
	s, d := typesafeServer(t, "~typesafe/jev-latest", reply{status: 503}, reply{status: 503}, reply{status: 503}, reply{body: answerJSON})
	result, err := d.Classify(context.Background(), "x", "Evidence", "direct", "")
	if err != nil {
		t.Fatal(err)
	}
	urls := make([]string, 0, len(s.calls))
	for _, c := range s.calls {
		urls = append(urls, c.url)
	}
	want := []string{"/api/alpha/decisions", "/api/alpha/decisions", "/api/alpha/decisions", "/v1/systemone"}
	if !reflect.DeepEqual(urls, want) {
		t.Errorf("urls = %v", urls)
	}
	if result["Decision_Status"] != "success" || result["Decision_Provider"] != "TypeSafe (direct)" {
		t.Errorf("result = %v", result)
	}
}

func TestFallbackFailureReportsBothRoutes(t *testing.T) {
	_, d := typesafeServer(t, "~typesafe/jev-latest", reply{status: 401}, reply{status: 401})
	result, err := d.Classify(context.Background(), "x", "Evidence", "direct", "")
	if err != nil {
		t.Fatal(err)
	}
	want := Result{"Decision_Status": "error", "Decision_Error": "OpenRouter HTTP 401; TypeSafe HTTP 401"}
	if !reflect.DeepEqual(result, want) {
		t.Errorf("result = %v", result)
	}
}

func TestTypesafeModelMapping(t *testing.T) {
	cases := map[string]string{
		"~typesafe/jev-latest":        "jev-latest",
		"typesafe/jev-1.13":           "jev-1.13",
		"openai/gpt-6-luna-decisions": "",
		"typesafe/jev-router":         "",
		"typesafe/jev-latest-x":       "",
	}
	for model, want := range cases {
		got, ok := TypesafeModelFor(model)
		if got != want || ok != (want != "") {
			t.Errorf("%s -> %q %v", model, got, ok)
		}
	}
}

func TestNonJevModelNeverCallsTypesafe(t *testing.T) {
	s, d := typesafeServer(t, "openai/gpt-6-luna-decisions", reply{status: 401})
	result, err := d.Classify(context.Background(), "x", "Evidence", "direct", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.calls) != 1 || result["Decision_Error"] != "OpenRouter HTTP 401" {
		t.Errorf("calls = %d, result = %v", len(s.calls), result)
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	noTypesafeEnv(t)
	target := newServer(t, reply{body: answerJSON})
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.srv.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirect.Close)
	d, err := New(testSpec(t), "", Options{APIKey: "k", OpenRouterURL: redirect.URL})
	if err != nil {
		t.Fatal(err)
	}
	result, err := d.Classify(context.Background(), "x", "Evidence", "direct", "")
	if err != nil {
		t.Fatal(err)
	}
	if result["Decision_Error"] != "OpenRouter HTTP 307" || len(target.calls) != 0 {
		t.Errorf("result = %v, followed = %d", result, len(target.calls))
	}
}

func TestConnectionFailureIsSanitizedAndRetried(t *testing.T) {
	noTypesafeEnv(t)
	d, err := New(testSpec(t), "", Options{APIKey: "k", OpenRouterURL: "http://127.0.0.1:1/never"})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	d.sleep = func(context.Context, time.Duration) error { n++; return nil }
	result, err := d.Classify(context.Background(), "x", "Evidence", "direct", "")
	if err != nil {
		t.Fatal(err)
	}
	if result["Decision_Error"] != "OpenRouter connection failed or timed out" || n != 2 {
		t.Errorf("result = %v, sleeps = %d", result, n)
	}
}

func TestResearchQuestions(t *testing.T) {
	d, err := New(testSpec(t), "", Options{APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"Would a mainstream brand be comfortable appearing beside this content?",
		"What is its primary purpose? Options: Education, Entertainment, Unknown",
	}
	if !reflect.DeepEqual(d.ResearchQuestions(), want) {
		t.Errorf("questions = %v", d.ResearchQuestions())
	}
}

func TestDecisionCostIsRecordedAndTypesafeFallbackCountsAsUnpriced(t *testing.T) {
	spec := testSpecFrom(t, `{"categories": [{"name": "Educational", "type": "boolean", "question": "Teach?"}]}`)
	answer := `"answers": {"c0": {"type": "noul", "noul": 0.8}}`
	s := newServer(t,
		reply{body: `{` + answer + `, "model": "typesafe/jev-1.13", "usage": {"cost": 0.0001}}`},
		reply{status: 402},
		reply{body: `{` + answer + `, "model": "jev-1.13.0"}`},
	)
	d, err := New(spec, "", Options{APIKey: "k", TypesafeAPIKey: "t",
		OpenRouterURL: s.srv.URL + "/api/alpha/decisions", TypesafeURL: s.srv.URL + "/v1/systemone"})
	if err != nil {
		t.Fatal(err)
	}
	d.sleep = func(context.Context, time.Duration) error { return nil }
	run := cost.NewMeter()
	ctx := cost.WithRun(context.Background(), run)
	for i := 0; i < 2; i++ {
		result, err := d.Classify(ctx, "x", "Evidence", "direct", "")
		if err != nil || result["Decision_Status"] != "success" {
			t.Fatalf("result = %v, %v", result, err)
		}
	}
	snap := run.Snapshot()
	if snap.USD != 0.0001 || snap.Calls != 2 || snap.UnpricedCalls != 1 {
		t.Errorf("snapshot = %+v", snap)
	}
}

func TestConcurrentClassifyIsBounded(t *testing.T) {
	noTypesafeEnv(t)
	var mu sync.Mutex
	inFlight, peak := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
		_, _ = io.WriteString(w, answerJSON)
	}))
	t.Cleanup(srv.Close)
	d, err := New(testSpec(t), "", Options{APIKey: "k", OpenRouterURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r, err := d.Classify(context.Background(), "x", "Evidence", "direct", ""); err != nil || r["Decision_Status"] != "success" {
				t.Errorf("result = %v, %v", r, err)
			}
		}()
	}
	wg.Wait()
	if peak > maxInFlight || peak == 0 {
		t.Errorf("peak = %d", peak)
	}
}

func TestFormatCell(t *testing.T) {
	cases := map[any]string{
		nil: "", "x": "x", 0.73: "0.73", 1.0: "1.0", 0.1: "0.1", 1e-05: "1e-05",
		int64(1): "1", 0.0: "0.0", 1e16: "1e+16", 123456789.125: "123456789.125",
	}
	for v, want := range cases {
		if got := FormatCell(v); got != want {
			t.Errorf("FormatCell(%v) = %q, want %q", v, got, want)
		}
	}
}

func TestLegacyColumnNames(t *testing.T) {
	cols := Columns(testSpec(t))
	for old, current := range LegacyColumnNames {
		if !strings.HasPrefix(old, "TypeSafe_") || !contains(cols, current) {
			t.Errorf("%s -> %s", old, current)
		}
	}
	if Columns(nil) != nil {
		t.Error("nil spec has columns")
	}
}

// Python's casefold folds multi-rune expansions, so these are duplicates.
func TestChoiceUniquenessUsesFullCaseFolding(t *testing.T) {
	for _, pair := range [][2]string{{"ẞ", "ss"}, {"ﬀ", "ff"}, {"Straße", "STRASSE"}} {
		spec := map[string]any{"categories": []any{map[string]any{
			"name": "Kind", "type": "choice", "question": "Which?", "options": []any{pair[0], pair[1]},
		}}}
		if _, err := Validate(spec); err == nil || err.Error() != "Choice options must be unique." {
			t.Errorf("options %q accepted: %v", pair, err)
		}
	}
}
