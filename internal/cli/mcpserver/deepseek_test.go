package mcpserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// rpcResponse is the assertion-side parse of one JSON-RPC response line.
type rpcResponse struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// callText extracts the single text block of a tools/call result.
func callText(t *testing.T, resp rpcResponse) string {
	t.Helper()
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("parse tools/call result: %v", err)
	}
	if len(res.Content) != 1 || res.Content[0].Type != "text" {
		t.Fatalf("want exactly one text content block, got %+v", res.Content)
	}
	return res.Content[0].Text
}

// newTestServer returns a deepseekServer pointed at a recording fake API.
// The handler asserts Bearer auth and JSON bodies per path; hits counts the
// egressed requests (contract violations must leave it at zero).
func newTestServer(t *testing.T, handler http.HandlerFunc) (*deepseekServer, *httptest.Server, *int32) {
	t.Helper()
	var hits int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q, want Bearer test-key", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		handler(w, r)
	}))
	t.Cleanup(ts.Close)
	return &deepseekServer{client: ts.Client(), base: ts.URL, key: "test-key"}, ts, &hits
}

// dispatchCall sends one JSON-RPC line and parses the answer.
func dispatchCall(t *testing.T, s *deepseekServer, line string) rpcResponse {
	t.Helper()
	raw := s.dispatch([]byte(line))
	if raw == nil {
		t.Fatal("dispatch returned nil (silent) for a request expecting a response")
	}
	var resp rpcResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("parse response %s: %v", raw, err)
	}
	return resp
}

func initializeLine(id, protocol string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":"initialize","params":{"protocolVersion":%q,"capabilities":{},"clientInfo":{"name":"probe","version":"1"}}}`, id, protocol)
}

func TestDispatchInitializeEchoesClientProtocol(t *testing.T) {
	s := &deepseekServer{}
	resp := dispatchCall(t, s, initializeLine("7", "2024-11-05"))
	var res struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("parse initialize result: %v", err)
	}
	if res.ProtocolVersion != "2024-11-05" {
		t.Errorf("protocolVersion = %q, want echo of client's 2024-11-05", res.ProtocolVersion)
	}
	if res.ServerInfo.Name != "model-proxy-deepseek-search" {
		t.Errorf("serverInfo.name = %q", res.ServerInfo.Name)
	}
	if string(resp.ID) != "7" {
		t.Errorf("id = %s, want 7 (numeric echo)", resp.ID)
	}
	// Empty client version falls back to the current protocol revision.
	resp = dispatchCall(t, s, initializeLine(`"abc"`, ""))
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("parse initialize result: %v", err)
	}
	if res.ProtocolVersion != "2025-03-26" {
		t.Errorf("default protocolVersion = %q, want 2025-03-26", res.ProtocolVersion)
	}
}

