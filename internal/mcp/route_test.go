package mcp

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestBuildResultAndError(t *testing.T) {
	res := BuildResultResponse(json.RawMessage(`"abc"`), json.RawMessage(`{"tools":[]}`))
	var v struct {
		ID     string          `json:"id"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(res, &v); err != nil || v.ID != "abc" || string(v.Result) != `{"tools":[]}` {
		t.Fatalf("result response = %s (%v)", res, err)
	}
	// nil id degrades to null, still valid JSON-RPC.
	res = BuildResultResponse(nil, json.RawMessage(`{}`))
	if !strings.Contains(string(res), `"id":null`) {
		t.Fatalf("nil id = %s", res)
	}
	errRes := BuildErrorResponse(json.RawMessage(`7`), -32601, "method not found")
	var e struct {
		ID    int `json:"id"`
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(errRes, &e); err != nil || e.ID != 7 || e.Error.Code != -32601 {
		t.Fatalf("error response = %s (%v)", errRes, err)
	}
}

func TestParseToolCallNameAndRewrite(t *testing.T) {
	body := []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"web_search","arguments":{"q":"hi"}}}`)
	if got := ParseToolCallName(body); got != "web_search" {
		t.Fatalf("ParseToolCallName = %q", got)
	}
	out, err := RewriteToolCallName(body, "web_search_prime")
	if err != nil {
		t.Fatal(err)
	}
	if got := ParseToolCallName(out); got != "web_search_prime" {
		t.Fatalf("rewritten name = %q", got)
	}
	// Everything else survives semantically.
	var v map[string]json.RawMessage
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatal(err)
	}
	if string(v["method"]) != `"tools/call"` || string(v["id"]) != `2` {
		t.Fatalf("rewrite dropped members: %s", out)
	}
	var params map[string]json.RawMessage
	json.Unmarshal(v["params"], &params)
	if string(params["arguments"]) != `{"q":"hi"}` {
		t.Fatalf("arguments changed: %s", params["arguments"])
	}
	// No params at all → params synthesized with the name.
	bare := []byte(`{"jsonrpc":"2.0","id":3,"method":"tools/call"}`)
	out, err = RewriteToolCallName(bare, "x")
	if err != nil || ParseToolCallName(out) != "x" {
		t.Fatalf("bare rewrite = %s err=%v", out, err)
	}
	if _, err := RewriteToolCallName([]byte("junk"), "x"); err == nil {
		t.Fatal("invalid body must error")
	}
	if got := ParseToolCallName([]byte(`{"params":null}`)); got != "" {
		t.Fatalf("null params = %q", got)
	}
}

func TestParseToolsListResult(t *testing.T) {
	// Full response message form.
	tools, err := ParseToolsListResult([]byte(`{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"a","description":"d","inputSchema":{"type":"object"}}]}}`))
	if err != nil || len(tools) != 1 || tools[0].Name != "a" {
		t.Fatalf("full form = %+v err=%v", tools, err)
	}
	// Bare result form.
	tools, err = ParseToolsListResult([]byte(`{"tools":[{"name":"b","inputSchema":{}}]}`))
	if err != nil || len(tools) != 1 || tools[0].Name != "b" {
		t.Fatalf("bare form = %+v err=%v", tools, err)
	}
}

