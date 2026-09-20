package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/provider"
)

// ---- aggregated route test harness ----

type routeHit struct {
	method  string
	tool    string // tools/call name as the backend saw it
	proto   string // MCP-Protocol-Version header
	session string
	auth    string
}

// fakeRouteBackend is a per-target MCP server double for route tests.
type fakeRouteBackend struct {
	mu         sync.Mutex
	sessionID  string        // "" = stateless backend
	protocol   string        // protocolVersion to negotiate
	tools      string        // JSON array fragment of tool specs
	callStatus int           // HTTP status for tools/call (default 200)
	callBizErr bool          // answer tools/call with a JSON-RPC error over 200
	down       bool          // refuse everything with 500
	callDelay  time.Duration // artificial latency for tools/call
	hits       []routeHit
	deletes    int
}

func (f *fakeRouteBackend) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	if f.down {
		f.mu.Unlock()
		http.Error(w, "boom", http.StatusInternalServerError)
		return
	}
	deletes := f.deletes
	_ = deletes
	f.mu.Unlock()

	if r.Method == http.MethodDelete {
		f.mu.Lock()
		f.deletes++
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		return
	}
	body := readAll(r)
	method := parseRPCMethod(body)
	hit := routeHit{method: method, proto: r.Header.Get("MCP-Protocol-Version"), session: r.Header.Get("Mcp-Session-Id"), auth: r.Header.Get("Authorization")}
	if method == "tools/call" {
		var v struct {
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		json.Unmarshal(body, &v)
		hit.tool = v.Params.Name
	}
	f.mu.Lock()
	f.hits = append(f.hits, hit)
	callStatus := f.callStatus
	callBizErr := f.callBizErr
	f.mu.Unlock()

	switch method {
	case "initialize":
		if f.sessionID != "" {
			w.Header().Set("Mcp-Session-Id", f.sessionID)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":%q,"capabilities":{"tools":{}},"serverInfo":{"name":"backend","version":"1"}}}`, f.protocol)
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	case "tools/list":
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":2,"result":{"tools":%s}}`, f.tools)
	case "tools/call":
		if f.callDelay > 0 {
			time.Sleep(f.callDelay)
		}
		if callStatus != 0 && callStatus != 200 {
			http.Error(w, "backend fault", callStatus)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if callBizErr {
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":2,"error":{"code":-32000,"message":"search quota exhausted"}}`)
			return
		}
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":%q}]}}`, "served:"+hit.tool)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeRouteBackend) lastCall() routeHit {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.hits) - 1; i >= 0; i-- {
		if f.hits[i].method == "tools/call" {
			return f.hits[i]
		}
	}
	return routeHit{}
}

func (f *fakeRouteBackend) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, h := range f.hits {
		if h.method == "tools/call" {
			n++
		}
	}
	return n
}

func (f *fakeRouteBackend) sawDelete() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deletes
}

// newMCPRouteTestProxyP builds a Proxy with one zhipu pool account plus the
// given mcp: servers and mcp_routes:, returning both the Proxy (for direct
// state injection) and the client-facing test server.
func newMCPRouteTestProxyP(t *testing.T, keys []string, servers map[string]configdomain.MCPServer, routes map[string]configdomain.MCPRoute) (*Proxy, *httptest.Server) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	writeMCPKeys(t, "zhipu", keys...)
	cfg := &configdomain.Config{
		Listen: "127.0.0.1:0",
		Providers: map[string]configdomain.Provider{
			"zhipu": {Provider: "zhipu", OpenAIBaseURL: "http://127.0.0.1:1", Models: []string{"m"}},
		},
		MCP:       servers,
		MCPRoutes: routes,
	}
	p := newTestProxy(t, cfg)
	srv := httptest.NewServer(http.HandlerFunc(p.Handler))
	t.Cleanup(srv.Close)
	return p, srv
}

