package app

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/testsyn"
)

// r6_mcp_route_sub_account_test.go closes the route-side twin of the F2 gap:
// a route sub-session — the proxy's own binding between a client session and
// a backend account — must stop being used once that account is gone. The
// exchange that finds the stale binding clears it (stdio child included) and
// fails closed with 410, recorded like every protocol-layer terminal; the
// removed credential is never sent again, and the other backends' bindings
// are left alone.
//
// On-disk twins of the test proxies' config, for Reload: r6ConfigYAML wraps
// the shared provider block around the `mcp:` server blocks and the
// `mcp_routes:` block a test passes in; the pieces right below are those
// blocks.

func r6ConfigYAML(mcp, mcpRoutes string) string {
	return `listen: 127.0.0.1:0
providers:
  zhipu:
    provider_id: zhipu
    openai_base_url: http://127.0.0.1:1
    models: [m]
mcp:
` + mcp + mcpRoutes
}

// r6ZSYAML is the zhipu-backed HTTP backend "zs".
func r6ZSYAML(upstream string) string {
	return fmt.Sprintf("  zs:\n    provider: zhipu\n    url: %s\n", upstream)
}

// r6VisionYAML is the stdio backend "vision" (the twin of r6StdioServer).
func r6VisionYAML() string {
	return fmt.Sprintf(`  vision:
    transport: stdio
    provider: zhipu
    command: [%q, "-test.run=^TestR6StdioHelper$"]
    env:
      MCP_STDIO_R6_HELPER: "env:MCP_STDIO_R6_HELPER"
      CHILD_KEY: "${account.api_key}"
`, os.Args[0])
}

const r6WebSearchRouteYAML = `mcp_routes:
  web-search:
    targets:
      - mcp: zs
        tools: {web_search: web_search_prime}
`

const r6VisionRouteYAML = `mcp_routes:
  vision-route:
    targets:
      - mcp: vision
        tools: {analyze: image_analysis}
`

// r6MixedRouteYAML aggregates an HTTP backend and a stdio backend.
const r6MixedRouteYAML = `mcp_routes:
  mixed:
    targets:
      - mcp: zs
        tools: {web_search: web_search_prime}
      - mcp: vision
        tools: {analyze: image_analysis}
`

func r6StdioRoute() map[string]configdomain.MCPRoute {
	return map[string]configdomain.MCPRoute{"vision-route": {Targets: []configdomain.MCPRouteTarget{
		{Server: "vision", Tools: map[string]string{"analyze": "image_analysis"}},
	}}}
}

func r6MixedRoute() map[string]configdomain.MCPRoute {
	return map[string]configdomain.MCPRoute{"mixed": {Targets: []configdomain.MCPRouteTarget{
		{Server: "zs", Tools: map[string]string{"web_search": "web_search_prime"}},
		{Server: "vision", Tools: map[string]string{"analyze": "image_analysis"}},
	}}}
}

// r6Reload shrinks the zhipu pool to the one remaining key, writes a config
// twin under the test HOME and reloads it — the generation that dropped every
// other account. The reload kicks off a quota re-sync that logs a failure
// against the fixture's unusable upstream: build the proxy through r6Logged
// (or mute the log before building it), so the line lands on a muted logger.
func r6Reload(t *testing.T, p *Proxy, yaml, remaining string) {
	t.Helper()
	writeMCPKeys(t, "zhipu", remaining)
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Fatalf("test HOME: %q %v", home, err)
	}
	yamlPath := filepath.Join(home, "config.yaml")
	if err := os.WriteFile(yamlPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p.Reload(yamlPath); err != nil {
		t.Fatalf("reload: %v", err)
	}
}

// r6PidProbe reports whether process liveness can be probed with Signal(0)
// on this platform. A test decides it once, up front, and hands it to the
// helpers that assert on pids: where it is false those assertions are not
// made and every other assertion runs unchanged — nothing is skipped
// half-way through a test.
func r6PidProbe() bool { return runtime.GOOS != "windows" }

// r6Hits returns a copy of the backend's hit log.
func r6Hits(b *fakeRouteBackend) []routeHit {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]routeHit(nil), b.hits...)
}

// r6LastHit returns the backend's most recent hit of one JSON-RPC method.
func r6LastHit(b *fakeRouteBackend, method string) routeHit {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := len(b.hits) - 1; i >= 0; i-- {
		if b.hits[i].method == method {
			return b.hits[i]
		}
	}
	return routeHit{}
}

