package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/provider"
)

// freeze_test.go pins the operator-freeze contract on the MCP gateway:
// freeze (CLI `freeze` / POST /api/health/freeze) excludes an account from
// every NEW selection — sessionless picks, 401 rotation, route sub-sessions,
// probes — and additionally breaks established bindings (pinned sessions and
// route sub-sessions rebind to a healthy sibling), mirroring the LLM
// scheduling surface where a frozen provider is never a target.

// poolVirtuals returns the zhipu pool's virtual account ids in snapshot
// order (virtual ids are key hashes, not the stored account ids).
func poolVirtuals(t *testing.T, p *Proxy) []string {
	t.Helper()
	ids := p.SnapshotRuntime().PoolIndex["zhipu"]
	if len(ids) != 2 {
		t.Fatalf("pool virtuals = %v, want two accounts", ids)
	}
	return ids
}

// poolKeyOf resolves one virtual account's raw credential (the same
// KeyReporter seam the gateway uses for custom auth headers).
func poolKeyOf(t *testing.T, p *Proxy, virtual string) string {
	t.Helper()
	prov, ok := p.SnapshotRuntime().Providers[virtual]
	if !ok {
		t.Fatalf("virtual %s has no provider impl", virtual)
	}
	kr, ok := prov.(provider.KeyReporter)
	if !ok {
		t.Fatalf("virtual %s cannot report its key", virtual)
	}
	key, err := kr.ReportKey()
	if err != nil {
		t.Fatalf("report key: %v", err)
	}
	return key
}

// freezeMCPAccount freezes one pool virtual account.
func freezeMCPAccount(t *testing.T, p *Proxy, virtual string) {
	t.Helper()
	parentOf := map[string]string{}
	for _, id := range poolVirtuals(t, p) {
		parentOf[id] = "zhipu"
	}
	if got := p.runtimeState.FreezeHealth(virtual, parentOf, poolVirtuals(t, p)); len(got) != 1 {
		t.Fatalf("freeze %s matched %v, want exactly it", virtual, got)
	}
}

// freezeAccountForKey freezes the pool virtual holding the given raw key
// ("" = none matches).
func freezeAccountForKey(t *testing.T, p *Proxy, key string) {
	t.Helper()
	for _, id := range poolVirtuals(t, p) {
		if poolKeyOf(t, p, id) == key {
			freezeMCPAccount(t, p, id)
			return
		}
	}
	t.Fatalf("no pool account holds key %q", key)
}

// newFreezeTestProxy builds a two-account zhipu pool plus the given mcp:
// servers, returning the Proxy (for freeze injection) and its test server.
func newFreezeTestProxy(t *testing.T, servers map[string]configdomain.MCPServer) (*Proxy, *httptest.Server) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeMCPKeys(t, "zhipu", "k-A", "k-B") // pool: #aaa (k-A), #bbb (k-B)
	cfg := &configdomain.Config{
		Listen: "127.0.0.1:0",
		Providers: map[string]configdomain.Provider{
			"zhipu": {Provider: "zhipu", OpenAIBaseURL: "http://127.0.0.1:1", Models: []string{"m"}},
		},
		MCP: servers,
	}
	p := newTestProxy(t, cfg)
	srv := httptest.NewServer(http.HandlerFunc(p.Handler))
	t.Cleanup(srv.Close)
	return p, srv
}

// TestMCPGateway_FrozenAccountExcludedFromSessionlessPicks: with one of two
// pool accounts frozen, every sessionless call is served by the healthy
// sibling — freeze must not leak picks (round-robin or 401 rotation) onto the
// frozen credential.
func TestMCPGateway_FrozenAccountExcludedFromSessionlessPicks(t *testing.T) {
	up := &fakeMCPUpstream{} // stateless: no local session, every call is a fresh pick
	upSrv := httptest.NewServer(http.HandlerFunc(up.serve))
	t.Cleanup(upSrv.Close)

	p, srv := newFreezeTestProxy(t, map[string]configdomain.MCPServer{
		"fc": {Provider: "zhipu", URL: upSrv.URL},
	})
	virtuals := poolVirtuals(t, p)
	healthyKey := poolKeyOf(t, p, virtuals[1])
	freezeMCPAccount(t, p, virtuals[0])

	for i := 0; i < 4; i++ {
		resp := mcpPost(t, srv.URL+"/mcp/fc", "", mcpCallBody)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("call %d = %d", i, resp.StatusCode)
		}
		resp.Body.Close()
	}
	for i, a := range up.auths() {
		if a != "Bearer "+healthyKey {
			t.Fatalf("hit %d auth = %q, want healthy sibling key only", i, a)
		}
	}
}