// newMCPRouteTestProxy builds a Proxy with one zhipu pool account plus the
// given mcp: servers and mcp_routes:.
func newMCPRouteTestProxy(t *testing.T, keys []string, servers map[string]configdomain.MCPServer, routes map[string]configdomain.MCPRoute) *httptest.Server {
	t.Helper()
	_, srv := newMCPRouteTestProxyP(t, keys, servers, routes)
	return srv
}

const routeInitBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`
const routeListBody = `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`

func routeCallBody(tool string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":%q,"arguments":{"q":"x"}}}`, tool)
}

// twoRouteBackends wires zhipu-backed (sessionful, 2024-11-05) and anonymous
// (stateless, 2025-03-26) backends with a shared canonical web_search tool.
func twoRouteBackends(t *testing.T) (zs, exa *fakeRouteBackend, servers map[string]configdomain.MCPServer, routes map[string]configdomain.MCPRoute) {
	zs = &fakeRouteBackend{sessionID: "zs-sid", protocol: "2024-11-05",
		tools: `[{"name":"web_search_prime","description":"zhipu search","inputSchema":{"type":"object"}}]`}
	exa = &fakeRouteBackend{protocol: "2025-03-26",
		tools: `[{"name":"web_search_exa","description":"exa search","inputSchema":{"type":"object","properties":{"q":{}}}}]`}
	zsSrv := httptest.NewServer(http.HandlerFunc(zs.serve))
	exaSrv := httptest.NewServer(http.HandlerFunc(exa.serve))
	t.Cleanup(zsSrv.Close)
	t.Cleanup(exaSrv.Close)
	servers = map[string]configdomain.MCPServer{
		"zs":  {Provider: "zhipu", URL: zsSrv.URL},
		"exa": {URL: exaSrv.URL, Auth: "none"},
	}
	routes = map[string]configdomain.MCPRoute{
		"web-search": {Targets: []configdomain.MCPRouteTarget{
			{Server: "zs", Tools: map[string]string{"web_search": "web_search_prime"}},
			{Server: "exa", Tools: map[string]string{"web_search": "web_search_exa"}},
		}},
	}
	return zs, exa, servers, routes
}

