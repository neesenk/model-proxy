package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"

	configdomain "model-proxy/internal/config"
)

// r6_mcp_stdio_freeze_test.go pins the operator-freeze contract on the pinned
// stdio path (R6 F2): a transport:stdio session whose bound account is frozen
// by an operator — or vanishes from a later generation — must fail closed with
// 410 and take its child process with it. The child carries ${account.api_key}
// in its environment, so keeping it alive keeps the credential in use.

// TestR6StdioHelper doubles as the stdio child of the r6 tests: with
// MCP_STDIO_R6_HELPER=1 it serves newline-delimited JSON-RPC on stdin/stdout.
// tools/call reports its pid, call count and the account key it was spawned
// with, except for the tool "boom", which is answered with a JSON-RPC error.
func TestR6StdioHelper(t *testing.T) {
	if os.Getenv("MCP_STDIO_R6_HELPER") != "1" {
		t.Skip("not a helper subprocess")
	}
	os.Exit(r6StdioHelperMain())
}

func r6StdioHelperMain() int {
	dec := json.NewDecoder(os.Stdin)
	enc := json.NewEncoder(os.Stdout)
	key := os.Getenv("CHILD_KEY")
	calls := 0
	for {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params *struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		if err := dec.Decode(&req); err != nil {
			return 0 // stdin closed: the session is over
		}
		if len(req.ID) == 0 {
			continue // notification: never answered
		}
		msg := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		switch {
		case req.Method == "tools/call" && req.Params != nil && req.Params.Name == "boom":
			msg["error"] = map[string]any{"code": -32602, "message": "invalid params: boom"}
		case req.Method == "initialize":
			msg["result"] = map[string]any{
				"protocolVersion": "2025-03-26",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "r6-stdio", "version": "1.0"},
			}
		case req.Method == "tools/call":
			calls++
			msg["result"] = map[string]any{"content": []map[string]string{{"type": "text",
				"text": fmt.Sprintf("r6-stdio key=%s pid=%d calls=%d", key, os.Getpid(), calls)}}}
		default:
			msg["result"] = map[string]any{}
		}
		if err := enc.Encode(msg); err != nil {
			return 1
		}
	}
}

// r6StdioServer builds a provider-backed transport:stdio server running this
// test binary as the child with the picked account's key in CHILD_KEY. Its
// on-disk twin for Reload is r6VisionYAML.
func r6StdioServer() configdomain.MCPServer {
	return configdomain.MCPServer{
		Transport: "stdio",
		Provider:  "zhipu",
		Command:   []string{os.Args[0], "-test.run=^TestR6StdioHelper$"},
		Env: map[string]string{
			"MCP_STDIO_R6_HELPER": "env:MCP_STDIO_R6_HELPER",
			"CHILD_KEY":           "${account.api_key}",
		},
	}
}

// r6ToolResult is what the helper child reports for one tools/call.
type r6ToolResult struct {
	Key   string
	PID   int
	Calls int
}

// r6ChildReport reads the helper child's report out of one tools/call response
// body. false for an error envelope or a plain-text terminal answer.
func r6ChildReport(t *testing.T, body string) (r6ToolResult, bool) {
	t.Helper()
	var rpc struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	var out r6ToolResult
	if err := json.Unmarshal([]byte(body), &rpc); err != nil || rpc.Error != nil || len(rpc.Result.Content) == 0 {
		return out, false
	}
	for _, field := range strings.Fields(rpc.Result.Content[0].Text) {
		k, v, _ := strings.Cut(field, "=")
		switch k {
		case "key":
			out.Key = v
		case "pid", "calls":
			n, err := strconv.Atoi(v)
			if err != nil {
				t.Fatalf("child report %q: %v", rpc.Result.Content[0].Text, err)
			}
			if k == "pid" {
				out.PID = n
			} else {
				out.Calls = n
			}
		}
	}
	if out.Key == "" || out.PID == 0 {
		t.Fatalf("child report lacks key/pid: %q", rpc.Result.Content[0].Text)
	}
	return out, true
}

// r6CallTools posts one tools/call on the pinned /mcp/vision session and
// returns the response status, the raw body and the child's report (zero
// value unless the call was served by a child).
func r6CallTools(t *testing.T, srv *httptest.Server, sid string) (int, string, r6ToolResult) {
	t.Helper()
	resp := mcpPost(t, srv.URL+"/mcp/vision", sid, routeCallBody("image_analysis"))
	body := mustRead(resp)
	out, _ := r6ChildReport(t, body)
	return resp.StatusCode, body, out
}

// r6Initialize establishes a pinned stdio session and returns its local id.
func r6Initialize(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	resp := mcpPost(t, srv.URL+"/mcp/vision", "", mcpInitBody)
	body := mustRead(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize = %d %q", resp.StatusCode, body)
	}
	sid := resp.Header.Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatal("no session minted for stdio server")
	}
	return sid
}

