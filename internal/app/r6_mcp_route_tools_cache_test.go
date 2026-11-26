package app

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	configdomain "model-proxy/internal/config"
)

// r6_mcp_route_tools_cache_test.go pins the gateway wiring of the route
// tools/list cache (R6 B21): a tools/list answered from the session cache
// costs no backend I/O — for a complete aggregation (cached for the session's
// lifetime) as much as for a degraded one inside its refresh window. The
// time-based refresh itself is pinned where the clock is injectable
// (internal/mcp, RouteToolsLoad).

// r6HitsByMethod summarizes a fake backend's hits by JSON-RPC method.
func r6HitsByMethod(b *fakeRouteBackend) map[string]int {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[string]int{}
	for _, h := range b.hits {
		out[h.method]++
	}
	return out
}

// r6OneHandshake is the footprint of one lazily built, listed sub-session:
// initialize + the initialized notification + tools/list.
var r6OneHandshake = map[string]int{"initialize": 1, "notifications/initialized": 1, "tools/list": 1}

// r6OpenRoute initializes a route session and returns its id.
func r6OpenRoute(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	resp := mcpPost(t, srv.URL+"/mcp/web-search", "", routeInitBody)
	sid := resp.Header.Get("Mcp-Session-Id")
	resp.Body.Close()
	if sid == "" {
		t.Fatal("no route session minted")
	}
	return sid
}

// r6RequireHits fails unless the named backends saw exactly want.
func r6RequireHits(t *testing.T, when string, want map[string]int, backends map[string]*fakeRouteBackend) {
	t.Helper()
	for name, b := range backends {
		if got := r6HitsByMethod(b); !reflect.DeepEqual(got, want) {
			t.Fatalf("backend %s hits %s = %v, want %v", name, when, got, want)
		}
	}
}

// TestR6MCPRoute_CompleteToolsListServedFromCache: once both backends
// contributed, every later tools/list of the session is the cached answer —
// byte-identical, with the backend call count of the first aggregation
// unchanged.
func TestR6MCPRoute_CompleteToolsListServedFromCache(t *testing.T) {
	zs, exa, servers, routes := twoRouteBackends(t)
	srv := newMCPRouteTestProxy(t, []string{"k-A"}, servers, routes)
	sid := r6OpenRoute(t, srv)
	backends := map[string]*fakeRouteBackend{"zs": zs, "exa": exa}

	resp := mcpPost(t, srv.URL+"/mcp/web-search", sid, routeListBody)
	first := mustRead(resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(first, `"name":"web_search"`) || !strings.Contains(first, "zhipu search") {
		t.Fatalf("first tools/list = %d %s, want the aggregated surface", resp.StatusCode, first)
	}
	r6RequireHits(t, "after the first tools/list", r6OneHandshake, backends)

	for i := 0; i < 3; i++ {
		resp = mcpPost(t, srv.URL+"/mcp/web-search", sid, routeListBody)
		if body := mustRead(resp); resp.StatusCode != http.StatusOK || body != first {
			t.Fatalf("tools/list %d = %d %s, want the cached first answer %s", i+2, resp.StatusCode, body, first)
		}
	}
	r6RequireHits(t, "after 4 tools/lists (no per-call I/O on a complete cache)", r6OneHandshake, backends)
}

// TestR6MCPRoute_DegradedToolsListServedFromCache: a tools/list that lost a
// down backend is still served from the session cache inside its refresh
// window — repeated lists neither retry the down backend nor re-list the
// healthy one.
func TestR6MCPRoute_DegradedToolsListServedFromCache(t *testing.T) {
	zs, exa, servers, routes := twoRouteBackends(t)
	zs.down = true
	srv := newMCPRouteTestProxy(t, []string{"k-A"}, servers, routes)
	sid := r6OpenRoute(t, srv)

	resp := mcpPost(t, srv.URL+"/mcp/web-search", sid, routeListBody)
	first := mustRead(resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(first, `"name":"web_search"`) || !strings.Contains(first, "exa search") || strings.Contains(first, "zhipu search") {
		t.Fatalf("degraded tools/list = %d %s, want the surviving backend's surface", resp.StatusCode, first)
	}
	// The fake records no hit while it is down (it answers 500 before the
	// hit log); once it recovers, any retry would show up here.
	r6RequireHits(t, "after the degraded tools/list", map[string]int{}, map[string]*fakeRouteBackend{"zs (down)": zs})
	r6RequireHits(t, "after the degraded tools/list", r6OneHandshake, map[string]*fakeRouteBackend{"exa": exa})

	// The backend recovers — inside the window the cache still answers, so
	// the recovered backend is not retried yet.
	zs.down = false
	for i := 0; i < 2; i++ {
		resp = mcpPost(t, srv.URL+"/mcp/web-search", sid, routeListBody)
		if body := mustRead(resp); resp.StatusCode != http.StatusOK || body != first {
			t.Fatalf("tools/list %d = %d %s, want the cached degraded answer inside the refresh window", i+2, resp.StatusCode, body)
		}
	}
	r6RequireHits(t, "inside the refresh window", map[string]int{}, map[string]*fakeRouteBackend{"zs (recovered)": zs})
	r6RequireHits(t, "inside the refresh window", r6OneHandshake, map[string]*fakeRouteBackend{"exa": exa})
}

// TestR6MCPRoute_ToolsListOfDepartedCallerNotCached: a tools/list whose
// client disconnects while its aggregation is in flight ends with every
// backend "failing" only because nobody waited for the answer. That result is
// not cached: the next tools/list aggregates for real instead of being served
// a surface with backends missing for a refresh window.
func TestR6MCPRoute_ToolsListOfDepartedCallerNotCached(t *testing.T) {
	zs, _, servers, routes := twoRouteBackends(t)
	// The first backend parks its initialize until the test lets it go, so
	// the aggregation is deterministically in flight when the client leaves.
	arrived := make(chan struct{})
	release := make(chan struct{})
	var announce, releaseOnce sync.Once
	gated := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readAll(r)
		if parseRPCMethod(body) == "initialize" {
			announce.Do(func() { close(arrived) })
			<-release
		}
		r.Body = io.NopCloser(bytes.NewReader(body)) // the backend reads the frame itself
		zs.serve(w, r)
	}))
	t.Cleanup(gated.Close) // cleanups run LIFO: the gate opens before the server waits for its handlers
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	servers["zs"] = configdomain.MCPServer{Provider: "zhipu", URL: gated.URL}
	p, srv := r6Logged(t, []string{"k-A"}, servers, routes, nil)
	url := srv.URL + "/mcp/web-search"
	status, body, sid := r6Send(t, url, http.MethodPost, "", routeInitBody)
	if status != http.StatusOK || sid == "" {
		t.Fatalf("initialize = %d %q", status, body)
	}

	// The departing client's request is served in-process: its cancellation
	// is then an explicit step instead of a transport race.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/mcp/web-search", strings.NewReader(routeListBody)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Mcp-Session-Id", sid)
	served := make(chan struct{})
	go func() {
		p.Handler(httptest.NewRecorder(), req)
		close(served)
	}()
	<-arrived // the aggregation is parked in the backend's initialize
	cancel()  // the client disconnects: the proxy gives up on every backend
	<-served
	releaseOnce.Do(func() { close(release) })

	if status, body, _ := r6Send(t, url, http.MethodPost, sid, routeListBody); status != http.StatusOK || !strings.Contains(body, "zhipu search") || strings.Contains(body, "exa search") {
		t.Fatalf("tools/list after the departed caller = %d %s, want a fresh aggregation with every backend", status, body)
	}
}