func TestToolsListSurface(t *testing.T) {
	s := &deepseekServer{}
	resp := dispatchCall(t, s, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	var res struct {
		Tools []struct {
			Name        string          `json:"name"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("parse tools/list: %v", err)
	}
	if len(res.Tools) != 2 {
		t.Fatalf("want 2 tools, got %d (%+v)", len(res.Tools), res.Tools)
	}
	if res.Tools[0].Name != "web_search" || res.Tools[1].Name != "url_fetch" {
		t.Fatalf("tool names = %s, %s; want web_search, url_fetch", res.Tools[0].Name, res.Tools[1].Name)
	}
	for _, tool := range res.Tools {
		var schema map[string]any
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			t.Errorf("tool %s inputSchema is not valid JSON: %v", tool.Name, err)
		}
	}
}

func TestDispatchNotificationsAndPing(t *testing.T) {
	s := &deepseekServer{}
	if got := s.dispatch([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); got != nil {
		t.Fatalf("initialized notification answered: %s", got)
	}
	resp := dispatchCall(t, s, `{"jsonrpc":"2.0","id":9,"method":"ping"}`)
	if string(resp.Result) != "{}" {
		t.Errorf("ping result = %s, want {}", resp.Result)
	}
}

func TestDispatchProtocolFaults(t *testing.T) {
	s := &deepseekServer{}
	cases := []struct {
		line string
		code int
	}{
		{`not json`, -32700},
		{`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`, -32600},
		{`{"jsonrpc":"2.0","id":3,"method":"resources/list"}`, -32601},
		{`{"jsonrpc":"2.0","id":4,"method":"","params":{}}`, -32600},
		{`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"nope"}}`, -32602},
	}
	for _, tc := range cases {
		resp := dispatchCall(t, s, tc.line)
		if resp.Error == nil || resp.Error.Code != tc.code {
			t.Errorf("%s: error = %+v, want code %d", tc.line, resp.Error, tc.code)
		}
	}
}

func searchResultsBody() string {
	return `{"results":[
		{"rerank_score":2.4,"title":"DeepSeek V4 released","url":"https://example.com/v4","snippet":"short snippet","summary":"Longer model-friendly summary of the release.","site_name":"Example News","date":"2026-06-01T00:00:00+00:00","authority":3},
		{"rerank_score":1.1,"title":"Second hit","url":"https://other.example/x"}
	]}`
}

func TestCallSearchHappyPath(t *testing.T) {
	var body map[string]any
	s, _, hits := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tools/search" {
			t.Errorf("path = %q, want /v1/tools/search", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		fmt.Fprint(w, searchResultsBody())
	})
	resp := dispatchCall(t, s, `{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"web_search","arguments":{"query":"deepseek mcp","count":5,"need_content":true}}}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	if body["query"] != "deepseek mcp" || body["count"] != float64(5) || body["need_content"] != true {
		t.Errorf("upstream body = %v, want query/count/need_content passed through", body)
	}
	text := callText(t, resp)
	for _, want := range []string{
		`2 results for "deepseek mcp" (DeepSeek)`,
		"1. DeepSeek V4 released",
		"https://example.com/v4",
		"Example News · 2026-06-01T00:00:00+00:00 · score 2.4",
		"Longer model-friendly summary of the release.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("result text missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "short snippet") {
		t.Errorf("snippet shown although summary exists:\n%s", text)
	}
	if atomic.LoadInt32(hits) != 1 {
		t.Errorf("upstream hits = %d, want 1", *hits)
	}
}

func TestCallSearchOmitsDefaultFields(t *testing.T) {
	var body map[string]any
	s, _, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		fmt.Fprint(w, `{"results":[]}`)
	})
	resp := dispatchCall(t, s, `{"jsonrpc":"2.0","id":12,"method":"tools/call","params":{"name":"web_search","arguments":{"query":"q"}}}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	if _, ok := body["count"]; ok {
		t.Errorf("count sent although unset: %v", body)
	}
	if _, ok := body["need_content"]; ok {
		t.Errorf("need_content sent although false: %v", body)
	}
	if text := callText(t, resp); text != "0 results for \"q\" (DeepSeek)\n" {
		t.Errorf("zero-result text = %q", text)
	}
}

func TestCallSearchContractViolationsNeverEgress(t *testing.T) {
	s, _, hits := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("contract violation reached the upstream API")
	})
	cases := []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"web_search","arguments":{}}}`,                                 // query missing
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"web_search","arguments":{"query":""}}}`,                       // query empty
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"web_search","arguments":{"query":"q","count":0}}}`,            // count below 1
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"web_search","arguments":{"query":"q","count":51}}}`,           // count above 50
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"web_search","arguments":{"query":"q","need_content":"yes"}}}`, // wrong type
	}
	for _, line := range cases {
		resp := dispatchCall(t, s, line)
		if resp.Error == nil || resp.Error.Code != -32602 {
			t.Errorf("%s: error = %+v, want -32602", line, resp.Error)
		}
	}
	if n := atomic.LoadInt32(hits); n != 0 {
		t.Errorf("upstream hits = %d, want 0 (invalid calls must not be billed)", n)
	}
}