func TestMergeCanonicalTools(t *testing.T) {
	toolsByServer := map[string][]ToolSpec{
		"zhipu": {{Name: "web_search_prime", Description: "z search", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		"exa":   {{Name: "web_search_exa", Description: "e search", InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{}}}`)}},
		"jina":  {{Name: "jina_search", Description: "j search", InputSchema: json.RawMessage(`{}`)}},
	}
	targets := []TargetMapping{
		{Server: "zhipu", Tools: map[string]string{"web_search": "web_search_prime"}},
		{Server: "exa", Tools: map[string]string{"web_search": "web_search_exa"}},
		{Server: "jina", Tools: map[string]string{"web_search": "jina_search", "url_read": "jina_read"}},
	}
	merged := MergeCanonicalTools(toolsByServer, targets)
	if len(merged) != 1 || merged[0].Name != "web_search" || merged[0].Description != "z search" {
		t.Fatalf("merged = %+v", merged)
	}
	// First target wins the canonical name; missing backend tools are skipped.
	targets2 := []TargetMapping{
		{Server: "exa", Tools: map[string]string{"web_search": "web_search_exa"}},
		{Server: "jina", Tools: map[string]string{"web_search": "jina_search"}},
	}
	merged2 := MergeCanonicalTools(toolsByServer, targets2)
	if len(merged2) != 1 || merged2[0].Description != "e search" {
		t.Fatalf("merged2 = %+v", merged2)
	}
	// Server with no fetched tools contributes nothing.
	merged3 := MergeCanonicalTools(map[string][]ToolSpec{}, targets)
	if len(merged3) != 0 {
		t.Fatalf("merged3 = %+v", merged3)
	}
}

func TestSplitRPCMessagesAndParseInitialize(t *testing.T) {
	msgs := SplitRPCMessages("text/event-stream", []byte("event: message\ndata: {\"a\":1}\n\ndata: {\"b\":2}\n\n"))
	if len(msgs) != 2 {
		t.Fatalf("sse split = %d", len(msgs))
	}
	msgs = SplitRPCMessages("application/json", []byte(` {"a":1} `))
	if len(msgs) != 1 || string(msgs[0]) != `{"a":1}` {
		t.Fatalf("json split = %q", msgs)
	}
	if msgs := SplitRPCMessages("application/json", nil); msgs != nil {
		t.Fatalf("empty = %q", msgs)
	}
	proto, name, ver, err := ParseInitializeResult([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2024-11-05","serverInfo":{"name":"z","version":"0.1"}}}`))
	if err != nil || proto != "2024-11-05" || name != "z" || ver != "0.1" {
		t.Fatalf("initialize result = %q %q %q err=%v", proto, name, ver, err)
	}
}

func TestSessionTableRouteOps(t *testing.T) {
	tbl := NewSessionTable(4, time.Minute)
	id := tbl.PutRoute("web-search")
	// Get strips Route (pinned-side accessor never exposes route state).
	s, ok := tbl.Get(id)
	if !ok || s.Server != "web-search" || s.Route != nil {
		t.Fatalf("Get = %+v ok=%v", s, ok)
	}
	// Sub lifecycle.
	if _, ok := tbl.RouteSubGet(id, "zhipu"); ok {
		t.Fatal("phantom sub")
	}
	if !tbl.RouteSubPut(id, SubSession{Server: "zhipu", Account: "z#1", UpstreamID: "u1", Protocol: "2024-11-05", Initialized: true}) {
		t.Fatal("RouteSubPut failed")
	}
	sub, ok := tbl.RouteSubGet(id, "zhipu")
	if !ok || sub.Account != "z#1" || !sub.Initialized {
		t.Fatalf("sub = %+v", sub)
	}
	subs := tbl.RouteSubs(id)
	if len(subs) != 1 || subs[0].Server != "zhipu" {
		t.Fatalf("subs = %+v", subs)
	}
	tbl.RouteSubDrop(id, "zhipu")
	if _, ok := tbl.RouteSubGet(id, "zhipu"); ok {
		t.Fatal("drop failed")
	}
	// Sticky.
	tbl.RouteStickyPut(id, "web_search", "exa")
	server, ok := tbl.RouteStickyGet(id, "web_search")
	if !ok || server != "exa" {
		t.Fatalf("sticky = %q ok=%v", server, ok)
	}
	// Tools cache.
	if _, ok := tbl.RouteToolsGet(id); ok {
		t.Fatal("tools cache should start empty")
	}
	tbl.RouteToolsPut(id, []ToolSpec{{Name: "web_search"}})
	tools, ok := tbl.RouteToolsGet(id)
	if !ok || len(tools) != 1 || tools[0].Name != "web_search" {
		t.Fatalf("tools = %+v ok=%v", tools, ok)
	}
	// Ops on pinned (non-route) sessions and unknown ids fail cleanly.
	pinID := tbl.Put("srv", "acct", "up")
	if _, ok := tbl.RouteSubGet(pinID, "x"); ok {
		t.Fatal("route op leaked into pinned session")
	}
	if tbl.RouteSubPut("ghost-id", SubSession{Server: "x"}) {
		t.Fatal("put on unknown session succeeded")
	}
	// Delete clears everything.
	tbl.Delete(id)
	if _, ok := tbl.RouteStickyGet(id, "web_search"); ok {
		t.Fatal("deleted session still serves sticky")
	}
}
