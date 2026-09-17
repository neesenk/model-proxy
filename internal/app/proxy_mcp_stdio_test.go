package app

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	configdomain "model-proxy/internal/config"
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
// binary as child with ${account.api_key} templated into CHILD_KEY.
func stdioServerConfig() configdomain.MCPServer {
	return configdomain.MCPServer{
		Transport: "stdio",
		Provider:  "zhipu",
		Command:   []string{os.Args[0], "-test.run=TestHelperProcess"},
		Env: map[string]string{
			"MCP_STDIO_APP_HELPER": "1",
			"CHILD_KEY":            "${account.api_key}",
		},
	}
}

// TestMCPGateway_StdioPinnedFlow: initialize spawns the child and mints a
// session; calls go through the child with the templated credential; DELETE
// kills the child and drops the session.
func TestMCPGateway_StdioPinnedFlow(t *testing.T) {
	srv := newMCPTestProxy(t, "zhipu", []string{"k-A"}, map[string]configdomain.MCPServer{
		"vision": stdioServerConfig(),
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
		MCP: map[string]configdomain.MCPServer{"vision": stdioServerConfig()},
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
		"vision": stdioServerConfig(),
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
		MCP: map[string]configdomain.MCPServer{"vision": stdioServerConfig()},
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
