// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/ElcanoTek/lens/internal/api"
	"github.com/ElcanoTek/lens/internal/apitoken"
	"github.com/ElcanoTek/lens/internal/catalog"
	"github.com/ElcanoTek/lens/internal/config"
	"github.com/ElcanoTek/lens/internal/jobs"
)

const lensHost = "lens.example.test"

func TestMain(m *testing.M) {
	// Stand-in for internal/detect: one website per non-empty Domain row.
	jobs.ComputeBreakdown = func(path string) (map[string]int, error) {
		out := map[string]int{"websites": 0, "ios": 0, "android": 0, "ctv": 0, "total": 0}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		records, err := csv.NewReader(bytes.NewReader(raw)).ReadAll()
		if err != nil || len(records) == 0 || !strings.EqualFold(records[0][0], "domain") {
			return out, nil
		}
		for _, r := range records[1:] {
			if strings.TrimSpace(r[0]) != "" {
				out["websites"]++
				out["total"]++
			}
		}
		return out, nil
	}
	os.Exit(m.Run())
}

type harness struct {
	t       *testing.T
	manager *jobs.Manager
	tokens  *apitoken.Store
	server  *Server
	mux     *http.ServeMux
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	t.Setenv("LENS_PUBLIC_URL", "https://"+lensHost)
	home := t.TempDir()
	m := jobs.NewManager(home)
	if err := m.Dirs.Ensure(); err != nil {
		t.Fatal(err)
	}
	models := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data": []}`)
	}))
	t.Cleanup(models.Close)
	source := catalog.NewSource("")
	source.URL = models.URL
	tokens := apitoken.NewStore(filepath.Join(home, "api_tokens.db"))
	t.Cleanup(func() { _ = tokens.Close() })
	a := api.New(api.Deps{Manager: m, Tokens: tokens, Catalog: source, Config: config.Default()})
	s := New(a)
	mux := http.NewServeMux()
	mux.Handle("/mcp", s)
	mux.Handle("/api/v1/", a.Routes())
	return &harness{t: t, manager: m, tokens: tokens, server: s, mux: mux}
}

func (h *harness) token(scopes ...string) string {
	h.t.Helper()
	if len(scopes) == 0 {
		scopes = apitoken.Scopes
	}
	_, raw, err := h.tokens.Create(context.Background(), "fleet-test", scopes)
	if err != nil {
		h.t.Fatal(err)
	}
	return raw
}

func (h *harness) post(token string, body []byte, headers ...string) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	return rec
}

func (h *harness) rpc(token, method string, params any) map[string]any {
	h.t.Helper()
	msg := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		msg["params"] = params
	}
	raw, _ := json.Marshal(msg)
	rec := h.post(token, raw)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		h.t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	return out
}

func (h *harness) call(token, name string, args map[string]any) map[string]any {
	h.t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	out := h.rpc(token, "tools/call", map[string]any{"name": name, "arguments": args})
	result, ok := out["result"].(map[string]any)
	if !ok {
		h.t.Fatalf("no result: %v", out)
	}
	var fromText any
	text := result["content"].([]any)[0].(map[string]any)["text"].(string)
	if err := json.Unmarshal([]byte(text), &fromText); err != nil || !reflect.DeepEqual(fromText, result["structuredContent"]) {
		h.t.Fatalf("text and structuredContent differ: %s", text)
	}
	return result
}

func TestRequiresAToken(t *testing.T) {
	h := newHarness(t)
	rec := h.post("", []byte(`{"jsonrpc": "2.0", "id": 1, "method": "initialize"}`))
	if rec.Code != 401 || !strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), "Bearer") {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		rec := httptest.NewRecorder()
		h.mux.ServeHTTP(rec, httptest.NewRequest(method, "/mcp", nil))
		if rec.Code != 405 || rec.Header().Get("Allow") != "POST" {
			t.Fatalf("%s: %d", method, rec.Code)
		}
	}
}