func TestCallSearchUpstreamErrorIsToolError(t *testing.T) {
	s, _, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		fmt.Fprint(w, `{"error":{"message":"Insufficient Balance","type":"insufficient_balance","param":null,"code":"invalid_request_error"}}`)
	})
	resp := dispatchCall(t, s, `{"jsonrpc":"2.0","id":21,"method":"tools/call","params":{"name":"web_search","arguments":{"query":"q"}}}`)
	if resp.Error != nil {
		t.Fatalf("upstream failure must be a tool error, not JSON-RPC error: %+v", resp.Error)
	}
	var res struct {
		IsError bool `json:"isError"`
	}
	_ = json.Unmarshal(resp.Result, &res)
	if !res.IsError {
		t.Fatalf("isError not set: %s", resp.Result)
	}
	if text := callText(t, resp); !strings.Contains(text, "HTTP 402") || !strings.Contains(text, "Insufficient Balance") {
		t.Errorf("error text = %q, want HTTP 402 + upstream message", text)
	}
}

func TestMissingKeyFailsClosedAtCallTime(t *testing.T) {
	s := &deepseekServer{client: http.DefaultClient, base: "http://127.0.0.1:1", key: ""} // unroutable base: must not be reached
	resp := dispatchCall(t, s, `{"jsonrpc":"2.0","id":31,"method":"tools/call","params":{"name":"web_search","arguments":{"query":"q"}}}`)
	var res struct {
		IsError bool `json:"isError"`
	}
	_ = json.Unmarshal(resp.Result, &res)
	if !res.IsError {
		t.Fatalf("missing key must surface as tool error: %s", resp.Result)
	}
	if text := callText(t, resp); !strings.Contains(text, "DEEPSEEK_API_KEY") {
		t.Errorf("error text = %q, want the env-var guidance", text)
	}
}

func TestCallFetchHappyPathAndValidation(t *testing.T) {
	var body map[string]any
	s, _, hits := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tools/fetch" {
			t.Errorf("path = %q, want /v1/tools/fetch", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		fmt.Fprint(w, `{"title":"Example article","content":"# Example article\n\nBody."}`)
	})
	resp := dispatchCall(t, s, `{"jsonrpc":"2.0","id":41,"method":"tools/call","params":{"name":"url_fetch","arguments":{"url":"https://example.com/a"}}}`)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	if body["url"] != "https://example.com/a" {
		t.Errorf("upstream body = %v", body)
	}
	if text := callText(t, resp); text != "# Example article\n\n# Example article\n\nBody." {
		t.Errorf("fetch text = %q", text)
	}
	// Invalid targets are rejected before egress (documented fetch contract).
	for _, line := range []string{
		`{"jsonrpc":"2.0","id":42,"method":"tools/call","params":{"name":"url_fetch","arguments":{"url":"ftp://example.com/a"}}}`,
		`{"jsonrpc":"2.0","id":43,"method":"tools/call","params":{"name":"url_fetch","arguments":{"url":"https://user:pass@example.com/a"}}}`,
		`{"jsonrpc":"2.0","id":44,"method":"tools/call","params":{"name":"url_fetch","arguments":{"url":"/relative"}}}`,
	} {
		resp := dispatchCall(t, s, line)
		if resp.Error == nil || resp.Error.Code != -32602 {
			t.Errorf("%s: error = %+v, want -32602", line, resp.Error)
		}
	}
	if n := atomic.LoadInt32(hits); n != 1 {
		t.Errorf("upstream hits = %d, want 1", n)
	}
}

func TestAPIErrorMessage(t *testing.T) {
	if got := apiErrorMessage([]byte(`{"error":{"message":"boom","code":"x"}}`)); got != "boom (code x)" {
		t.Errorf("structured message = %q", got)
	}
	if got := apiErrorMessage([]byte("plain text")); got != "plain text" {
		t.Errorf("raw fallback = %q", got)
	}
	if got := apiErrorMessage(nil); got != "<empty body>" {
		t.Errorf("empty fallback = %q", got)
	}
	long := strings.Repeat("x", 300)
	if got := apiErrorMessage([]byte(long)); len(got) != 200 {
		t.Errorf("raw prefix not trimmed to 200: %d", len(got))
	}
}
