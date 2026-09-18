package app

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	configdomain "model-proxy/internal/config"
	mcpkg "model-proxy/internal/mcp"
)

// ---- stdio backend tests ----

// TestHelperProcess doubles as the fake MCP stdio child for app-level tests:
// with MCP_STDIO_APP_HELPER=1 it serves newline-delimited JSON-RPC on
// stdin/stdout, echoing its Z_AI_API_KEY-style env in tool results.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("MCP_STDIO_APP_HELPER") != "1" {
		t.Skip("not a helper subprocess")
	}
	os.Exit(runMCPStdioAppHelper())
}

func runMCPStdioAppHelper() int {
	scanner := bufio.NewScanner(os.Stdin)
	writer := bufio.NewWriter(os.Stdout)
	for scanner.Scan() {
		var v struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params *struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &v); err != nil {
			continue
		}
		respond := func(result string) {
			fmt.Fprintf(writer, `{"jsonrpc":"2.0","id":%s,"result":%s}`+"\n", v.ID, result)
			writer.Flush()
		}
		switch v.Method {
		case "initialize":
			if os.Getenv("HANG_CHILD") == "1" {
				continue // hung backend: accept the request, never answer
			}
			respond(`{"protocolVersion":"2025-03-26","capabilities":{"tools":{}},"serverInfo":{"name":"fake-stdio-backend","version":"2.0"}}`)
		case "notifications/initialized":
			// no response
		case "tools/list":
			respond(`{"tools":[{"name":"image_analysis","description":"vision","inputSchema":{"type":"object"}}]}`)
		case "tools/call":
			name := ""
			if v.Params != nil {
				name = v.Params.Name
			}
			respond(fmt.Sprintf(`{"content":[{"type":"text","text":%q}]}`, "stdio:"+name+" key:"+os.Getenv("CHILD_KEY")))
		}
	}
	return 0
}

// stdioServerConfig builds a transport:stdio server entry running the test
// binary as child with ${account.api_key} templated into CHILD_KEY. The
// helper marker goes through env: indirection to exercise that path too.
func stdioServerConfig(t *testing.T) configdomain.MCPServer {
	t.Helper()
	t.Setenv("MCP_STDIO_APP_HELPER", "1")
	return configdomain.MCPServer{
		Transport: "stdio",
		Provider:  "zhipu",
		Command:   []string{os.Args[0], "-test.run=TestHelperProcess"},
		Env: map[string]string{
			"MCP_STDIO_APP_HELPER": "env:MCP_STDIO_APP_HELPER",
			"CHILD_KEY":            "${account.api_key}",
		},
	}
}

// TestMCPGateway_StdioPinnedFlow: initialize spawns the child and mints a
// session; calls go through the child with the templated credential; DELETE
// kills the child and drops the session.
func TestMCPGateway_StdioPinnedFlow(t *testing.T) {
	srv := newMCPTestProxy(t, "zhipu", []string{"k-A"}, map[string]configdomain.MCPServer{
		"vision": stdioServerConfig(t),
	})

	resp := mcpPost(t, srv.URL+"/mcp/vision", "", mcpInitBody)
	if resp.StatusCode != 200 {
		t.Fatalf("initialize = %d", resp.StatusCode)
	}
	sid := resp.Header.Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatal("no session minted for stdio server")
	}
	if body := mustRead(resp); !strings.Contains(body, "fake-stdio-backend") {
		t.Fatalf("initialize result not relayed: %s", body)
	}

	// tools/call through the child: templated account key in its env.
	resp = mcpPost(t, srv.URL+"/mcp/vision", sid, routeCallBody("image_analysis"))
	if resp.StatusCode != 200 {
		t.Fatalf("call = %d", resp.StatusCode)
	}
	body := mustRead(resp)
	if !strings.Contains(body, "stdio:image_analysis") || !strings.Contains(body, "key:k-A") {
		t.Fatalf("call = %s (account key not templated into child env?)", body)
	}

	// tools/call without a session → 400.
	if r := mcpPost(t, srv.URL+"/mcp/vision", "", routeCallBody("image_analysis")); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("sessionless call = %d, want 400", r.StatusCode)
	}

	// GET → 405.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/mcp/vision", nil)
	gresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if gresp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d, want 405", gresp.StatusCode)
	}
	gresp.Body.Close()

	// DELETE: child killed, session dropped.
	req, _ = http.NewRequest(http.MethodDelete, srv.URL+"/mcp/vision", nil)
	req.Header.Set("Mcp-Session-Id", sid)
	dresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	dresp.Body.Close()
	if dresp.StatusCode != 200 {
		t.Fatalf("DELETE = %d", dresp.StatusCode)
	}
	if r := mcpPost(t, srv.URL+"/mcp/vision", sid, routeCallBody("image_analysis")); r.StatusCode != http.StatusNotFound {
		t.Fatalf("post-DELETE call = %d, want 404", r.StatusCode)
	}
}