// r6OpenHTTPRoute initializes the single-target HTTP route session and drives
// one tools/call, so its backend sub-session is bound to a pool account. It
// returns the session id and the credential ("Bearer <key>") that call used.
func r6OpenHTTPRoute(t *testing.T, url string, zs *fakeRouteBackend) (sid, bound string) {
	t.Helper()
	status, body, sid := r6Send(t, url, http.MethodPost, "", routeInitBody)
	if status != http.StatusOK || sid == "" {
		t.Fatalf("initialize = %d %q", status, body)
	}
	if status, body, _ = r6Send(t, url, http.MethodPost, sid, routeCallBody("web_search")); status != http.StatusOK || !strings.Contains(body, "served:web_search_prime") {
		t.Fatalf("first call = %d %q, want it served by the backend", status, body)
	}
	if bound = zs.lastCall().auth; !strings.HasPrefix(bound, "Bearer k-") {
		t.Fatalf("backend saw credential %q, want a pool account's key", bound)
	}
	return sid, bound
}

// r6OpenStdioRoute is the stdio twin: it returns the route session id and
// the child's report (pid, and the key its sub-session is bound to). Where
// pids can be probed, the child must be running here — a later "reaped"
// assertion is only meaningful once the same probe has seen it alive.
func r6OpenStdioRoute(t *testing.T, url string, pidProbe bool) (string, r6ToolResult) {
	t.Helper()
	status, body, sid := r6Send(t, url, http.MethodPost, "", routeInitBody)
	if status != http.StatusOK || sid == "" {
		t.Fatalf("initialize = %d %q", status, body)
	}
	if status, body, _ = r6Send(t, url, http.MethodPost, sid, routeCallBody("analyze")); status != http.StatusOK {
		t.Fatalf("first call = %d %q, want it served by the child", status, body)
	}
	first, ok := r6ChildReport(t, body)
	if !ok {
		t.Fatalf("first call = %q: not the child's report", body)
	}
	r6RequireChildAlive(t, first.PID, pidProbe)
	return sid, first
}

// r6RequireChildAlive asserts, where pids can be probed, that the helper
// child with the pid is running.
func r6RequireChildAlive(t *testing.T, pid int, pidProbe bool) {
	t.Helper()
	if pidProbe && !r6ProcessAlive(pid) {
		t.Fatalf("child %d does not answer Signal(0) while serving — a reaped assertion on it would be vacuous", pid)
	}
}

// r6RequireChildReaped waits — where pids can be probed — for the helper
// child with the pid to be gone.
func r6RequireChildReaped(t *testing.T, pid int, what string, pidProbe bool) {
	t.Helper()
	if !pidProbe {
		return
	}
	testsyn.WaitUntil(t, what, func() bool {
		return !r6ProcessAlive(pid)
	})
}

// TestR6MCPRoute_SubSessionAccountRemovedFailsClosed410: a tools/call that
// finds the backend sub-session bound to an account a later generation
// dropped answers 410 (recorded) instead of retrying the dead binding
// forever; the next call rebuilds the sub-session on an account that still
// exists, and the removed credential is never sent again.
func TestR6MCPRoute_SubSessionAccountRemovedFailsClosed410(t *testing.T) {
	zs, _, servers, _ := twoRouteBackends(t)
	p, srv := r6Logged(t, []string{"k-A", "k-B"}, servers, r6SingleTargetRoute(), nil)
	url := srv.URL + "/mcp/web-search"
	sid, bound := r6OpenHTTPRoute(t, url, zs)
	other := "Bearer " + r6OtherKey(t, strings.TrimPrefix(bound, "Bearer "))
	before := len(r6Hits(zs))
	r6Reload(t, p, r6ConfigYAML(r6ZSYAML(servers["zs"].URL), r6WebSearchRouteYAML), strings.TrimPrefix(other, "Bearer "))

	status, body, _ := r6Send(t, url, http.MethodPost, sid, routeCallBody("web_search"))
	if status != http.StatusGone || strings.TrimSpace(body) != mcpSessionAccountGoneMsg {
		t.Fatalf("call on the stale sub-session = %d %q, want 410 %q", status, body, mcpSessionAccountGoneMsg)
	}
	if status, body, _ = r6Send(t, url, http.MethodPost, sid, routeCallBody("web_search")); status != http.StatusOK || !strings.Contains(body, "served:web_search_prime") {
		t.Fatalf("call after the stale binding was cleared = %d %q, want the sub-session rebuilt", status, body)
	}
	for i, h := range r6Hits(zs)[before:] {
		if h.auth == bound {
			t.Fatalf("removed credential sent after the reload (hit %d: %+v)", before+i, h)
		}
	}
	if got := zs.lastCall().auth; got != other {
		t.Fatalf("rebuilt sub-session authenticates as %q, want the surviving account %q", got, other)
	}

	r6RequireLog(t, p, r6Records("web-search", []r6Term{
		{method: "initialize", status: http.StatusOK},
		{method: "tools/call", status: http.StatusOK, tool: "web_search", provider: "zs"},
		{method: "tools/call", status: http.StatusGone, tool: "web_search"},
		{method: "tools/call", status: http.StatusOK, tool: "web_search", provider: "zs"},
	}))
	r6RequireStats(t, p, "web-search", 4, 1)
}

