package app

import (
	"bufio"
	"bytes"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/observe/requestlog"
)

// TestHelperProcessStdioDies doubles as a stdio backend that dies while
// answering initialize — bad key, missing runtime — echoing the key it was
// handed on its way out: exactly what the failure diagnostics must mask.
func TestHelperProcessStdioDies(t *testing.T) {
	if os.Getenv("MCP_STDIO_APP_DIES") != "1" {
		t.Skip("not a helper subprocess")
	}
	if !bufio.NewScanner(os.Stdin).Scan() {
		os.Exit(2)
	}
	fmt.Fprintf(os.Stderr, "boot failed: key=%s\n    at boot (server.js:2:3)\n", os.Getenv("CHILD_KEY"))
	os.Exit(2)
}

// newMCPEvidenceProxy wires a request logger behind an MCP gateway test
// server and captures serve.log, so one test reads every surface of an
// exchange. The returned reader hands back both (log text, request-log rows);
// call it once, after the exchange returned.
func newMCPEvidenceProxy(t *testing.T, provider string, keys []string, servers map[string]configdomain.MCPServer, routes map[string]configdomain.MCPRoute) (*httptest.Server, func() (string, []requestlog.Record)) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	writeMCPKeys(t, provider, keys...)
	cfg := &configdomain.Config{
		Listen: "127.0.0.1:0",
		Providers: map[string]configdomain.Provider{
			provider: {Provider: provider, OpenAIBaseURL: "http://127.0.0.1:1", Models: []string{"m"}},
		},
		MCP:       servers,
		MCPRoutes: routes,
	}
	proxy, dir, shutdown := newReqLogProxy(t, cfg)
	srv := httptest.NewServer(http.HandlerFunc(proxy.Handler))
	t.Cleanup(srv.Close)
	var logs bytes.Buffer
	original := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(original) })
	return srv, func() (string, []requestlog.Record) {
		log.SetOutput(original)
		shutdown()
		return logs.String(), allRecords(t, dir)
	}
}