// TestMCPRoute_FullFlow: synthesized initialize → aggregated tools/list →
// routed+sticky tools/call with name rewrite and per-backend protocol →
// DELETE fan-out.
func TestMCPRoute_FullFlow(t *testing.T) {
	zs, exa, servers, routes := twoRouteBackends(t)
	srv := newMCPRouteTestProxy(t, []string{"k-A"}, servers, routes)

	// initialize: proxy-synthesized, protocol echoed, session minted.
	resp := mcpPost(t, srv.URL+"/mcp/web-search", "", routeInitBody)
	if resp.StatusCode != 200 {
		t.Fatalf("initialize = %d", resp.StatusCode)
	}
	sid := resp.Header.Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatal("no route session minted")
	}
	initBody := mustRead(resp)
	if !strings.Contains(initBody, `"model-proxy route web-search"`) || !strings.Contains(initBody, `"protocolVersion":"2025-03-26"`) {
		t.Fatalf("initialize body = %s", initBody)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("initialize content-type = %q", ct)
	}

	// tools/list: canonical surface from the first target's schema.
	resp = mcpPost(t, srv.URL+"/mcp/web-search", sid, routeListBody)
	listBody := mustRead(resp)
	if !strings.Contains(listBody, `"name":"web_search"`) || !strings.Contains(listBody, `"zhipu search"`) {
		t.Fatalf("tools/list = %s", listBody)
	}
	if strings.Contains(listBody, "web_search_prime") || strings.Contains(listBody, "web_search_exa") {
		t.Fatalf("backend tool names leaked: %s", listBody)
	}
	// Both backends got lazily initialized + listed.
	if zs.lastCall().tool != "" || exa.lastCall().tool != "" {
		t.Fatal("no calls expected yet")
	}
	zs.mu.Lock()
	zsInits := 0
	for _, h := range zs.hits {
		if h.method == "initialize" {
			zsInits++
		}
	}
	zs.mu.Unlock()
	if zsInits != 1 {
		t.Fatalf("zs initialize count = %d", zsInits)
	}

	// tools/call: canonical → backend name rewrite, first target serves.
	resp = mcpPost(t, srv.URL+"/mcp/web-search", sid, routeCallBody("web_search"))
	callBody := mustRead(resp)
	if !strings.Contains(callBody, "served:web_search_prime") {
		t.Fatalf("call 1 = %s", callBody)
	}
	zsCall := zs.lastCall()
	if zsCall.tool != "web_search_prime" {
		t.Fatalf("backend saw tool %q", zsCall.tool)
	}
	if zsCall.proto != "2024-11-05" {
		t.Fatalf("backend protocol header = %q (want the negotiated 2024-11-05)", zsCall.proto)
	}
	if zsCall.session != "zs-sid" {
		t.Fatalf("backend session = %q", zsCall.session)
	}
	if zsCall.auth != "Bearer k-A" {
		t.Fatalf("backend auth = %q", zsCall.auth)
	}
	// Sticky: second call also goes to zs even though exa serves the tool too.
	mcpPost(t, srv.URL+"/mcp/web-search", sid, routeCallBody("web_search")).Body.Close()
	if zs.calls() != 2 || exa.calls() != 0 {
		t.Fatalf("stickiness broken: zs=%d exa=%d", zs.calls(), exa.calls())
	}

	// DELETE: both sub-sessions terminated, local session gone.
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/mcp/web-search", nil)
	req.Header.Set("Mcp-Session-Id", sid)
	dresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	dresp.Body.Close()
	if dresp.StatusCode != 200 {
		t.Fatalf("DELETE = %d", dresp.StatusCode)
	}
	if zs.sawDelete() != 1 || exa.sawDelete() != 1 {
		t.Fatalf("sub DELETE fan-out: zs=%d exa=%d", zs.sawDelete(), exa.sawDelete())
	}
	if r := mcpPost(t, srv.URL+"/mcp/web-search", sid, routeCallBody("web_search")); r.StatusCode != http.StatusNotFound {
		t.Fatalf("post-DELETE session = %d, want 404", r.StatusCode)
	}
}

// TestMCPRoute_Failover: a 5xx on the first target moves the call to the next
// one and re-sticks the session; a JSON-RPC business error never fails over.
func TestMCPRoute_Failover(t *testing.T) {
	zs, exa, servers, routes := twoRouteBackends(t)
	zs.callStatus = 500
	srv := newMCPRouteTestProxy(t, []string{"k-A"}, servers, routes)

	resp := mcpPost(t, srv.URL+"/mcp/web-search", "", routeInitBody)
	sid := resp.Header.Get("Mcp-Session-Id")
	resp.Body.Close()

	resp = mcpPost(t, srv.URL+"/mcp/web-search", sid, routeCallBody("web_search"))
	callBody := mustRead(resp)
	if !strings.Contains(callBody, "served:web_search_exa") {
		t.Fatalf("failover call = %s", callBody)
	}
	if zs.calls() != 1 || exa.calls() != 1 {
		t.Fatalf("calls: zs=%d exa=%d", zs.calls(), exa.calls())
	}
	// Sticky moved: next call goes to exa FIRST (zs stays at 1 call).
	mcpPost(t, srv.URL+"/mcp/web-search", sid, routeCallBody("web_search")).Body.Close()
	if zs.calls() != 1 || exa.calls() != 2 {
		t.Fatalf("sticky after failover: zs=%d exa=%d", zs.calls(), exa.calls())
	}
}