func TestHandshake(t *testing.T) {
	h := newHarness(t)
	raw := h.token()
	msg, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{
		"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "fleet", "version": "x"},
	}})
	rec := h.post(raw, msg)
	if rec.Header().Get("Mcp-Session-Id") != "" || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("headers %v", rec.Header())
	}
	var init map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &init)
	result := init["result"].(map[string]any)
	if result["protocolVersion"] != "2025-06-18" || result["serverInfo"].(map[string]any)["name"] != "lens" ||
		!strings.Contains(result["instructions"].(string), "create_job") {
		t.Fatalf("init = %v", init)
	}
	if !reflect.DeepEqual(result["capabilities"], map[string]any{"tools": map[string]any{"listChanged": false}}) {
		t.Fatalf("capabilities = %v", result["capabilities"])
	}
	unknown := h.rpc(raw, "initialize", map[string]any{"protocolVersion": "1999-01-01"})
	if unknown["result"].(map[string]any)["protocolVersion"] != SupportedProtocolVersions()[0] {
		t.Fatalf("unknown = %v", unknown)
	}
	// The 2024 HTTP+SSE transport is not served, so it is never agreed to.
	legacy := h.rpc(raw, "initialize", map[string]any{"protocolVersion": "2024-11-05"})
	if legacy["result"].(map[string]any)["protocolVersion"] == "2024-11-05" {
		t.Fatal("agreed to 2024-11-05")
	}
	if note := h.post(raw, []byte(`{"jsonrpc": "2.0", "method": "notifications/initialized"}`)); note.Code != 202 || note.Body.Len() != 0 {
		t.Fatalf("notification: %d %s", note.Code, note.Body)
	}
	if ping := h.rpc(raw, "ping", nil); !reflect.DeepEqual(ping["result"], map[string]any{}) || ping["id"] != 1.0 {
		t.Fatalf("ping = %v", ping)
	}
	if got := h.rpc(raw, "resources/list", nil)["error"].(map[string]any)["code"]; got != -32601.0 {
		t.Fatalf("resources/list = %v", got)
	}
	if batch := h.post(raw, []byte(`[{"jsonrpc": "2.0", "id": 1, "method": "ping"}]`)); batch.Code != 400 {
		t.Fatalf("batch: %d", batch.Code)
	}
	for body, want := range map[string]int{`{bad`: 400, `{"jsonrpc": "1.0", "id": 1, "method": "ping"}`: 400, `"x"`: 400, `{"jsonrpc": "2.0", "id": 1, "method": 5}`: 400} {
		if rec := h.post(raw, []byte(body)); rec.Code != want {
			t.Fatalf("%s: %d", body, rec.Code)
		}
	}
	// A string id is echoed back as sent.
	rec = h.post(raw, []byte(`{"jsonrpc": "2.0", "id": "abc", "method": "ping"}`))
	if !strings.Contains(rec.Body.String(), `"id":"abc"`) {
		t.Fatalf("id: %s", rec.Body)
	}
}

func TestRejectsForeignBrowserOrigins(t *testing.T) {
	h := newHarness(t)
	raw := h.token()
	ping := []byte(`{"jsonrpc": "2.0", "id": 1, "method": "ping"}`)
	if rec := h.post(raw, ping, "Origin", "https://evil.example"); rec.Code != 403 {
		t.Fatalf("foreign origin: %d", rec.Code)
	}
	if rec := h.post(raw, ping, "Origin", "https://"+lensHost); rec.Code != 200 {
		t.Fatalf("own origin: %d", rec.Code)
	}
	if rec := h.post(raw, ping, "Origin", "http://example.com"); rec.Code != 200 { // httptest's Host
		t.Fatalf("host origin: %d", rec.Code)
	}
}

