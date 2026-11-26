package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	configdomain "model-proxy/internal/config"
	obscounters "model-proxy/internal/observe/counters"
)

// r6_mcp_terminal_log_test.go pins the terminal-exchange logging contract of
// the MCP gateway (R6 B5): every exchange that entered the MCP protocol layer
// — on the pinned, route and stdio faces alike — is recorded by mcpLog
// (request log + MCP stats) with the HTTP status the client got. HTTP-layer
// precheck rejections (405 / 413 / guard 400) and admission failures (503: no
// schedulable account, the stdio per-server child cap) are never recorded.

// r6AnyProvider stands for "whichever pool account served the exchange" in an
// expected record — a virtual id the test does not predict.
const r6AnyProvider = "\x00any"

// r6LogRecord is the request-log projection the tests compare.
type r6LogRecord struct {
	Kind     string `json:"kind"`
	Method   string `json:"method"`
	Status   int    `json:"status"`
	Path     string `json:"path"`
	Provider string `json:"provider"`
	Tool     string `json:"tool"`
}

// r6Term is one expected record of /mcp/<name> (kind "mcp").
type r6Term struct {
	method   string
	status   int
	tool     string
	provider string // "" = no backend account on the record
}

func r6Records(name string, want []r6Term) []r6LogRecord {
	out := make([]r6LogRecord, len(want))
	for i, w := range want {
		out[i] = r6LogRecord{Kind: "mcp", Method: w.method, Status: w.status, Path: "/mcp/" + name, Provider: w.provider, Tool: w.tool}
	}
	return out
}

// r6SingleTargetRoute aggregates the zhipu-backed "zs" backend alone.
func r6SingleTargetRoute() map[string]configdomain.MCPRoute {
	return map[string]configdomain.MCPRoute{"web-search": {Targets: []configdomain.MCPRouteTarget{
		{Server: "zs", Tools: map[string]string{"web_search": "web_search_prime"}},
	}}}
}

// r6Logged builds a test Proxy with the request log enabled into a temp dir,
// so every terminal exchange a test drives can be read back from the JSONL
// stream. tune may set extra config on the way in.
//
// The process log is muted before the proxy is built: enabling the request
// log announces itself, and a reload re-syncs quotas against the fixture's
// unusable upstream. Cleanups run LIFO, so the logger is restored only after
// the proxy's Close — which waits for that re-sync — has logged.
func r6Logged(t *testing.T, keys []string, servers map[string]configdomain.MCPServer, routes map[string]configdomain.MCPRoute, tune func(*configdomain.Config)) (*Proxy, *httptest.Server) {
	t.Helper()
	silenceLog(t)
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
	if tune != nil {
		tune(cfg)
	}
	p := newTestProxy(t, cfg)
	p.initRequestLog(configdomain.RequestLogConfig{Enabled: true, Dir: t.TempDir()})
	p.reqLogStarted = p.lifecycle.Run(func(<-chan struct{}) { p.reqLog.Run() })
	srv := httptest.NewServer(http.HandlerFunc(p.Handler))
	t.Cleanup(srv.Close)
	return p, srv
}

// r6Send issues one MCP request and consumes the whole response body. A
// response is only complete once its handler returned, and a handler records
// its terminal before returning — so the record exists when r6Send comes back.
func r6Send(t *testing.T, url, method, session, body string) (int, string, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, mustRead(resp), resp.Header.Get("Mcp-Session-Id")
}

// r6DrainLog shuts the request log down — it drains every accepted record and
// takes none afterwards — and returns what was written, in order.
func r6DrainLog(t *testing.T, p *Proxy) []r6LogRecord {
	t.Helper()
	p.reqLog.Shutdown()
	var out []r6LogRecord
	for _, line := range strings.Split(readAllLogFiles(t, p.reqLog.Directory()), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec r6LogRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("request-log line is not JSON: %v", err)
		}
		out = append(out, rec)
	}
	return out
}