// TestR6MCPRoute_ToolsListOnStaleSubSessionFailsClosed410: tools/list
// aggregates through the same sub-sessions, so it takes the same fail-closed
// answer when it finds a stale binding — instead of silently listing the
// backend's tools as missing. The aborted aggregation is not cached either:
// the very next tools/list aggregates again, on an account that still exists.
func TestR6MCPRoute_ToolsListOnStaleSubSessionFailsClosed410(t *testing.T) {
	zs, _, servers, _ := twoRouteBackends(t)
	p, srv := r6Logged(t, []string{"k-A", "k-B"}, servers, r6SingleTargetRoute(), nil)
	url := srv.URL + "/mcp/web-search"
	sid, bound := r6OpenHTTPRoute(t, url, zs)
	other := "Bearer " + r6OtherKey(t, strings.TrimPrefix(bound, "Bearer "))
	r6Reload(t, p, r6ConfigYAML(r6ZSYAML(servers["zs"].URL), r6WebSearchRouteYAML), strings.TrimPrefix(other, "Bearer "))

	status, body, _ := r6Send(t, url, http.MethodPost, sid, routeListBody)
	if status != http.StatusGone || strings.TrimSpace(body) != mcpSessionAccountGoneMsg {
		t.Fatalf("tools/list on the stale sub-session = %d %q, want 410 %q", status, body, mcpSessionAccountGoneMsg)
	}
	// Nothing of the aborted aggregation stayed behind in the session's cache:
	// this tools/list re-aggregates at once instead of answering the degraded
	// list (the backend's tools missing) for a refresh window.
	if status, body, _ = r6Send(t, url, http.MethodPost, sid, routeListBody); status != http.StatusOK || !strings.Contains(body, "zhipu search") {
		t.Fatalf("tools/list right after the 410 = %d %q, want the backend's tools re-aggregated at once", status, body)
	}
	if got := r6LastHit(zs, "tools/list"); got.auth != other {
		t.Fatalf("the re-aggregation listed the backend as %q, want the surviving account %q", got.auth, other)
	}
	r6RequireLog(t, p, r6Records("web-search", []r6Term{
		{method: "initialize", status: http.StatusOK},
		{method: "tools/call", status: http.StatusOK, tool: "web_search", provider: "zs"},
		{method: "tools/list", status: http.StatusGone},
		{method: "tools/list", status: http.StatusOK},
	}))
	r6RequireStats(t, p, "web-search", 4, 1)
}