func TestToolsAreFilteredByScopeAndRecheckedOnCall(t *testing.T) {
	h := newHarness(t)
	reader := h.token("jobs:read")
	tools := h.rpc(reader, "tools/list", nil)["result"].(map[string]any)["tools"].([]any)
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.(map[string]any)["name"].(string))
	}
	sort.Strings(names)
	if !reflect.DeepEqual(names, []string{"get_download_link", "get_job", "get_job_results", "get_options", "list_input_files", "list_jobs"}) {
		t.Fatalf("names = %v", names)
	}
	refused := h.rpc(reader, "tools/call", map[string]any{"name": "create_job", "arguments": map[string]any{"items": []string{"a.com"}}})
	if refused["error"].(map[string]any)["code"] != -32602.0 {
		t.Fatalf("refused = %v", refused)
	}
	full := map[string]map[string]any{}
	for _, tool := range h.rpc(h.token(), "tools/list", nil)["result"].(map[string]any)["tools"].([]any) {
		m := tool.(map[string]any)
		full[m["name"].(string)] = m
		if _, leaked := m["scope"]; leaked {
			t.Fatal("scope leaked into the listing")
		}
	}
	if len(full) != len(ToolNames()) {
		t.Fatalf("full = %v", full)
	}
	upload := full["create_job"]["inputSchema"].(map[string]any)["properties"].(map[string]any)["content_base64"].(map[string]any)
	// Fleet only offers workspace file references for exactly this annotation.
	if upload["type"] != "string" || upload["contentEncoding"] != "base64" {
		t.Fatalf("upload = %v", upload)
	}
	if full["get_job"]["annotations"].(map[string]any)["readOnlyHint"] != true ||
		full["cancel_job"]["annotations"].(map[string]any)["destructiveHint"] != true {
		t.Fatal("annotations")
	}
}

func TestEndToEndJobFlow(t *testing.T) {
	h := newHarness(t)
	raw := h.token()
	created := h.call(raw, "create_job", map[string]any{
		"name":           "chat list",
		"content_base64": base64.StdEncoding.EncodeToString([]byte("Domain\nexample.com\nexample.org\nexample.net\n")),
		"filename":       "chat list.csv",
	})
	body := created["structuredContent"].(map[string]any)
	if created["isError"] != false || body["ok"] != true || body["breakdown"].(map[string]any)["websites"] != 3.0 {
		t.Fatalf("created = %v", created)
	}
	jobID := body["id"].(string)

	listed := h.call(raw, "list_jobs", map[string]any{"status": "queued"})["structuredContent"].(map[string]any)
	if jobsList := listed["jobs"].([]any); len(jobsList) != 1 || jobsList[0].(map[string]any)["id"] != jobID {
		t.Fatalf("listed = %v", listed)
	}
	inputs := h.call(raw, "list_input_files", nil)["structuredContent"].(map[string]any)["files"].([]any)
	if inputs[0].(map[string]any)["name"] != "chat list.csv" {
		t.Fatalf("inputs = %v", inputs)
	}

	finish(t, h.manager, jobID)
	status := h.call(raw, "get_job", map[string]any{"job_id": jobID})["structuredContent"].(map[string]any)
	if status["status"] != "completed" || status["progress"].(map[string]any)["percent"] != 100.0 ||
		!reflect.DeepEqual(status["files"], []any{"output", "log", "input"}) || status["cost_usd"] != 0.0123 {
		t.Fatalf("status = %v", status)
	}
	results := h.call(raw, "get_job_results", map[string]any{"job_id": jobID, "limit": 0})["structuredContent"].(map[string]any)
	if len(results["rows"].([]any)) != 0 || results["total_rows"] != 3.0 {
		t.Fatalf("results = %v", results)
	}
	premium := h.call(raw, "get_job_results", map[string]any{"job_id": jobID, "quality": "Premium", "columns": []string{"Domain"}, "limit": 1})["structuredContent"].(map[string]any)
	if !reflect.DeepEqual(premium["rows"], []any{map[string]any{"Domain": "example.com"}}) {
		t.Fatalf("premium = %v", premium)
	}

	link := h.call(raw, "get_download_link", map[string]any{"job_id": jobID})["structuredContent"].(map[string]any)
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, strings.SplitN(link["url"].(string), lensHost, 2)[1], nil))
	records, _ := csv.NewReader(rec.Body).ReadAll()
	if rec.Code != 200 || len(records) != 4 || records[1][0] != "example.com" {
		t.Fatalf("%d %v", rec.Code, records)
	}

	options := h.call(raw, "get_options", nil)["structuredContent"].(map[string]any)
	if options["defaults"].(map[string]any)["llm_model"] != catalog.RecommendedModel {
		t.Fatalf("options = %v", options)
	}
	cancel := h.call(raw, "cancel_job", map[string]any{"job_id": jobID})
	if cancel["isError"] != true || cancel["structuredContent"].(map[string]any)["code"] != "not_cancellable" {
		t.Fatalf("cancel = %v", cancel)
	}
}