// TestMCPGateway_StdioChildDeathFailsClosed: when the child dies mid-session,
// the call errors 502 and the session is dropped (client re-initializes).
func TestMCPGateway_StdioChildDeathFailsClosed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeMCPKeys(t, "zhipu", "k-A")
	cfg := &configdomain.Config{
		Listen: "127.0.0.1:0",
		Providers: map[string]configdomain.Provider{
			"zhipu": {Provider: "zhipu", OpenAIBaseURL: "http://127.0.0.1:1", Models: []string{"m"}},
		},
		MCP: map[string]configdomain.MCPServer{"vision": stdioServerConfig(t)},
	}
	p := newTestProxy(t, cfg)
	srv := httptest.NewServer(http.HandlerFunc(p.Handler))
	t.Cleanup(srv.Close)

	resp := mcpPost(t, srv.URL+"/mcp/vision", "", mcpInitBody)
	sid := resp.Header.Get("Mcp-Session-Id")
	resp.Body.Close()

	// Kill the child out-of-band (simulating a crash).
	p.mcpStdio.kill(sid)

	resp = mcpPost(t, srv.URL+"/mcp/vision", sid, routeCallBody("image_analysis"))
	if resp.StatusCode != http.StatusBadGateway && resp.StatusCode != http.StatusNotFound {
		t.Fatalf("dead child call = %d, want 502 or 404", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestMCPRoute_StdioBackend: an aggregated route can target a stdio backend —
// the sub-session is a child process keyed by (route session, server).
func TestMCPRoute_StdioBackend(t *testing.T) {
	servers := map[string]configdomain.MCPServer{
		"vision": stdioServerConfig(t),
		"exa":    {URL: "http://127.0.0.1:1/mcp", Auth: "none"}, // unreachable; stdio is target 1
	}
	routes := map[string]configdomain.MCPRoute{
		"vision-route": {Targets: []configdomain.MCPRouteTarget{
			{Server: "vision", Tools: map[string]string{"analyze": "image_analysis"}},
		}},
	}
	srv := newMCPRouteTestProxy(t, []string{"k-A"}, servers, routes)

	resp := mcpPost(t, srv.URL+"/mcp/vision-route", "", routeInitBody)
	sid := resp.Header.Get("Mcp-Session-Id")
	resp.Body.Close()

	resp = mcpPost(t, srv.URL+"/mcp/vision-route", sid, routeListBody)
	if body := mustRead(resp); !strings.Contains(body, `"name":"analyze"`) || strings.Contains(body, "image_analysis") {
		t.Fatalf("tools/list (canonical surface, backend name hidden) = %s", body)
	}

	resp = mcpPost(t, srv.URL+"/mcp/vision-route", sid, routeCallBody("analyze"))
	body := mustRead(resp)
	if !strings.Contains(body, "stdio:image_analysis") || !strings.Contains(body, "key:k-A") {
		t.Fatalf("route stdio call = %s", body)
	}

	// DELETE kills the sub-session child.
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/mcp/vision-route", nil)
	req.Header.Set("Mcp-Session-Id", sid)
	dresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	dresp.Body.Close()
	if r := mcpPost(t, srv.URL+"/mcp/vision-route", sid, routeCallBody("analyze")); r.StatusCode != http.StatusNotFound {
		t.Fatalf("post-DELETE = %d, want 404", r.StatusCode)
	}
}

// TestMCPStdioEvictionKillsChild: session-table removal (TTL/LRU/DELETE)
// drives child reaping through the OnEvict wiring — the pinned child's
// registry entry dies with its session, route sub children with theirs.
func TestMCPStdioEvictionKillsChild(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeMCPKeys(t, "zhipu", "k-A")
	cfg := &configdomain.Config{
		Listen: "127.0.0.1:0",
		Providers: map[string]configdomain.Provider{
			"zhipu": {Provider: "zhipu", OpenAIBaseURL: "http://127.0.0.1:1", Models: []string{"m"}},
		},
		MCP: map[string]configdomain.MCPServer{"vision": stdioServerConfig(t)},
	}
	p := newTestProxy(t, cfg)
	srv := httptest.NewServer(http.HandlerFunc(p.Handler))
	t.Cleanup(srv.Close)

	resp := mcpPost(t, srv.URL+"/mcp/vision", "", mcpInitBody)
	sid := resp.Header.Get("Mcp-Session-Id")
	resp.Body.Close()
	if _, ok := p.mcpStdio.get(sid); !ok {
		t.Fatal("child not registered after initialize")
	}
	// Session removal via the table (any cause) must reap the child.
	p.mcpSessions.Delete(sid)
	if _, ok := p.mcpStdio.get(sid); ok {
		t.Fatal("child survived session removal")
	}
}

// TestMCPGateway_StdioSessionBoundToServer: sessions are owned by their
// server — A's session id against /mcp/B 404s (never reaches B's child), and
// a cross-server DELETE must not kill A's child. Regression for the pinned
// stdio path skipping the ownership check the HTTP pinned path has.
func TestMCPGateway_StdioSessionBoundToServer(t *testing.T) {
	srv := newMCPTestProxy(t, "zhipu", []string{"k-A"}, map[string]configdomain.MCPServer{
		"vision-a": stdioServerConfig(t),
		"vision-b": stdioServerConfig(t),
	})
	resp := mcpPost(t, srv.URL+"/mcp/vision-a", "", mcpInitBody)
	sid := resp.Header.Get("Mcp-Session-Id")
	resp.Body.Close()
	if sid == "" {
		t.Fatal("no session minted")
	}
	if r := mcpPost(t, srv.URL+"/mcp/vision-b", sid, routeCallBody("image_analysis")); r.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-server call = %d, want 404", r.StatusCode)
	} else {
		r.Body.Close()
	}
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/mcp/vision-b", nil)
	req.Header.Set("Mcp-Session-Id", sid)
	dresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	dresp.Body.Close()
	if dresp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-server DELETE = %d, want 404", dresp.StatusCode)
	}
	// A's session and child survived the cross-server DELETE.
	if r := mcpPost(t, srv.URL+"/mcp/vision-a", sid, routeCallBody("image_analysis")); r.StatusCode != http.StatusOK {
		t.Fatalf("own session after cross-server DELETE = %d, want 200", r.StatusCode)
	} else {
		r.Body.Close()
	}
}