// TestR6MCPRoute_StdioSubSessionAccountRemovedFailsClosed410: the stdio
// backend's child carries the bound account's key in its environment. Once a
// later generation dropped that account, the call that finds the binding
// answers 410 and the child is killed; the next call gets a fresh child on an
// account that still exists.
func TestR6MCPRoute_StdioSubSessionAccountRemovedFailsClosed410(t *testing.T) {
	pidProbe := r6PidProbe()
	t.Setenv("MCP_STDIO_R6_HELPER", "1")
	p, srv := r6Logged(t, []string{"k-A", "k-B"}, map[string]configdomain.MCPServer{"vision": r6StdioServer()}, r6StdioRoute(), nil)
	url := srv.URL + "/mcp/vision-route"
	sid, first := r6OpenStdioRoute(t, url, pidProbe)
	other := r6OtherKey(t, first.Key)
	r6Reload(t, p, r6ConfigYAML(r6VisionYAML(), r6VisionRouteYAML), other)

	status, body, _ := r6Send(t, url, http.MethodPost, sid, routeCallBody("analyze"))
	if status != http.StatusGone || strings.TrimSpace(body) != mcpSessionAccountGoneMsg {
		t.Fatalf("call on the stale sub-session = %d %q, want 410 %q", status, body, mcpSessionAccountGoneMsg)
	}
	r6RequireChildReaped(t, first.PID, "stale sub-session's child process to be reaped", pidProbe)
	if status, body, _ = r6Send(t, url, http.MethodPost, sid, routeCallBody("analyze")); status != http.StatusOK {
		t.Fatalf("call after the stale binding was cleared = %d %q, want the sub-session rebuilt", status, body)
	}
	if second, ok := r6ChildReport(t, body); !ok || second.Key != other || second.PID == first.PID || second.Calls != 1 {
		t.Fatalf("rebuilt sub-session answered %+v (parsed %v), want a fresh child on account %q", second, ok, other)
	}
	r6RequireLog(t, p, r6Records("vision-route", []r6Term{
		{method: "initialize", status: http.StatusOK},
		{method: "tools/call", status: http.StatusOK, tool: "analyze", provider: "vision"},
		{method: "tools/call", status: http.StatusGone, tool: "analyze"},
		{method: "tools/call", status: http.StatusOK, tool: "analyze", provider: "vision"},
	}))
	r6RequireStats(t, p, "vision-route", 4, 1)
}

// TestR6MCPRoute_NotificationDropsStaleSubSession: a notification is fanned
// out to every initialized sub-session and must not be the path that keeps a
// stale binding — and its child, with the account key in its environment —
// in use. The binding is dropped with its child; the client still gets 202
// (notification semantics). Stale means the account was dropped by a reload
// or frozen by an operator alike.
func TestR6MCPRoute_NotificationDropsStaleSubSession(t *testing.T) {
	t.Setenv("MCP_STDIO_R6_HELPER", "1")
	t.Run("account removed by reload", func(t *testing.T) {
		r6NotificationFlow(t, func(t *testing.T, p *Proxy, first r6ToolResult) {
			r6Reload(t, p, r6ConfigYAML(r6VisionYAML(), r6VisionRouteYAML), r6OtherKey(t, first.Key))
		})
	})
	t.Run("account operator-frozen", func(t *testing.T) {
		r6NotificationFlow(t, func(t *testing.T, p *Proxy, first r6ToolResult) {
			freezeAccountForKey(t, p, first.Key)
		})
	})
}

func r6NotificationFlow(t *testing.T, stale func(*testing.T, *Proxy, r6ToolResult)) {
	t.Helper()
	pidProbe := r6PidProbe()
	p, srv := r6Logged(t, []string{"k-A", "k-B"}, map[string]configdomain.MCPServer{"vision": r6StdioServer()}, r6StdioRoute(), nil)
	url := srv.URL + "/mcp/vision-route"
	sid, first := r6OpenStdioRoute(t, url, pidProbe)
	stale(t, p, first)

	if status, body, _ := r6Send(t, url, http.MethodPost, sid, mcpNotifBody); status != http.StatusAccepted {
		t.Fatalf("notification = %d %q, want 202", status, body)
	}
	r6RequireChildReaped(t, first.PID, "stale sub-session's child process to be reaped", pidProbe)
	r6RequireLog(t, p, r6Records("vision-route", []r6Term{
		{method: "initialize", status: http.StatusOK},
		{method: "tools/call", status: http.StatusOK, tool: "analyze", provider: "vision"},
		{method: "notifications/initialized", status: http.StatusAccepted},
	}))
}

// r6FreezePool freezes every live account of the zhipu pool: both virtual
// accounts of a two-key pool, or the plain provider name of a single-key one.
func r6FreezePool(t *testing.T, p *Proxy) {
	t.Helper()
	ids := mcpAccounts(p.SnapshotRuntime(), configdomain.MCPServer{Provider: "zhipu"})
	if len(ids) == 0 {
		t.Fatal("the zhipu pool has no live account to freeze")
	}
	parentOf := make(map[string]string, len(ids))
	for _, id := range ids {
		parentOf[id] = "zhipu"
	}
	if got := p.runtimeState.FreezeHealth("zhipu", parentOf, ids); len(got) != len(ids) {
		t.Fatalf("freezing the zhipu pool matched %v, want all of %v", got, ids)
	}
}