// finish marks a job completed with a three-row output, the way the
// pipeline would leave it.
func finish(t *testing.T, m *jobs.Manager, id string) {
	t.Helper()
	out := m.Dirs.Outputs
	files := map[string]string{
		id + "_output.csv":    "Domain,Type,Quality,IAB Tier 1\nexample.com,WEBSITE,Premium,News\nexample.org,WEBSITE,Standard,News\nexample.net,WEBSITE,Premium,Technology\n",
		id + "_progress.json": `{"total_domains": 3, "processed_domains": {"a": {}, "b": {}, "c": {}}, "cost": {"usd": "0.0123", "calls": 4}}`,
		id + ".log":           "started\nfinished\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(out, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := os.ReadFile(m.Dirs.StatePath())
	var doc map[string]any
	_ = json.Unmarshal(raw, &doc)
	for _, j := range doc["jobs"].([]any) {
		if job := j.(map[string]any); job["id"] == id {
			job["status"], job["output_file"], job["progress_file"], job["log_file"] = "completed", id+"_output.csv", id+"_progress.json", id+".log"
		}
	}
	raw, _ = json.Marshal(doc)
	if err := os.WriteFile(m.Dirs.StatePath(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	m.Load()
}

func TestToolErrorsAreResultsNotProtocolFailures(t *testing.T) {
	h := newHarness(t)
	raw := h.token()
	missing := h.call(raw, "get_job", map[string]any{"job_id": "20260101000000-abcdef"})
	if missing["isError"] != true || missing["structuredContent"].(map[string]any)["code"] != "job_not_found" {
		t.Fatalf("missing = %v", missing)
	}
	extra := h.call(raw, "get_job", map[string]any{"job_id": "20260101000000-abcdef", "bogus": 1})
	if !strings.Contains(extra["structuredContent"].(map[string]any)["error"].(string), "Unknown argument") {
		t.Fatalf("extra = %v", extra)
	}
	required := h.call(raw, "get_job", nil)
	if !strings.Contains(required["structuredContent"].(map[string]any)["error"].(string), "Missing required") {
		t.Fatalf("required = %v", required)
	}
	badLimit := h.call(raw, "list_jobs", map[string]any{"limit": true})
	if badLimit["structuredContent"].(map[string]any)["code"] != "invalid_argument" {
		t.Fatalf("badLimit = %v", badLimit)
	}
}

func TestChunkedBodiesAreCappedWhileStreaming(t *testing.T) {
	h := newHarness(t)
	h.server.MaxBodyBytes = 4096
	big, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping", "pad": strings.Repeat("x", 10_000)})
	req := httptest.NewRequest(http.MethodPost, "/mcp", io.MultiReader(bytes.NewReader(big)))
	req.ContentLength = -1
	req.TransferEncoding = []string{"chunked"}
	req.Header.Set("Authorization", "Bearer "+h.token())
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	if rec.Code != 413 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestRejectsFalseyNonObjectParams(t *testing.T) {
	h := newHarness(t)
	raw := h.token()
	for _, bad := range []string{"[]", "false", "0", `"x"`} {
		rec := h.post(raw, []byte(`{"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": `+bad+`}`))
		if !strings.Contains(rec.Body.String(), `"code":-32602`) {
			t.Fatalf("%s: %s", bad, rec.Body)
		}
	}
	if got := h.rpc(raw, "tools/call", map[string]any{"name": "get_options", "arguments": []any{}}); got["error"].(map[string]any)["code"] != -32602.0 {
		t.Fatalf("got %v", got)
	}
	if got := h.rpc(raw, "tools/list", nil); got["result"] == nil { // null params is "none"
		t.Fatalf("got %v", got)
	}
}

func TestToolSchemasUseOnlyECMAScriptRegex(t *testing.T) {
	var patterns []string
	var walk func(any)
	walk = func(node any) {
		switch n := node.(type) {
		case map[string]any:
			for k, v := range n {
				if s, ok := v.(string); ok && k == "pattern" {
					patterns = append(patterns, s)
				} else {
					walk(v)
				}
			}
		case []any:
			for _, v := range n {
				walk(v)
			}
		}
	}
	for _, tool := range defs.Tools {
		var schema any
		_ = json.Unmarshal(tool.InputSchema, &schema)
		walk(schema)
	}
	if len(patterns) == 0 {
		t.Fatal("no patterns")
	}
	for _, p := range patterns {
		if strings.Contains(p, "(?") { // inline flags are not ECMAScript
			t.Fatalf("pattern %s", p)
		}
		regexp.MustCompile(p)
	}
	var schema struct {
		Properties map[string]struct {
			Pattern string `json:"pattern"`
		} `json:"properties"`
	}
	_ = json.Unmarshal(findTool("create_job").InputSchema, &schema)
	upload := regexp.MustCompile(schema.Properties["filename"].Pattern)
	if !upload.MatchString("List.XLSX") || !upload.MatchString("a.csv") || upload.MatchString("a.exe") || upload.MatchString("a/b.csv") {
		t.Fatal("filename pattern")
	}
}

// The tool schemas are the Python release's, verbatim; their limits must
// still be the API's.
func TestSchemaLimitsMatchTheAPI(t *testing.T) {
	var schema struct {
		Properties map[string]map[string]any `json:"properties"`
	}
	_ = json.Unmarshal(findTool("create_job").InputSchema, &schema)
	items := schema.Properties["items"]
	if items["maxItems"] != float64(api.MaxInlineItems) || items["items"].(map[string]any)["maxLength"] != float64(api.MaxItemChars) {
		t.Fatalf("items = %v", items)
	}
	props := make([]string, 0, len(schema.Properties))
	for name := range schema.Properties {
		props = append(props, name)
	}
	sort.Strings(props)
	if !reflect.DeepEqual(props, api.CreateJobArgs) {
		t.Fatalf("create_job properties %v != %v", props, api.CreateJobArgs)
	}
	_ = json.Unmarshal(findTool("list_jobs").InputSchema, &schema)
	enum := schema.Properties["status"]["enum"].([]any)
	if len(enum) != len(api.JobStatuses) || schema.Properties["limit"]["maximum"] != float64(api.MaxJobList) {
		t.Fatalf("list_jobs = %v", schema.Properties)
	}
	for i, s := range api.JobStatuses {
		if enum[i] != s {
			t.Fatalf("enum = %v", enum)
		}
	}
	_ = json.Unmarshal(findTool("get_download_link").InputSchema, &schema)
	if kinds := schema.Properties["file"]["enum"].([]any); len(kinds) != len(api.FileKinds) {
		t.Fatalf("kinds = %v", kinds)
	}
	_ = json.Unmarshal(findTool("get_job_results").InputSchema, &schema)
	if schema.Properties["limit"]["maximum"] != float64(api.MaxResultRows) {
		t.Fatal("results limit")
	}
	for _, tool := range defs.Tools {
		if tool.Scope != apitoken.ScopeRead && tool.Scope != apitoken.ScopeRun {
			t.Fatalf("%s scope %q", tool.Name, tool.Scope)
		}
	}
}