// TestMCPGateway_AllAccountsFrozenAnswersDistinct503: an empty pool and a
// fully-frozen pool are different failures; the latter must name freeze, not
// tell the operator to log in.
func TestMCPGateway_AllAccountsFrozenAnswersDistinct503(t *testing.T) {
	up := &fakeMCPUpstream{}
	upSrv := httptest.NewServer(http.HandlerFunc(up.serve))
	t.Cleanup(upSrv.Close)

	p, srv := newFreezeTestProxy(t, map[string]configdomain.MCPServer{
		"fc": {Provider: "zhipu", URL: upSrv.URL},
	})
	for _, id := range poolVirtuals(t, p) {
		freezeMCPAccount(t, p, id)
	}

	resp := mcpPost(t, srv.URL+"/mcp/fc", "", mcpCallBody)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	body, _ := readBodyString(resp)
	if !strings.Contains(body, "frozen") || strings.Contains(body, "login") {
		t.Fatalf("body = %q, want the frozen/cooldown message without the login hint", body)
	}
	if n := len(up.auths()); n != 0 {
		t.Fatalf("frozen pool still reached upstream %d times", n)
	}
}

// TestMCPGateway_FrozenSessionAccountFailsClosed410: a session pinned to a
// later-frozen account is dropped with 410 (the client re-initializes onto a
// healthy account); the same session id afterwards is simply sessionless.
func TestMCPGateway_FrozenSessionAccountFailsClosed410(t *testing.T) {
	up := &fakeMCPUpstream{sessionID: "up-1"}
	upSrv := httptest.NewServer(http.HandlerFunc(up.serve))
	t.Cleanup(upSrv.Close)

	p, srv := newFreezeTestProxy(t, map[string]configdomain.MCPServer{
		"zs": {Provider: "zhipu", URL: upSrv.URL},
	})

	resp := mcpPost(t, srv.URL+"/mcp/zs", "", mcpInitBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize = %d", resp.StatusCode)
	}
	localSID := resp.Header.Get("Mcp-Session-Id")
	resp.Body.Close()
	if localSID == "" {
		t.Fatal("no local session minted")
	}
	bound := up.auths()[0] // Bearer <key> of whichever account won the pick
	freezeAccountForKey(t, p, strings.TrimPrefix(bound, "Bearer "))

	resp = mcpPost(t, srv.URL+"/mcp/zs", localSID, mcpCallBody)
	body, _ := readBodyString(resp)
	if resp.StatusCode != http.StatusGone || !strings.Contains(body, "frozen") {
		t.Fatalf("pinned call = %d %q, want 410 naming the freeze", resp.StatusCode, body)
	}

	// The binding is gone: the same id now takes the sessionless path and is
	// served by the surviving account instead of failing closed again.
	resp = mcpPost(t, srv.URL+"/mcp/zs", localSID, mcpCallBody)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post-drop call = %d %q, want sessionless service on the healthy account", resp.StatusCode, mustRead(resp))
	}
	last := up.auths()[len(up.auths())-1]
	if last == bound {
		t.Fatalf("post-drop call reused the frozen credential %q", last)
	}
}