// TestR6MCPRoute_FrozenSubSessionChildReapedEvenIfRebuildFails: once a
// sub-session's account is operator-frozen, its child — the frozen key is in
// its environment — dies the moment an exchange finds the binding. It does
// not wait for a rebuild on a healthy account to replace it: when there is no
// account to rebuild on, or the replacement cannot be started, the frozen
// child must not survive that either.
func TestR6MCPRoute_FrozenSubSessionChildReapedEvenIfRebuildFails(t *testing.T) {
	const reaped = "frozen sub-session's child process to be reaped"
	t.Setenv("MCP_STDIO_R6_HELPER", "1")

	t.Run("tools/call: the replacement cannot be started", func(t *testing.T) {
		pidProbe := r6PidProbe()
		// The child can only be spawned while R6_SPAWN_GATE is set; unsetting
		// it makes the rebuild fail before anything is forked.
		t.Setenv("R6_SPAWN_GATE", "1")
		srv := r6StdioServer()
		srv.Env["R6_SPAWN_GATE"] = "env:R6_SPAWN_GATE"
		p, proxySrv := r6Logged(t, []string{"k-A", "k-B"}, map[string]configdomain.MCPServer{"vision": srv}, r6StdioRoute(), nil)
		url := proxySrv.URL + "/mcp/vision-route"
		sid, first := r6OpenStdioRoute(t, url, pidProbe)
		freezeAccountForKey(t, p, first.Key)
		if err := os.Unsetenv("R6_SPAWN_GATE"); err != nil {
			t.Fatal(err)
		}

		status, body, _ := r6Send(t, url, http.MethodPost, sid, routeCallBody("analyze"))
		if status != http.StatusBadGateway || !strings.Contains(body, "R6_SPAWN_GATE") {
			t.Fatalf("call after the freeze = %d %q, want 502 naming why the replacement could not start", status, body)
		}
		r6RequireChildReaped(t, first.PID, reaped, pidProbe)
	})

	t.Run("no account to rebuild on", func(t *testing.T) {
		pidProbe := r6PidProbe()
		p, proxySrv := r6Logged(t, []string{"k-A"}, map[string]configdomain.MCPServer{"vision": r6StdioServer()}, r6StdioRoute(), nil)
		url := proxySrv.URL + "/mcp/vision-route"
		callSid, callChild := r6OpenStdioRoute(t, url, pidProbe)
		listSid, listChild := r6OpenStdioRoute(t, url, pidProbe)
		r6FreezePool(t, p)

		// tools/call is answered before it reaches the sub-session (every
		// account is frozen) — the frozen child dies all the same.
		if status, body, _ := r6Send(t, url, http.MethodPost, callSid, routeCallBody("analyze")); status != http.StatusOK || !strings.Contains(body, "frozen") {
			t.Fatalf("call after the freeze = %d %q, want the -32000 frozen/cooldown answer", status, body)
		}
		r6RequireChildReaped(t, callChild.PID, reaped, pidProbe)
		// tools/list goes through the sub-session and finds no account to
		// rebuild it on: degraded to an empty surface, child gone.
		if status, body, _ := r6Send(t, url, http.MethodPost, listSid, routeListBody); status != http.StatusOK || !strings.Contains(body, `"tools":[]`) {
			t.Fatalf("tools/list after the freeze = %d %q, want the degraded empty surface", status, body)
		}
		r6RequireChildReaped(t, listChild.PID, reaped, pidProbe)
	})
}

// r6OpenMixedRoute opens a session on the two-backend route with each
// backend's sub-session bound to a different pool account: the HTTP
// backend's is built first (round-robin takes one account), the stdio
// backend's right after (the next one). It returns the session id, the HTTP
// backend's credential ("Bearer <key>") and the stdio child's report.
func r6OpenMixedRoute(t *testing.T, url string, zs *fakeRouteBackend, pidProbe bool) (sid, httpBound string, child r6ToolResult) {
	t.Helper()
	status, body, sid := r6Send(t, url, http.MethodPost, "", routeInitBody)
	if status != http.StatusOK || sid == "" {
		t.Fatalf("initialize = %d %q", status, body)
	}
	if status, body, _ = r6Send(t, url, http.MethodPost, sid, routeCallBody("web_search")); status != http.StatusOK || !strings.Contains(body, "served:web_search_prime") {
		t.Fatalf("call on the HTTP backend = %d %q, want it served", status, body)
	}
	httpBound = zs.lastCall().auth
	if status, body, _ = r6Send(t, url, http.MethodPost, sid, routeCallBody("analyze")); status != http.StatusOK {
		t.Fatalf("call on the stdio backend = %d %q, want it served", status, body)
	}
	var ok bool
	if child, ok = r6ChildReport(t, body); !ok {
		t.Fatalf("call on the stdio backend = %q: not the child's report", body)
	}
	if child.Key == strings.TrimPrefix(httpBound, "Bearer ") {
		t.Fatalf("fixture: both backends bound the same account (key %q); the two consecutive sub-session builds take different ones", child.Key)
	}
	r6RequireChildAlive(t, child.PID, pidProbe)
	return sid, httpBound, child
}