// r6ProcessAlive reports whether the pid still answers Signal(0) (Unix; a
// reaped child no longer does).
func r6ProcessAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// r6OpenSession initializes a pinned stdio session, drives one tools/call and
// returns the local session id with the child's report.
func r6OpenSession(t *testing.T, srv *httptest.Server, pidProbe bool) (string, r6ToolResult) {
	t.Helper()
	sid := r6Initialize(t, srv)
	status, body, first := r6CallTools(t, srv, sid)
	if status != http.StatusOK || first.Calls != 1 {
		t.Fatalf("first call = %d %q %+v, want the child's first answer", status, body, first)
	}
	r6RequireChildAlive(t, first.PID, pidProbe)
	return sid, first
}

// r6RequireSessionGone asserts the "no session" semantics on the old id and
// — where pids can be probed — that the child process was reaped.
func r6RequireSessionGone(t *testing.T, srv *httptest.Server, sid string, child r6ToolResult, pidProbe bool) {
	t.Helper()
	status, body, _ := r6CallTools(t, srv, sid)
	if status != http.StatusNotFound || !strings.Contains(body, "unknown or expired session") {
		t.Fatalf("call on the dropped session = %d %q, want 404 unknown or expired session", status, body)
	}
	r6RequireChildReaped(t, child.PID, "dropped session's child process to be reaped", pidProbe)
}

// r6OtherKey returns the pool key that is not the given one (the freeze
// harness installs exactly k-A and k-B).
func r6OtherKey(t *testing.T, key string) string {
	t.Helper()
	switch key {
	case "k-A":
		return "k-B"
	case "k-B":
		return "k-A"
	}
	t.Fatalf("unexpected bound credential %q", key)
	return ""
}

// TestMCPGateway_StdioFrozenSessionAccountFailsClosed410: a pinned stdio
// session bound to a later-frozen account is dropped with 410 naming the
// freeze and its child is killed — the credential in the child's environment
// stops being used; the client re-initializes onto the healthy sibling.
// Regression for the stdio path dispatching before the pinned HTTP freeze
// check.
func TestMCPGateway_StdioFrozenSessionAccountFailsClosed410(t *testing.T) {
	pidProbe := r6PidProbe()
	t.Setenv("MCP_STDIO_R6_HELPER", "1")
	p, srv := newFreezeTestProxy(t, map[string]configdomain.MCPServer{"vision": r6StdioServer()})
	sid, first := r6OpenSession(t, srv, pidProbe)
	other := r6OtherKey(t, first.Key)

	freezeAccountForKey(t, p, first.Key)
	status, body, _ := r6CallTools(t, srv, sid)
	if status != http.StatusGone || strings.TrimSpace(body) != mcpSessionAccountFrozenMsg {
		t.Fatalf("call after freezing the bound account = %d %q, want 410 %q", status, body, mcpSessionAccountFrozenMsg)
	}
	r6RequireSessionGone(t, srv, sid, first, pidProbe)

	// Recovery: the client re-initializes onto the schedulable sibling.
	resid := r6Initialize(t, srv)
	status, body, rebound := r6CallTools(t, srv, resid)
	if status != http.StatusOK || rebound.Key != other || rebound.PID == first.PID {
		t.Fatalf("re-initialize call = %d %q %+v, want a fresh child on the healthy account", status, body, rebound)
	}
}

// TestMCPGateway_StdioSiblingFreezeKeepsBinding: freezing a DIFFERENT pool
// account must not break the stdio session — only the session's own account
// does (the freeze check is a per-binding verdict, not a pool-wide one).
func TestMCPGateway_StdioSiblingFreezeKeepsBinding(t *testing.T) {
	t.Setenv("MCP_STDIO_R6_HELPER", "1")
	p, srv := newFreezeTestProxy(t, map[string]configdomain.MCPServer{"vision": r6StdioServer()})
	sid, first := r6OpenSession(t, srv, r6PidProbe())
	freezeAccountForKey(t, p, r6OtherKey(t, first.Key))
	status, body, kept := r6CallTools(t, srv, sid)
	if status != http.StatusOK || kept != (r6ToolResult{Key: first.Key, PID: first.PID, Calls: 2}) {
		t.Fatalf("call after freezing a sibling account = %d %q %+v, want the same child and key on its 2nd call", status, body, kept)
	}
}

// TestMCPGateway_StdioSessionAccountRemovedFailsClosed: after a reload drops
// the session's account from the pool, requests on the old stdio session fail
// closed (410) and the child is reaped — the same contract the HTTP pinned
// path already has for a removed account.
func TestMCPGateway_StdioSessionAccountRemovedFailsClosed(t *testing.T) {
	pidProbe := r6PidProbe()
	t.Setenv("MCP_STDIO_R6_HELPER", "1")
	silenceLog(t) // before the proxy is built: the reload's quota re-sync logs on a muted logger (see r6Logged)
	p, srv := newFreezeTestProxy(t, map[string]configdomain.MCPServer{"vision": r6StdioServer()})
	sid, first := r6OpenSession(t, srv, pidProbe)

	r6Reload(t, p, r6ConfigYAML(r6VisionYAML(), ""), r6OtherKey(t, first.Key))
	status, body, _ := r6CallTools(t, srv, sid)
	if status != http.StatusGone || strings.TrimSpace(body) != mcpSessionAccountGoneMsg {
		t.Fatalf("call after the account was removed = %d %q, want 410 %q", status, body, mcpSessionAccountGoneMsg)
	}
	r6RequireSessionGone(t, srv, sid, first, pidProbe)
}