// TestMCPGateway_StdioHungChildTimesOut: a child that never answers must not
// park the caller — the server's timeout: bounds the stdio exchange, the
// client gets 502, and the child is reaped. Regression for the ctx-less
// StdioConn.Call blocking forever.
func TestMCPGateway_StdioHungChildTimesOut(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("HANG_CHILD", "1")
	writeMCPKeys(t, "zhipu", "k-A")
	srvCfg := stdioServerConfig(t)
	srvCfg.Timeout = "100ms"
	srvCfg.Env["HANG_CHILD"] = "env:HANG_CHILD"
	cfg := &configdomain.Config{
		Listen: "127.0.0.1:0",
		Providers: map[string]configdomain.Provider{
			"zhipu": {Provider: "zhipu", OpenAIBaseURL: "http://127.0.0.1:1", Models: []string{"m"}},
		},
		MCP: map[string]configdomain.MCPServer{"vision": srvCfg},
	}
	p := newTestProxy(t, cfg)
	srv := httptest.NewServer(http.HandlerFunc(p.Handler))
	t.Cleanup(srv.Close)

	done := make(chan int, 1)
	go func() {
		resp, err := http.Post(srv.URL+"/mcp/vision", "application/json", strings.NewReader(mcpInitBody))
		if err != nil {
			done <- -1
			return
		}
		resp.Body.Close()
		done <- resp.StatusCode
	}()
	select {
	case st := <-done:
		if st != http.StatusBadGateway {
			t.Fatalf("hung initialize = %d, want 502", st)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hung stdio child parked the initialize call (timeout not honored)")
	}
	// The failed handshake reaped the child: no registry entry lingers.
	p.mcpStdio.mu.Lock()
	n := len(p.mcpStdio.conns)
	p.mcpStdio.mu.Unlock()
	if n != 0 {
		t.Fatalf("hung child left registered: %d entries", n)
	}
}

// startAppHelperConn spawns the raw helper child (registry-level tests).
func startAppHelperConn(t *testing.T) *mcpkg.StdioConn {
	t.Helper()
	conn, err := mcpkg.StartStdio(
		[]string{os.Args[0], "-test.run=TestHelperProcess"},
		[]string{"MCP_STDIO_APP_HELPER=1", "PATH=" + os.Getenv("PATH")},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(conn.Close)
	return conn
}

// TestMCPStdioRegistry_PutOverwriteClosesOld: a concurrent ensure race puts
// two children under one key — the replaced conn must be Closed (reaped),
// not leaked. Regression for put overwriting without Close.
func TestMCPStdioRegistry_PutOverwriteClosesOld(t *testing.T) {
	reg := newMCPStdioRegistry()
	first := startAppHelperConn(t)
	second := startAppHelperConn(t)
	if err := reg.put("k", "vision", first); err != nil {
		t.Fatal(err)
	}
	if err := reg.put("k", "vision", second); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Call(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("overwritten conn still usable: %v", err)
	}
	got, ok := reg.get("k")
	if !ok || got != second {
		t.Fatal("registry did not keep the replacement conn")
	}
	resp, err := second.Call(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	if err != nil || !strings.Contains(string(resp), "fake-stdio-backend") {
		t.Fatalf("replacement conn broken: %v %s", err, resp)
	}
}

// TestMCPRoute_StdioEnsureConcurrentSingleChild: concurrent first calls race
// the lazy stdio sub-session build — exactly one child survives in the
// registry (the losers are reaped by put-overwrite Close).
func TestMCPRoute_StdioEnsureConcurrentSingleChild(t *testing.T) {
	servers := map[string]configdomain.MCPServer{"vision": stdioServerConfig(t)}
	routes := map[string]configdomain.MCPRoute{
		"vision-route": {Targets: []configdomain.MCPRouteTarget{
			{Server: "vision", Tools: map[string]string{"analyze": "image_analysis"}},
		}},
	}
	p, srv := newMCPRouteTestProxyP(t, []string{"k-A"}, servers, routes)
	resp := mcpPost(t, srv.URL+"/mcp/vision-route", "", routeInitBody)
	sid := resp.Header.Get("Mcp-Session-Id")
	resp.Body.Close()
	if sid == "" {
		t.Fatal("no route session minted")
	}
	const n = 4
	var wg sync.WaitGroup
	errs := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Distinct JSON-RPC ids: concurrent calls share the child conn and
			// in-flight ids are per-conn unique.
			callBody := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"analyze","arguments":{"q":"x"}}}`, 100+i)
			req, err := http.NewRequest(http.MethodPost, srv.URL+"/mcp/vision-route", strings.NewReader(callBody))
			if err != nil {
				errs <- err.Error()
				return
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			req.Header.Set("Mcp-Session-Id", sid)
			r, err := http.DefaultClient.Do(req)
			if err != nil {
				errs <- err.Error()
				return
			}
			body := mustRead(r)
			if r.StatusCode != http.StatusOK || !strings.Contains(body, "stdio:image_analysis") {
				errs <- fmt.Sprintf("status=%d body=%s", r.StatusCode, body)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	p.mcpStdio.mu.Lock()
	count := 0
	for k := range p.mcpStdio.conns {
		if strings.HasPrefix(k, sid+"\x00") {
			count++
		}
	}
	p.mcpStdio.mu.Unlock()
	if count != 1 {
		t.Fatalf("stdio children for one route session = %d, want 1 (raced children leaked)", count)
	}
}