// r6RequireSiblingServing asserts the healthy backend's sub-session is the
// one it was: the next tools/call is answered by the very same child, still
// on its own account, one call further along.
func r6RequireSiblingServing(t *testing.T, url, sid string, sibling r6ToolResult, healthy string) {
	t.Helper()
	status, body, _ := r6Send(t, url, http.MethodPost, sid, routeCallBody("analyze"))
	if status != http.StatusOK {
		t.Fatalf("call on the healthy backend = %d %q, want 200", status, body)
	}
	got, ok := r6ChildReport(t, body)
	if !ok || got.PID != sibling.PID || got.Key != healthy || got.Calls != sibling.Calls+1 {
		t.Fatalf("healthy backend answered %+v (parsed %v); want its own child (pid %d) on account %q, call %d", got, ok, sibling.PID, healthy, sibling.Calls+1)
	}
}

// TestR6MCPRoute_StaleSubSessionDropSparesHealthySibling: dropping a stale
// sub-session touches that backend alone. On a two-backend route — an HTTP
// backend and a stdio backend, each bound to a different pool account —
// making the HTTP backend's account unusable clears its sub-session only:
// the stdio backend's child is left alone and keeps serving tools/call. Both
// paths that drop are covered: the fail-closed 410 and the notification
// fan-out.
func TestR6MCPRoute_StaleSubSessionDropSparesHealthySibling(t *testing.T) {
	t.Setenv("MCP_STDIO_R6_HELPER", "1")
	// open builds the route, a session on it and both backends' sub-sessions.
	type fixture struct {
		p        *Proxy
		url      string
		sid      string
		zsURL    string       // the HTTP backend's upstream
		staleKey string       // the HTTP backend's account (made unusable below)
		sibling  r6ToolResult // the stdio backend's child, on the other account
	}
	open := func(t *testing.T) fixture {
		t.Helper()
		zs, _, servers, _ := twoRouteBackends(t)
		servers["vision"] = r6StdioServer()
		p, proxySrv := r6Logged(t, []string{"k-A", "k-B"}, servers, r6MixedRoute(), nil)
		url := proxySrv.URL + "/mcp/mixed"
		sid, httpBound, sibling := r6OpenMixedRoute(t, url, zs, r6PidProbe())
		return fixture{p: p, url: url, sid: sid, zsURL: servers["zs"].URL, staleKey: strings.TrimPrefix(httpBound, "Bearer "), sibling: sibling}
	}

	t.Run("410 path: the account was removed by a reload", func(t *testing.T) {
		f := open(t)
		r6Reload(t, f.p, r6ConfigYAML(r6ZSYAML(f.zsURL)+r6VisionYAML(), r6MixedRouteYAML), f.sibling.Key)

		if status, body, _ := r6Send(t, f.url, http.MethodPost, f.sid, routeCallBody("web_search")); status != http.StatusGone || strings.TrimSpace(body) != mcpSessionAccountGoneMsg {
			t.Fatalf("call on the stale sub-session = %d %q, want 410 %q", status, body, mcpSessionAccountGoneMsg)
		}
		r6RequireSiblingServing(t, f.url, f.sid, f.sibling, f.sibling.Key)
	})

	t.Run("notification path: the account was operator-frozen", func(t *testing.T) {
		f := open(t)
		freezeAccountForKey(t, f.p, f.staleKey)

		if status, body, _ := r6Send(t, f.url, http.MethodPost, f.sid, mcpNotifBody); status != http.StatusAccepted {
			t.Fatalf("notification = %d %q, want 202", status, body)
		}
		r6RequireSiblingServing(t, f.url, f.sid, f.sibling, f.sibling.Key)
	})
}