// TestMCPGateway_FailureLeavesEvidence: an exchange the gateway answers with
// its own 5xx must say why on every surface. Before the fix the reason
// travelled only in the HTTP answer — serve.log stayed silent (no MCP line at
// all) and the request-log row recorded the status with an empty body, so the
// row read "502, 0 bytes" (2026-11-25: three /mcp/vision initialize failures
// whose cause, npx missing from PATH, was recoverable only by reconstructing
// Homebrew symlink mtimes). A dead stdio child additionally reports its wait
// status, and its stderr reaches serve.log redacted: the HTTP answer never
// carries child stderr, and the credential the child was handed is masked
// wherever the tail does appear.
func TestMCPGateway_FailureLeavesEvidence(t *testing.T) {
	const key = "sk-fake-vision-key-0123456789abcdef"
	unreachable := configdomain.MCPServer{Provider: "zhipu", URL: "http://127.0.0.1:1/mcp"}
	dyingChild := func(env map[string]string) configdomain.MCPServer {
		return configdomain.MCPServer{
			Transport: "stdio",
			Provider:  "zhipu",
			Command:   []string{os.Args[0], "-test.run=TestHelperProcessStdioDies"},
			Env:       env,
		}
	}
	t.Setenv("MCP_STDIO_APP_DIES", "1")
	plain := func(path, body string) func(*testing.T, *httptest.Server) (int, string) {
		return func(t *testing.T, srv *httptest.Server) (int, string) {
			resp := mcpPost(t, srv.URL+path, "", body)
			return resp.StatusCode, mustRead(resp)
		}
	}
	// routeCall initializes the aggregated route (the proxy's own answer),
	// then calls a tool only the dead backend declares.
	routeCall := func(t *testing.T, srv *httptest.Server) (int, string) {
		init := mcpPost(t, srv.URL+"/mcp/agg", "", routeInitBody)
		sid := init.Header.Get("Mcp-Session-Id")
		mustRead(init)
		if sid == "" {
			t.Fatal("route initialize minted no session")
		}
		resp := mcpPost(t, srv.URL+"/mcp/agg", sid, routeCallBody("web_search"))
		return resp.StatusCode, mustRead(resp)
	}

	cases := []struct {
		name    string // the /mcp/<name> endpoint
		label   string // the failure class under test
		servers map[string]configdomain.MCPServer
		routes  map[string]configdomain.MCPRoute
		do      func(*testing.T, *httptest.Server) (int, string)
		cause   string // named by the client answer AND serve.log
		method  string // the exchange serve.log names
		stderr  string // child stderr: serve.log only, never the client answer
	}{
		{
			name:  "vision",
			label: "stdio child cannot start",
			servers: map[string]configdomain.MCPServer{"vision": {
				Transport: "stdio", Provider: "zhipu", Command: []string{"model-proxy-no-such-mcp-child"},
			}},
			do:     plain("/mcp/vision", mcpInitBody),
			cause:  `mcp "vision": stdio start:`,
			method: "initialize",
		},
		{
			name:  "vision",
			label: "stdio child dies at initialize",
			servers: map[string]configdomain.MCPServer{"vision": dyingChild(map[string]string{
				"MCP_STDIO_APP_DIES": "env:MCP_STDIO_APP_DIES",
				"CHILD_KEY":          "${account.api_key}",
			})},
			do:     plain("/mcp/vision", mcpInitBody),
			cause:  `mcp "vision": stdio initialize: stdio: child exited (exit status 2)`,
			method: "initialize",
			stderr: "boot failed",
		},
		{
			name:    "dp",
			label:   "pinned upstream unreachable",
			servers: map[string]configdomain.MCPServer{"dp": unreachable},
			do:      plain("/mcp/dp", mcpCallBody),
			cause:   `mcp "dp": upstream:`,
			method:  "tools/call",
		},
		{
			name:    "agg",
			label:   "route: every backend failed",
			servers: map[string]configdomain.MCPServer{"dead": unreachable},
			routes: map[string]configdomain.MCPRoute{"agg": {Targets: []configdomain.MCPRouteTarget{
				{Server: "dead", Tools: map[string]string{"web_search": "web_search"}},
			}}},
			do:     routeCall,
			cause:  `mcp route "agg": all 1 backend(s) failed for tool "web_search"`,
			method: "tools/call",
		},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			srv, evidence := newMCPEvidenceProxy(t, "zhipu", []string{key}, tc.servers, tc.routes)
			status, client := tc.do(t, srv)
			if status != http.StatusBadGateway {
				t.Fatalf("status = %d (body %q), want 502", status, client)
			}
			if !strings.Contains(client, tc.cause) {
				t.Fatalf("client answer = %q, want it to name the cause %q", client, tc.cause)
			}
			if strings.Contains(client, key) {
				t.Errorf("client answer carries the account key: %q", client)
			}
			if tc.stderr != "" && strings.Contains(client, tc.stderr) {
				t.Errorf("client answer carries child stderr: %q", client)
			}

			logs, records := evidence()
			var lines []string
			for _, line := range strings.Split(logs, "\n") {
				if strings.Contains(line, "[mcp "+tc.name+"]") {
					lines = append(lines, line)
				}
			}
			if len(lines) != 1 {
				t.Fatalf("serve.log lines naming [mcp %s] = %q, want exactly one failure line", tc.name, lines)
			}
			line := lines[0]
			for _, want := range []string{tc.method + " failed (status 502)", tc.cause, strings.TrimSpace(client)} {
				if !strings.Contains(line, want) {
					t.Errorf("serve.log line = %q, want it to carry %q", line, want)
				}
			}
			if strings.Contains(line, key) {
				t.Errorf("serve.log line carries the account key: %q", line)
			}
			if tc.stderr != "" {
				for _, want := range []string{tc.stderr, "<redacted:CHILD_KEY>"} {
					if !strings.Contains(line, want) {
						t.Errorf("serve.log line = %q, want the child's redacted stderr (%q)", line, want)
					}
				}
			}

			var rows []requestlog.Record
			for _, rec := range records {
				if rec.Kind == "mcp" {
					rows = append(rows, rec)
				}
			}
			wantRows := 1
			if tc.name == "agg" {
				wantRows = 2 // the route's own initialize answer, then the failed call
			}
			if len(rows) != wantRows {
				t.Fatalf("mcp request-log rows = %+v, want %d", rows, wantRows)
			}
			// The failed exchange is found by its method, never by position.
			// QueryRecords orders rows by the second-resolution ts (newest
			// first): with the route's initialize answer and the failed call in
			// different seconds the two swap places, and "the last row" was the
			// 200 of the initialize (a one-in-N flake whenever the exchanges
			// straddled a second boundary).
			var failed *requestlog.Record
			for i := range rows {
				if rows[i].Method == tc.method {
					failed = &rows[i]
				}
			}
			if failed == nil {
				t.Fatalf("mcp request-log rows = %+v, want one for the failed %s exchange", rows, tc.method)
			}
			if failed.Status != http.StatusBadGateway {
				t.Errorf("request-log row status = %d, want 502", failed.Status)
			}
			if failed.ResponseBody != client || failed.ResponseSize != int64(len(client)) {
				t.Errorf("request-log row response = %q (%d bytes), want exactly the client bytes %q (%d)", failed.ResponseBody, failed.ResponseSize, client, len(client))
			}
		})
	}
}