// r6Show renders records as "path method status [tool]" for failure messages.
func r6Show(records []r6LogRecord) string {
	parts := make([]string, len(records))
	for i, r := range records {
		parts[i] = r.Path + " " + r.Method + " " + strconv.Itoa(r.Status)
		if r.Tool != "" {
			parts[i] += " " + r.Tool
		}
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// r6RequireLog fails unless the drained request log holds exactly want, in
// order — every record kind "mcp" — and nothing else.
func r6RequireLog(t *testing.T, p *Proxy, want []r6LogRecord) {
	t.Helper()
	got := r6DrainLog(t, p)
	if len(got) != len(want) {
		t.Fatalf("request log holds %d records %s, want exactly %d %s", len(got), r6Show(got), len(want), r6Show(want))
	}
	for i, w := range want {
		rec := got[i]
		if rec.Kind != "mcp" || rec.Path != w.Path || rec.Method != w.Method || rec.Status != w.Status || rec.Tool != w.Tool {
			t.Fatalf("record %d = kind %q %s %s %d tool %q, want kind \"mcp\" %s %s %d tool %q",
				i, rec.Kind, rec.Path, rec.Method, rec.Status, rec.Tool, w.Path, w.Method, w.Status, w.Tool)
		}
		if providerOK := rec.Provider == w.Provider || (w.Provider == r6AnyProvider && rec.Provider != ""); !providerOK {
			t.Fatalf("record %d (%s %s %d) provider = %q, want %q", i, rec.Path, rec.Method, rec.Status, rec.Provider, w.Provider)
		}
	}
}

// r6RequireStats fails unless the name's MCP stats counters are exactly
// (calls, errors) — errors being the recorded HTTP statuses >= 400.
func r6RequireStats(t *testing.T, p *Proxy, name string, calls, errs uint64) {
	t.Helper()
	got, ok := p.mcpStats.Snapshot()[name]
	if !ok || got.Calls != calls || got.Errors != errs {
		t.Fatalf("MCPStats[%q] = %+v (present %v), want calls=%d errors=%d", name, got, ok, calls, errs)
	}
}

// r6RequireToolCalls fails unless exactly calls tools/call exchanges were
// counted under (name, tool) in the per-tool stats.
func r6RequireToolCalls(t *testing.T, p *Proxy, name, tool string, calls uint64) {
	t.Helper()
	if got := p.mcpStats.RawToolSnapshot()[obscounters.MCPToolKey{Name: name, Tool: tool}]; got.Calls != calls {
		t.Fatalf("MCPStats tool (%q, %q) calls = %d, want %d", name, tool, got.Calls, calls)
	}
}

// TestR6MCPRoute_EveryProtocolTerminalLogged drives one of each terminal the
// route face synthesizes and reads the request log back: every exchange that
// entered the MCP protocol layer is recorded, with the HTTP status the client
// got (a JSON-RPC error sent over 200 is a 200).
func TestR6MCPRoute_EveryProtocolTerminalLogged(t *testing.T) {
	_, _, servers, _ := twoRouteBackends(t)
	p, srv := r6Logged(t, []string{"k-A", "k-B"}, servers, r6SingleTargetRoute(), nil)
	url := srv.URL + "/mcp/web-search"
	post := func(what, session, body string, want int) (int, string, string) {
		t.Helper()
		status, resp, sid := r6Send(t, url, http.MethodPost, session, body)
		if status != want {
			t.Fatalf("%s = %d %q, want %d", what, status, resp, want)
		}
		return status, resp, sid
	}

	post("call without a session", "", routeCallBody("web_search"), http.StatusBadRequest)
	post("call on an unknown session", "bogus-sid", routeCallBody("web_search"), http.StatusNotFound)
	_, _, sid := post("initialize", "", routeInitBody, http.StatusOK)
	if sid == "" {
		t.Fatal("no route session minted")
	}
	post("ping", sid, `{"jsonrpc":"2.0","id":4,"method":"ping"}`, http.StatusOK)
	post("notification", sid, mcpNotifBody, http.StatusAccepted)
	if _, body, _ := post("unknown method", sid, `{"jsonrpc":"2.0","id":5,"method":"resources/list","params":{}}`, http.StatusOK); !strings.Contains(body, "-32601") {
		t.Fatalf("unknown method = %s, want the -32601 JSON-RPC error", body)
	}
	post("call of an unknown tool", sid, routeCallBody("nonexistent"), http.StatusOK)
	post("call without params.name", sid, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{}}`, http.StatusOK)
	post("tools/list", sid, routeListBody, http.StatusOK)
	// The declaring backend has no schedulable account left: the call is
	// answered with the -32000 JSON-RPC error over 200.
	for _, id := range poolVirtuals(t, p) {
		freezeMCPAccount(t, p, id)
	}
	if _, body, _ := post("call with every account frozen", sid, routeCallBody("web_search"), http.StatusOK); !strings.Contains(body, "frozen") {
		t.Fatalf("call with every account frozen = %s, want the -32000 frozen/cooldown answer", body)
	}
	if status, body, _ := r6Send(t, url, http.MethodDelete, sid, ""); status != http.StatusOK {
		t.Fatalf("DELETE = %d %q, want 200", status, body)
	}

	r6RequireLog(t, p, r6Records("web-search", []r6Term{
		{method: "tools/call", status: http.StatusBadRequest, tool: "web_search"},
		{method: "tools/call", status: http.StatusNotFound, tool: "web_search"},
		{method: "initialize", status: http.StatusOK},
		{method: "ping", status: http.StatusOK},
		{method: "notifications/initialized", status: http.StatusAccepted},
		{method: "resources/list", status: http.StatusOK},
		{method: "tools/call", status: http.StatusOK, tool: "nonexistent"},
		{method: "tools/call", status: http.StatusOK},
		{method: "tools/list", status: http.StatusOK},
		{method: "tools/call", status: http.StatusOK, tool: "web_search"},
		{method: http.MethodDelete, status: http.StatusOK},
	}))
	r6RequireStats(t, p, "web-search", 11, 2)
	r6RequireToolCalls(t, p, "web-search", "web_search", 3)
	r6RequireToolCalls(t, p, "web-search", "nonexistent", 1)
}

// TestR6MCPStdio_EveryProtocolTerminalLogged is the stdio twin: the
// session-required 400, unknown-session 404, notification 202 and DELETE 200
// are recorded like the initialize and the relayed calls already were.
func TestR6MCPStdio_EveryProtocolTerminalLogged(t *testing.T) {
	t.Setenv("MCP_STDIO_R6_HELPER", "1")
	p, srv := r6Logged(t, []string{"k-A", "k-B"}, map[string]configdomain.MCPServer{"vision": r6StdioServer()}, nil, nil)
	url := srv.URL + "/mcp/vision"
	post := func(what, method, session, body string, want int) (int, string, string) {
		t.Helper()
		status, resp, sid := r6Send(t, url, method, session, body)
		if status != want {
			t.Fatalf("%s = %d %q, want %d", what, status, resp, want)
		}
		return status, resp, sid
	}

	post("call without a session", http.MethodPost, "", routeCallBody("image_analysis"), http.StatusBadRequest)
	post("call on an unknown session", http.MethodPost, "bogus-sid", routeCallBody("image_analysis"), http.StatusNotFound)
	_, _, sid := post("initialize", http.MethodPost, "", mcpInitBody, http.StatusOK)
	if sid == "" {
		t.Fatal("no session minted")
	}
	if _, body, _ := post("relayed call", http.MethodPost, sid, routeCallBody("image_analysis"), http.StatusOK); !strings.Contains(body, `"result"`) {
		t.Fatalf("relayed call = %s, want the child's result", body)
	}
	post("ping", http.MethodPost, sid, `{"jsonrpc":"2.0","id":4,"method":"ping"}`, http.StatusOK)
	post("notification", http.MethodPost, sid, mcpNotifBody, http.StatusAccepted)
	post("unknown method relayed to the child", http.MethodPost, sid, `{"jsonrpc":"2.0","id":5,"method":"resources/list","params":{}}`, http.StatusOK)
	if _, body, _ := post("call answered with a JSON-RPC error", http.MethodPost, sid, routeCallBody("boom"), http.StatusOK); !strings.Contains(body, "-32602") {
		t.Fatalf("call of \"boom\" = %s, want the child's -32602", body)
	}
	post("DELETE", http.MethodDelete, sid, "", http.StatusOK)
	// No schedulable account is an HTTP-layer precheck: nothing is recorded.
	for _, id := range poolVirtuals(t, p) {
		freezeMCPAccount(t, p, id)
	}
	post("initialize with every account frozen", http.MethodPost, "", mcpInitBody, http.StatusServiceUnavailable)

	r6RequireLog(t, p, r6Records("vision", []r6Term{
		{method: "tools/call", status: http.StatusBadRequest, tool: "image_analysis"},
		{method: "tools/call", status: http.StatusNotFound, tool: "image_analysis"},
		{method: "initialize", status: http.StatusOK, provider: r6AnyProvider},
		{method: "tools/call", status: http.StatusOK, tool: "image_analysis"},
		{method: "ping", status: http.StatusOK},
		{method: "notifications/initialized", status: http.StatusAccepted},
		{method: "resources/list", status: http.StatusOK},
		{method: "tools/call", status: http.StatusOK, tool: "boom"},
		{method: http.MethodDelete, status: http.StatusOK},
	}))
	r6RequireStats(t, p, "vision", 9, 2)
	// Every tools/call record carries the client's tool name — the rejected
	// ones (400 / 404) included.
	r6RequireToolCalls(t, p, "vision", "image_analysis", 3)
	r6RequireToolCalls(t, p, "vision", "boom", 1)
}

// TestR6MCP_PrecheckRejectionsNotLogged: an HTTP-layer precheck rejection —
// method 405, body over the cap 413, outbound guard 400, no schedulable
// account 503 — never reaches the MCP protocol layer and leaves no record in
// the request log or the stats, on the route and the stdio face alike.
func TestR6MCP_PrecheckRejectionsNotLogged(t *testing.T) {
	t.Setenv("MCP_STDIO_R6_HELPER", "1")
	_, _, servers, _ := twoRouteBackends(t)
	stdio := map[string]configdomain.MCPServer{"vision": r6StdioServer()}
	oversized := `{"jsonrpc":"2.0","id":2,"method":"ping","params":{"pad":"` + strings.Repeat("x", 64) + `"}}`
	secret := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search","arguments":{"q":"` + guardFixtureKey + `"}}}`
	tinyCap := func(cfg *configdomain.Config) { cfg.MaxRequestBodyBytes = 32 }
	guardBlock := func(cfg *configdomain.Config) { cfg.Guard = configdomain.GuardConfig{MCPSecrets: "block"} }
	postTo := func(path, body string) func(*testing.T, string) int {
		return func(t *testing.T, base string) int {
			status, _, _ := r6Send(t, base+path, http.MethodPost, "", body)
			return status
		}
	}
	get := func(path string) func(*testing.T, string) int {
		return func(t *testing.T, base string) int {
			status, _, _ := r6Send(t, base+path, http.MethodGet, "", "")
			return status
		}
	}
	cases := []struct {
		name    string
		servers map[string]configdomain.MCPServer
		tune    func(*configdomain.Config)
		freeze  bool // no schedulable account before the request
		send    func(*testing.T, string) int
		want    int
	}{
		{"route: method not allowed", servers, nil, false, get("/mcp/web-search"), http.StatusMethodNotAllowed},
		{"route: body over the cap", servers, tinyCap, false, postTo("/mcp/web-search", oversized), http.StatusRequestEntityTooLarge},
		{"route: body blocked by the outbound guard", servers, guardBlock, false, postTo("/mcp/web-search", secret), http.StatusBadRequest},
		{"stdio: method not allowed", stdio, nil, false, get("/mcp/vision"), http.StatusMethodNotAllowed},
		{"stdio: body over the cap", stdio, tinyCap, false, postTo("/mcp/vision", oversized), http.StatusRequestEntityTooLarge},
		{"stdio: body blocked by the outbound guard", stdio, guardBlock, false, postTo("/mcp/vision", secret), http.StatusBadRequest},
		{"stdio: no schedulable account", stdio, nil, true, postTo("/mcp/vision", mcpInitBody), http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, srv := r6Logged(t, []string{"k-A", "k-B"}, tc.servers, r6SingleTargetRoute(), tc.tune)
			if tc.freeze {
				for _, id := range poolVirtuals(t, p) {
					freezeMCPAccount(t, p, id)
				}
			}
			if got := tc.send(t, srv.URL); got != tc.want {
				t.Fatalf("status = %d, want %d", got, tc.want)
			}
			if got := r6DrainLog(t, p); len(got) != 0 {
				t.Fatalf("a precheck rejection was recorded: %s", r6Show(got))
			}
			if got := p.mcpStats.Snapshot(); len(got) != 0 {
				t.Fatalf("a precheck rejection was counted: %+v", got)
			}
		})
	}
}

// TestR6MCPGateway_PinnedSessionAccount410Logged: the pinned face records its
// protocol-layer 410 — a session whose account an operator froze — and its
// foreign-session 404, like every other terminal.
func TestR6MCPGateway_PinnedSessionAccount410Logged(t *testing.T) {
	up := &fakeMCPUpstream{sessionID: "up-1"}
	upSrv := httptest.NewServer(http.HandlerFunc(up.serve))
	t.Cleanup(upSrv.Close)
	servers := map[string]configdomain.MCPServer{
		"zs":    {Provider: "zhipu", URL: upSrv.URL},
		"other": {Provider: "zhipu", URL: upSrv.URL},
	}
	p, srv := r6Logged(t, []string{"k-A", "k-B"}, servers, nil, nil)
	zsURL, otherURL := srv.URL+"/mcp/zs", srv.URL+"/mcp/other"

	status, body, sid := r6Send(t, zsURL, http.MethodPost, "", mcpInitBody)
	if status != http.StatusOK || sid == "" {
		t.Fatalf("initialize = %d %q", status, body)
	}
	if status, body, _ = r6Send(t, zsURL, http.MethodPost, sid, mcpCallBody); status != http.StatusOK {
		t.Fatalf("call = %d %q", status, body)
	}
	bound := up.auths()[len(up.auths())-1] // Bearer <key> of the session's account
	// zs's session id against another server is an unknown session — a
	// protocol-layer terminal, recorded under the server that answered.
	if status, body, _ = r6Send(t, otherURL, http.MethodPost, sid, mcpCallBody); status != http.StatusNotFound || !strings.Contains(body, "unknown session") {
		t.Fatalf("foreign session = %d %q, want 404 unknown session", status, body)
	}
	freezeAccountForKey(t, p, strings.TrimPrefix(bound, "Bearer "))
	if status, body, _ = r6Send(t, zsURL, http.MethodPost, sid, mcpCallBody); status != http.StatusGone || strings.TrimSpace(body) != mcpSessionAccountFrozenMsg {
		t.Fatalf("call after the freeze = %d %q, want 410 %q", status, body, mcpSessionAccountFrozenMsg)
	}

	var want []r6LogRecord
	want = append(want, r6Records("zs", []r6Term{
		{method: "initialize", status: http.StatusOK, provider: r6AnyProvider},
		{method: "tools/call", status: http.StatusOK, tool: "search", provider: r6AnyProvider},
	})...)
	want = append(want, r6Records("other", []r6Term{{method: "tools/call", status: http.StatusNotFound, tool: "search"}})...)
	want = append(want, r6Records("zs", []r6Term{{method: "tools/call", status: http.StatusGone, tool: "search", provider: r6AnyProvider}})...)
	r6RequireLog(t, p, want)
	r6RequireStats(t, p, "zs", 3, 1)
	r6RequireStats(t, p, "other", 1, 1)
}

// TestR6MCPGateway_PinnedSessionAccountRemoved410Logged: the pinned 410 for a
// session whose account a reload removed is recorded too; the sessionless
// call that follows it is an ordinary 200.
func TestR6MCPGateway_PinnedSessionAccountRemoved410Logged(t *testing.T) {
	up := &fakeMCPUpstream{sessionID: "up-1"}
	upSrv := httptest.NewServer(http.HandlerFunc(up.serve))
	t.Cleanup(upSrv.Close)
	p, srv := r6Logged(t, []string{"k-A", "k-B"}, map[string]configdomain.MCPServer{"zs": {Provider: "zhipu", URL: upSrv.URL}}, nil, nil)
	url := srv.URL + "/mcp/zs"

	status, body, sid := r6Send(t, url, http.MethodPost, "", mcpInitBody)
	if status != http.StatusOK || sid == "" {
		t.Fatalf("initialize = %d %q", status, body)
	}
	if status, body, _ = r6Send(t, url, http.MethodPost, sid, mcpCallBody); status != http.StatusOK {
		t.Fatalf("call = %d %q", status, body)
	}
	r6Reload(t, p, r6ConfigYAML(r6ZSYAML(upSrv.URL), ""), r6OtherKey(t, strings.TrimPrefix(up.auths()[0], "Bearer ")))
	if status, body, _ = r6Send(t, url, http.MethodPost, sid, mcpCallBody); status != http.StatusGone || strings.TrimSpace(body) != mcpSessionAccountGoneMsg {
		t.Fatalf("call after the reload = %d %q, want 410 %q", status, body, mcpSessionAccountGoneMsg)
	}
	if status, body, _ = r6Send(t, url, http.MethodPost, sid, mcpCallBody); status != http.StatusOK {
		t.Fatalf("sessionless call after the drop = %d %q, want 200", status, body)
	}

	r6RequireLog(t, p, r6Records("zs", []r6Term{
		{method: "initialize", status: http.StatusOK, provider: r6AnyProvider},
		{method: "tools/call", status: http.StatusOK, tool: "search", provider: r6AnyProvider},
		{method: "tools/call", status: http.StatusGone, tool: "search", provider: r6AnyProvider},
		{method: "tools/call", status: http.StatusOK, tool: "search", provider: r6AnyProvider},
	}))
	r6RequireStats(t, p, "zs", 4, 1)
}

// TestR6MCPGateway_StdioSessionAccount410Logged: the stdio 410s F2 added are
// recorded like the pinned ones, whichever way the account stopped being
// usable.
func TestR6MCPGateway_StdioSessionAccount410Logged(t *testing.T) {
	t.Setenv("MCP_STDIO_R6_HELPER", "1")
	t.Run("operator-frozen account", func(t *testing.T) {
		p, srv := r6Logged(t, []string{"k-A", "k-B"}, map[string]configdomain.MCPServer{"vision": r6StdioServer()}, nil, nil)
		sid, first := r6OpenSession(t, srv, r6PidProbe())
		freezeAccountForKey(t, p, first.Key)
		if status, body, _ := r6CallTools(t, srv, sid); status != http.StatusGone {
			t.Fatalf("call after the freeze = %d %q, want 410", status, body)
		}
		r6RequireLog(t, p, r6Records("vision", []r6Term{
			{method: "initialize", status: http.StatusOK, provider: r6AnyProvider},
			{method: "tools/call", status: http.StatusOK, tool: "image_analysis"},
			{method: "tools/call", status: http.StatusGone, tool: "image_analysis", provider: r6AnyProvider},
		}))
		r6RequireStats(t, p, "vision", 3, 1)
	})
	t.Run("account removed by reload", func(t *testing.T) {
		p, srv := r6Logged(t, []string{"k-A", "k-B"}, map[string]configdomain.MCPServer{"vision": r6StdioServer()}, nil, nil)
		sid, first := r6OpenSession(t, srv, r6PidProbe())
		r6Reload(t, p, r6ConfigYAML(r6VisionYAML(), ""), r6OtherKey(t, first.Key))
		if status, body, _ := r6CallTools(t, srv, sid); status != http.StatusGone {
			t.Fatalf("call after the reload = %d %q, want 410", status, body)
		}
		r6RequireLog(t, p, r6Records("vision", []r6Term{
			{method: "initialize", status: http.StatusOK, provider: r6AnyProvider},
			{method: "tools/call", status: http.StatusOK, tool: "image_analysis"},
			{method: "tools/call", status: http.StatusGone, tool: "image_analysis", provider: r6AnyProvider},
		}))
		r6RequireStats(t, p, "vision", 3, 1)
	})
}

// TestR6MCPGateway_StdioForeignSessionDelete404Logged: a DELETE carrying a
// session id minted by another stdio server is the unknown-session 404 — a
// protocol-layer terminal, recorded under the server that answered — and it
// leaves the owning server's session alone.
func TestR6MCPGateway_StdioForeignSessionDelete404Logged(t *testing.T) {
	t.Setenv("MCP_STDIO_R6_HELPER", "1")
	p, srv := r6Logged(t, []string{"k-A", "k-B"}, map[string]configdomain.MCPServer{"vision": r6StdioServer(), "twin": r6StdioServer()}, nil, nil)
	sid, _ := r6OpenSession(t, srv, r6PidProbe())
	if status, body, _ := r6Send(t, srv.URL+"/mcp/twin", http.MethodDelete, sid, ""); status != http.StatusNotFound || !strings.Contains(body, "unknown session") {
		t.Fatalf("DELETE of a foreign session = %d %q, want 404 unknown session", status, body)
	}
	if status, body, _ := r6CallTools(t, srv, sid); status != http.StatusOK {
		t.Fatalf("call on the owning server after the foreign DELETE = %d %q, want 200", status, body)
	}

	var want []r6LogRecord
	want = append(want, r6Records("vision", []r6Term{
		{method: "initialize", status: http.StatusOK, provider: r6AnyProvider},
		{method: "tools/call", status: http.StatusOK, tool: "image_analysis"},
	})...)
	want = append(want, r6Records("twin", []r6Term{{method: http.MethodDelete, status: http.StatusNotFound}})...)
	want = append(want, r6Records("vision", []r6Term{{method: "tools/call", status: http.StatusOK, tool: "image_analysis"}})...)
	r6RequireLog(t, p, want)
	r6RequireStats(t, p, "twin", 1, 1)
}

// TestR6MCPGateway_StdioSessionCap503Logged: running into the per-server
// child cap after the handshake fails through mcpFail (three-sided failure
// evidence), so it is recorded with status 503 and counted as an error in MCP
// stats alongside the admitted initializes before it.
func TestR6MCPGateway_StdioSessionCap503Logged(t *testing.T) {
	t.Setenv("MCP_STDIO_R6_HELPER", "1")
	p, srv := r6Logged(t, []string{"k-A", "k-B"}, map[string]configdomain.MCPServer{"vision": r6StdioServer()}, nil, nil)
	url := srv.URL + "/mcp/vision"
	var want []r6LogRecord
	for i := 0; i < mcpStdioPerServerCap; i++ {
		status, body, _ := r6Send(t, url, http.MethodPost, "", mcpInitBody)
		if status != http.StatusOK {
			t.Fatalf("initialize %d = %d %q, want 200", i+1, status, body)
		}
		want = append(want, r6Records("vision", []r6Term{{method: "initialize", status: http.StatusOK, provider: r6AnyProvider}})...)
	}
	if status, body, _ := r6Send(t, url, http.MethodPost, "", mcpInitBody); status != http.StatusServiceUnavailable || !strings.Contains(body, "too many live stdio sessions") {
		t.Fatalf("initialize %d = %d %q, want 503 naming the session cap", mcpStdioPerServerCap+1, status, body)
	}
	want = append(want, r6Records("vision", []r6Term{{method: "initialize", status: http.StatusServiceUnavailable, provider: r6AnyProvider}})...)
	r6RequireLog(t, p, want)
	r6RequireStats(t, p, "vision", mcpStdioPerServerCap+1, 1)
}