// TestMCPRoute_BusinessErrorNoFailover: a 200 + JSON-RPC error is the
// backend's answer, not a transport failure — returned as-is, no failover.
func TestMCPRoute_BusinessErrorNoFailover(t *testing.T) {
	zs, exa, servers, routes := twoRouteBackends(t)
	zs.callBizErr = true
	srv := newMCPRouteTestProxy(t, []string{"k-A"}, servers, routes)

	resp := mcpPost(t, srv.URL+"/mcp/web-search", "", routeInitBody)
	sid := resp.Header.Get("Mcp-Session-Id")
	resp.Body.Close()
	resp = mcpPost(t, srv.URL+"/mcp/web-search", sid, routeCallBody("web_search"))
	callBody := mustRead(resp)
	if !strings.Contains(callBody, "search quota exhausted") {
		t.Fatalf("business error not passed through: %s", callBody)
	}
	if exa.calls() != 0 {
		t.Fatalf("business error triggered failover: exa=%d", exa.calls())
	}
}

// TestMCPRoute_SessionDiscipline: no session → 400, unknown session → 404,
// GET → 405, unknown method → -32601, unknown tool → -32602.
func TestMCPRoute_SessionDiscipline(t *testing.T) {
	_, _, servers, routes := twoRouteBackends(t)
	srv := newMCPRouteTestProxy(t, []string{"k-A"}, servers, routes)

	if r := mcpPost(t, srv.URL+"/mcp/web-search", "", routeCallBody("web_search")); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("no session = %d, want 400", r.StatusCode)
	}
	if r := mcpPost(t, srv.URL+"/mcp/web-search", "bogus-sid", routeCallBody("web_search")); r.StatusCode != http.StatusNotFound {
		t.Fatalf("bogus session = %d, want 404", r.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/mcp/web-search", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d, want 405", resp.StatusCode)
	}
	resp.Body.Close()

	resp = mcpPost(t, srv.URL+"/mcp/web-search", "", routeInitBody)
	sid := resp.Header.Get("Mcp-Session-Id")
	resp.Body.Close()
	r := mcpPost(t, srv.URL+"/mcp/web-search", sid, `{"jsonrpc":"2.0","id":9,"method":"resources/list","params":{}}`)
	if body := mustRead(r); !strings.Contains(body, "-32601") {
		t.Fatalf("unknown method = %s", body)
	}
	r = mcpPost(t, srv.URL+"/mcp/web-search", sid, routeCallBody("nonexistent"))
	if body := mustRead(r); !strings.Contains(body, "unknown tool") {
		t.Fatalf("unknown tool = %s", body)
	}
	// Notifications: 202 without a body, no session fan-out errors.
	r = mcpPost(t, srv.URL+"/mcp/web-search", sid, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if r.StatusCode != http.StatusAccepted {
		t.Fatalf("notification = %d", r.StatusCode)
	}
	r.Body.Close()
}

// TestMCPRoute_ToolsListDegradation: a down backend is skipped — the route
// still exposes the surviving target's canonical surface.
func TestMCPRoute_ToolsListDegradation(t *testing.T) {
	zs, _, servers, routes := twoRouteBackends(t)
	zs.down = true
	srv := newMCPRouteTestProxy(t, []string{"k-A"}, servers, routes)

	resp := mcpPost(t, srv.URL+"/mcp/web-search", "", routeInitBody)
	sid := resp.Header.Get("Mcp-Session-Id")
	resp.Body.Close()
	resp = mcpPost(t, srv.URL+"/mcp/web-search", sid, routeListBody)
	listBody := mustRead(resp)
	if !strings.Contains(listBody, `"name":"web_search"`) || !strings.Contains(listBody, `"exa search"`) {
		t.Fatalf("degraded tools/list = %s", listBody)
	}
	// And calls still work through the surviving backend.
	resp = mcpPost(t, srv.URL+"/mcp/web-search", sid, routeCallBody("web_search"))
	if body := mustRead(resp); !strings.Contains(body, "served:web_search_exa") {
		t.Fatalf("degraded call = %s", body)
	}
}