// TestMCPRoute_FrozenSubSessionRebindsHealthySibling: a route sub-session
// bound to a later-frozen account is dropped and rebuilt on the healthy
// sibling — the call still succeeds instead of hammering the frozen account.
func TestMCPRoute_FrozenSubSessionRebindsHealthySibling(t *testing.T) {
	zs, _, servers, routes := twoRouteBackends(t)
	// Single provider-backed target: the rebuild must stay within the pool.
	routes["web-search"] = configdomain.MCPRoute{Targets: []configdomain.MCPRouteTarget{
		{Server: "zs", Tools: map[string]string{"web_search": "web_search_prime"}},
	}}
	p, srv := newMCPRouteTestProxyP(t, []string{"k-A", "k-B"}, servers, routes)

	resp := mcpPost(t, srv.URL+"/mcp/web-search", "", routeInitBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize = %d", resp.StatusCode)
	}
	sid := resp.Header.Get("Mcp-Session-Id")
	resp.Body.Close()

	resp = mcpPost(t, srv.URL+"/mcp/web-search", sid, routeCallBody("web_search"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first call = %d", resp.StatusCode)
	}
	resp.Body.Close()
	auths := func() []string {
		zs.mu.Lock()
		defer zs.mu.Unlock()
		var out []string
		for _, h := range zs.hits {
			if h.method == "tools/call" && h.auth != "" {
				out = append(out, h.auth)
			}
		}
		return out
	}
	bound := auths()[0]
	freezeAccountForKey(t, p, strings.TrimPrefix(bound, "Bearer "))

	resp = mcpPost(t, srv.URL+"/mcp/web-search", sid, routeCallBody("web_search"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post-freeze call = %d %q, want success on the healthy sibling", resp.StatusCode, mustRead(resp))
	}
	got := auths()
	if got[len(got)-1] == bound {
		t.Fatalf("post-freeze call reused frozen credential %q", bound)
	}
}

// TestMCPRoute_AllAccountsFrozenIsNotUnknownTool: when every backend
// declaring the tool has no schedulable account, the route answers a -32000
// server error naming freeze — never -32602 "unknown tool" (mirrors the
// quota-exhausted distinction).
func TestMCPRoute_AllAccountsFrozenIsNotUnknownTool(t *testing.T) {
	_, _, servers, routes := twoRouteBackends(t)
	routes["web-search"] = configdomain.MCPRoute{Targets: []configdomain.MCPRouteTarget{
		{Server: "zs", Tools: map[string]string{"web_search": "web_search_prime"}},
	}}
	p, srv := newMCPRouteTestProxyP(t, []string{"k-A", "k-B"}, servers, routes)
	for _, id := range poolVirtuals(t, p) {
		freezeMCPAccount(t, p, id)
	}

	resp := mcpPost(t, srv.URL+"/mcp/web-search", "", routeInitBody)
	sid := resp.Header.Get("Mcp-Session-Id")
	resp.Body.Close()
	resp = mcpPost(t, srv.URL+"/mcp/web-search", sid, routeCallBody("web_search"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 carrying the JSON-RPC error", resp.StatusCode)
	}
	var rpc struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(mustRead(resp)), &rpc); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if rpc.Error.Code != -32000 || !strings.Contains(rpc.Error.Message, "frozen") {
		t.Fatalf("error = %d %q, want -32000 naming the freeze", rpc.Error.Code, rpc.Error.Message)
	}
}

// TestMCPProbe_FrozenPoolNamesFreeze: the probe surface reports the
// unschedulable pool as a freeze condition, not as a missing login.
func TestMCPProbe_FrozenPoolNamesFreeze(t *testing.T) {
	up := &fakeMCPUpstream{}
	upSrv := httptest.NewServer(http.HandlerFunc(up.serve))
	t.Cleanup(upSrv.Close)

	p, _ := newFreezeTestProxy(t, map[string]configdomain.MCPServer{
		"zs": {Provider: "zhipu", URL: upSrv.URL},
	})
	for _, id := range poolVirtuals(t, p) {
		freezeMCPAccount(t, p, id)
	}

	res, err := p.probeMCP(context.Background(), "zs")
	if err != nil {
		t.Fatalf("probeMCP: %v", err)
	}
	if res.OK || !strings.Contains(res.Error, "frozen") {
		t.Fatalf("probe = ok:%v err:%q, want the freeze message", res.OK, res.Error)
	}
}