// TestMCPRoute_RequestLogProjection: route exchanges land in the request log
// with kind="mcp"; a served tools/call names the chosen backend in provider.
func TestMCPRoute_RequestLogProjection(t *testing.T) {
	_, _, servers, routes := twoRouteBackends(t)
	t.Setenv("HOME", t.TempDir())
	writeMCPKeys(t, "zhipu", "k-A")
	cfg := &configdomain.Config{
		Listen: "127.0.0.1:0",
		Providers: map[string]configdomain.Provider{
			"zhipu": {Provider: "zhipu", OpenAIBaseURL: "http://127.0.0.1:1", Models: []string{"m"}},
		},
		MCP:       servers,
		MCPRoutes: routes,
	}
	p := newTestProxy(t, cfg)
	logDir := t.TempDir()
	p.initRequestLog(configdomain.RequestLogConfig{Enabled: true, Dir: logDir})
	p.reqLogStarted = p.lifecycle.Run(func(<-chan struct{}) { p.reqLog.Run() })
	srv := httptest.NewServer(http.HandlerFunc(p.Handler))
	t.Cleanup(srv.Close)

	resp := mcpPost(t, srv.URL+"/mcp/web-search", "", routeInitBody)
	sid := resp.Header.Get("Mcp-Session-Id")
	resp.Body.Close()
	mcpPost(t, srv.URL+"/mcp/web-search", sid, routeListBody).Body.Close()
	mcpPost(t, srv.URL+"/mcp/web-search", sid, routeCallBody("web_search")).Body.Close()

	deadline := time.Now().Add(5 * time.Second)
	var content string
	for {
		entries, _ := os.ReadDir(logDir)
		var b strings.Builder
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".log") {
				data, _ := os.ReadFile(filepath.Join(logDir, e.Name()))
				b.Write(data)
			}
		}
		content = b.String()
		if strings.Count(content, `"kind":"mcp"`) >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("route records never landed:\n%s", content)
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, want := range []string{`"kind":"mcp"`, `"path":"/mcp/web-search"`, `"method":"initialize"`, `"method":"tools/list"`, `"method":"tools/call"`, `"provider":"zs"`} {
		if !strings.Contains(content, want) {
			t.Errorf("route log missing %s\n%s", want, content)
		}
	}
}

// TestMCPRoute_QuotaExhaustedSkipsBackend: a provider whose shared MCP-tool
// window is exhausted (zhipu TIME_LIMIT) is skipped pre-emptively — the call
// never reaches it and fails over to the next target.
func TestMCPRoute_QuotaExhaustedSkipsBackend(t *testing.T) {
	zs, exa, servers, routes := twoRouteBackends(t)
	p, srv := newMCPRouteTestProxyP(t, []string{"k-A"}, servers, routes)
	// Inject an exhausted MCP-tool window for zhipu (generation 1).
	p.runtimeState.SetQuota("zhipu", &provider.QuotaSnapshot{
		Billing: provider.BillingPlan,
		Windows: []provider.QuotaWindow{
			{Label: "Daily time", Kind: "time", DetailLabel: "By MCP tool", RemainingPct: 0},
		},
	}, 1)

	resp := mcpPost(t, srv.URL+"/mcp/web-search", "", routeInitBody)
	sid := resp.Header.Get("Mcp-Session-Id")
	resp.Body.Close()
	resp = mcpPost(t, srv.URL+"/mcp/web-search", sid, routeCallBody("web_search"))
	body := mustRead(resp)
	if !strings.Contains(body, "served:web_search_exa") {
		t.Fatalf("quota-exhausted call = %s", body)
	}
	if zs.calls() != 0 {
		t.Fatalf("exhausted backend received a call: zs=%d", zs.calls())
	}
	if exa.calls() != 1 {
		t.Fatalf("fallback not used: exa=%d", exa.calls())
	}
}

// TestMCPRoute_AllCandidatesQuotaExhausted: when every backend declaring a
// tool is quota-skipped, the answer must say quota-exhausted (-32000), not
// misreport -32602 "unknown tool". An undeclared tool still gets -32602.
func TestMCPRoute_AllCandidatesQuotaExhausted(t *testing.T) {
	zs, _, servers, routes := twoRouteBackends(t)
	// Single-target route: the zhipu backend alone declares web_search.
	routes = map[string]configdomain.MCPRoute{
		"web-search": {Targets: []configdomain.MCPRouteTarget{
			{Server: "zs", Tools: map[string]string{"web_search": "web_search_prime"}},
		}},
	}
	p, srv := newMCPRouteTestProxyP(t, []string{"k-A"}, servers, routes)
	p.runtimeState.SetQuota("zhipu", &provider.QuotaSnapshot{
		Billing: provider.BillingPlan,
		Windows: []provider.QuotaWindow{
			{Label: "Daily time", Kind: "time", DetailLabel: "By MCP tool", RemainingPct: 0},
		},
	}, 1)

	resp := mcpPost(t, srv.URL+"/mcp/web-search", "", routeInitBody)
	sid := resp.Header.Get("Mcp-Session-Id")
	resp.Body.Close()

	resp = mcpPost(t, srv.URL+"/mcp/web-search", sid, routeCallBody("web_search"))
	body := mustRead(resp)
	if !strings.Contains(body, "quota-exhausted") || strings.Contains(body, "unknown tool") {
		t.Fatalf("quota-exhausted call misreported: %s", body)
	}
	if zs.calls() != 0 {
		t.Fatalf("exhausted backend received a call: %d", zs.calls())
	}

	// A tool nobody declares is still unknown (-32602).
	resp = mcpPost(t, srv.URL+"/mcp/web-search", sid, routeCallBody("nope"))
	body = mustRead(resp)
	if !strings.Contains(body, `-32602`) || !strings.Contains(body, "unknown tool") {
		t.Fatalf("undeclared tool = %s, want -32602 unknown tool", body)
	}
}

// TestMCPRoute_ToolsCallStatsCreditBackend: a successful routed tools/call
// increments the route's own stats entry and ALSO credits the chosen backend
// server, so the in-memory MCP stats reflect both the exposed name and the
// real target.
func TestMCPRoute_ToolsCallStatsCreditBackend(t *testing.T) {
	zs, _, servers, routes := twoRouteBackends(t)
	zs.callDelay = 10 * time.Millisecond
	p, srv := newMCPRouteTestProxyP(t, []string{"k-A"}, servers, routes)

	resp := mcpPost(t, srv.URL+"/mcp/web-search", "", routeInitBody)
	sid := resp.Header.Get("Mcp-Session-Id")
	resp.Body.Close()
	mcpPost(t, srv.URL+"/mcp/web-search", sid, routeListBody).Body.Close()

	before := p.mcpStats.Snapshot()
	mcpPost(t, srv.URL+"/mcp/web-search", sid, routeCallBody("web_search")).Body.Close()
	after := p.mcpStats.Snapshot()

	routeBefore := before["web-search"]
	routeAfter := after["web-search"]
	if routeAfter.Calls != routeBefore.Calls+1 {
		t.Fatalf("route calls %d -> %d, want +1", routeBefore.Calls, routeAfter.Calls)
	}
	if routeAfter.Errors != routeBefore.Errors {
		t.Fatalf("route errors changed: %d -> %d", routeBefore.Errors, routeAfter.Errors)
	}

	zsStat := after["zs"]
	if zsStat.Calls != 1 {
		t.Fatalf("backend 'zs' calls = %d, want 1", zsStat.Calls)
	}
	if zsStat.Errors != 0 {
		t.Fatalf("backend 'zs' errors = %d, want 0", zsStat.Errors)
	}
	if zsStat.AvgLatencyMs == 0 {
		t.Fatalf("backend 'zs' avg_latency_ms unset")
	}

	if after["exa"].Calls != 0 {
		t.Fatalf("unused backend 'exa' calls = %d, want 0", after["exa"].Calls)
	}
}
