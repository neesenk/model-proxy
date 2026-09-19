package app

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	configdomain "model-proxy/internal/config"
	observeevents "model-proxy/internal/observe/events"
)

// ---- MCP gateway test harness ----

// mcpHit records one upstream exchange the fake server saw.
type mcpHit struct {
	method  string // JSON-RPC method ("" for GET/DELETE)
	auth    string
	planKey string // X-Agent-Plan-Key
	session string
	path    string
}

// fakeMCPUpstream is a streamable-HTTP MCP server double. It answers the
// handshake (optionally issuing Mcp-Session-Id), notifications, tools/call,
// DELETE, and can fail chosen keys with 401.
type fakeMCPUpstream struct {
	mu        sync.Mutex
	hits      []mcpHit
	bodies    []string
	sessionID string         // "" = stateless server (never issues a session)
	failKeys  map[string]int // Authorization value → remaining 401 count
	sseInit   bool
	// reinitSessionID, when set, replaces sessionID on initialize requests that
	// ALREADY carry an upstream session id (upstream-side id rotation).
	reinitSessionID string
}

func (f *fakeMCPUpstream) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	auth := r.Header.Get("Authorization")
	hit := mcpHit{auth: auth, planKey: r.Header.Get("X-Agent-Plan-Key"), session: r.Header.Get("Mcp-Session-Id"), path: r.URL.Path}
	if n := f.failKeys[auth]; n > 0 && auth != "" {
		f.failKeys[auth] = n - 1
		f.hits = append(f.hits, hit)
		f.mu.Unlock()
		http.Error(w, "bad key", http.StatusUnauthorized)
		return
	}
	f.mu.Unlock()

	if r.Method == http.MethodGet {
		f.record(hit)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, ": heartbeat\n\n")
		return
	}
	if r.Method == http.MethodDelete {
		f.record(hit)
		w.WriteHeader(http.StatusOK)
		return
	}
	body := readAll(r)
	frame := parseRPCMethod(body)
	hit.method = frame
	f.record(hit)
	f.mu.Lock()
	f.bodies = append(f.bodies, string(body))
	f.mu.Unlock()
	switch frame {
	case "initialize":
		sid := f.sessionID
		if f.reinitSessionID != "" && hit.session != "" {
			sid = f.reinitSessionID
		}
		if sid != "" {
			w.Header().Set("Mcp-Session-Id", sid)
		}
		payload := `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26","capabilities":{},"serverInfo":{"name":"fake","version":"0.1"}}}`
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", payload)
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	case "tools/call":
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", `{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"done"}]}}`)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeMCPUpstream) record(h mcpHit) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits = append(f.hits, h)
}

func (f *fakeMCPUpstream) auths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.hits))
	for i, h := range f.hits {
		out[i] = h.auth
	}
	return out
}

func readAll(r *http.Request) []byte {
	buf := &strings.Builder{}
	chunk := make([]byte, 4096)
	for {
		n, err := r.Body.Read(chunk)
		buf.Write(chunk[:n])
		if err != nil {
			break
		}
	}
	return []byte(buf.String())
}

func parseRPCMethod(body []byte) string {
	var v struct {
		Method string `json:"method"`
	}
	json.Unmarshal(body, &v)
	return v.Method
}

// writeMCPKeys installs credentials under a temp HOME as a credential POOL
// (the plural file — 1 entry binds the plain provider name, >=2 entries
// unroll into virtual providers). Rewriting with fewer keys replaces the
// pool, which the reload tests rely on.
func writeMCPKeys(t *testing.T, provider string, keys ...string) {
	t.Helper()
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".model-proxy")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString(`{"version":1,"accounts":[`)
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"id":%q,"label":%q,"api_key":%q,"added_at":"2026-01-01T00:00:00Z"}`,
			[]string{"aaa", "bbb", "ccc"}[i], fmt.Sprintf("acct-%d", i), k)
	}
	b.WriteString(`]}`)
	path := filepath.Join(dir, provider+"_apikeys.json")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

// newMCPTestProxy builds a Proxy over a temp HOME with one provider whose
// base URLs point at a dummy (LLM traffic is never exercised), plus the given
// mcp: servers. Returns the client-facing test server.
func newMCPTestProxy(t *testing.T, provider string, keys []string, servers map[string]configdomain.MCPServer) *httptest.Server {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	writeMCPKeys(t, provider, keys...)
	cfg := &configdomain.Config{
		Listen: "127.0.0.1:0",
		Providers: map[string]configdomain.Provider{
			provider: {Provider: provider, OpenAIBaseURL: "http://127.0.0.1:1", Models: []string{"m"}},
		},
		MCP: servers,
	}
	p := newTestProxy(t, cfg)
	srv := httptest.NewServer(http.HandlerFunc(p.Handler))
	t.Cleanup(srv.Close)
	return srv
}

func mcpPost(t *testing.T, url, session, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
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
	return resp
}

const mcpInitBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`
const mcpCallBody = `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search","arguments":{"q":"x"}}}`
const mcpNotifBody = `{"jsonrpc":"2.0","method":"notifications/initialized"}`

// ---- tests ----

// TestMCPGateway_SessionPinsAccount: with a two-account pool, initialize
// picks one account, the upstream session id is rewritten to a local id, and
// every subsequent request carrying the local id reaches the SAME account
// with the UPSTREAM session id restored.
func TestMCPGateway_SessionPinsAccount(t *testing.T) {
	up := &fakeMCPUpstream{sessionID: "upstream-sid-1"}
	upSrv := httptest.NewServer(http.HandlerFunc(up.serve))
	t.Cleanup(upSrv.Close)

	srv := newMCPTestProxy(t, "zhipu", []string{"k-A", "k-B"}, map[string]configdomain.MCPServer{
		"zs": {Provider: "zhipu", URL: upSrv.URL},
	})

	resp := mcpPost(t, srv.URL+"/mcp/zs", "", mcpInitBody)
	if resp.StatusCode != 200 {
		t.Fatalf("initialize = %d", resp.StatusCode)
	}
	localSID := resp.Header.Get("Mcp-Session-Id")
	if localSID == "" || localSID == "upstream-sid-1" {
		t.Fatalf("local session id = %q (must be minted, not the upstream id)", localSID)
	}
	body, _ := readBodyString(resp)
	if !strings.Contains(body, `"serverInfo"`) {
		t.Fatalf("initialize body not passed through: %q", body)
	}
	initAuth := up.auths()[0]

	// Notification + call on the local session.
	if r := mcpPost(t, srv.URL+"/mcp/zs", localSID, mcpNotifBody); r.StatusCode != http.StatusAccepted {
		t.Fatalf("notification = %d", r.StatusCode)
	}
	resp = mcpPost(t, srv.URL+"/mcp/zs", localSID, mcpCallBody)
	if resp.StatusCode != 200 || !strings.Contains(mustRead(resp), `"done"`) {
		t.Fatalf("tools/call failed: %d", resp.StatusCode)
	}

	hits := up.auths()
	for i, a := range hits {
		if a != initAuth {
			t.Fatalf("hit %d auth = %q, want session-pinned %q", i, a, initAuth)
		}
	}
	up.mu.Lock()
	defer up.mu.Unlock()
	lastCall := up.hits[len(up.hits)-1]
	if lastCall.session != "upstream-sid-1" {
		t.Fatalf("upstream session = %q, want rewritten upstream-sid-1", lastCall.session)
	}
	if lastCall.method != "tools/call" {
		t.Fatalf("method = %q", lastCall.method)
	}
}

// TestMCPGateway_StatelessUpstreamNoSession: an upstream that never issues a
// session id (Firecrawl shape) gets no local session minted, and sessionless
// calls spread round-robin across the pool.
func TestMCPGateway_StatelessUpstreamNoSession(t *testing.T) {
	up := &fakeMCPUpstream{} // stateless
	upSrv := httptest.NewServer(http.HandlerFunc(up.serve))
	t.Cleanup(upSrv.Close)

	srv := newMCPTestProxy(t, "zhipu", []string{"k-A", "k-B"}, map[string]configdomain.MCPServer{
		"fc": {Provider: "zhipu", URL: upSrv.URL},
	})
	resp := mcpPost(t, srv.URL+"/mcp/fc", "", mcpInitBody)
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		t.Fatalf("stateless upstream got local session %q", sid)
	}
	resp.Body.Close()
	// Two stateless calls → round-robin over both accounts.
	mcpPost(t, srv.URL+"/mcp/fc", "", mcpCallBody).Body.Close()
	mcpPost(t, srv.URL+"/mcp/fc", "", mcpCallBody).Body.Close()
	auths := up.auths()
	seen := map[string]bool{}
	for _, a := range auths {
		seen[a] = true
	}
	if len(seen) != 2 {
		t.Fatalf("round-robin did not reach both accounts: %v", auths)
	}
}

// TestMCPGateway_AuthNoneServer: anonymous public servers inject no
// credential and need no provider.
func TestMCPGateway_AuthNoneServer(t *testing.T) {
	up := &fakeMCPUpstream{}
	upSrv := httptest.NewServer(http.HandlerFunc(up.serve))
	t.Cleanup(upSrv.Close)

	srv := newMCPTestProxy(t, "zhipu", []string{"k-A"}, map[string]configdomain.MCPServer{
		"exa": {URL: upSrv.URL, Auth: "none"},
	})
	resp := mcpPost(t, srv.URL+"/mcp/exa", "", mcpCallBody)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	resp.Body.Close()
	if got := up.auths()[0]; got != "" {
		t.Fatalf("auth: none leaked credential %q", got)
	}
}

// TestMCPGateway_CustomAuthHeader: auth_header injects the RAW key under the
// custom header (volcengine X-Agent-Plan-Key shape), not Authorization.
func TestMCPGateway_CustomAuthHeader(t *testing.T) {
	up := &fakeMCPUpstream{}
	upSrv := httptest.NewServer(http.HandlerFunc(up.serve))
	t.Cleanup(upSrv.Close)

	srv := newMCPTestProxy(t, "volcengine", []string{"plan-key-1"}, map[string]configdomain.MCPServer{
		"dp": {Provider: "volcengine", URL: upSrv.URL, AuthHeader: "X-Agent-Plan-Key"},
	})
	resp := mcpPost(t, srv.URL+"/mcp/dp", "", mcpCallBody)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	resp.Body.Close()
	up.mu.Lock()
	defer up.mu.Unlock()
	hit := up.hits[0]
	if hit.planKey != "plan-key-1" {
		t.Fatalf("X-Agent-Plan-Key = %q", hit.planKey)
	}
	if hit.auth != "" {
		t.Fatalf("Authorization must not be set for custom-header auth, got %q", hit.auth)
	}
}

// TestMCPGateway_401RotatesOnce: a sessionless 401 retries once on the next
// pool account. Deterministic: pool ids sort aaa<bbb, RR starts at 0 → first
// call lands on #bbb (k-B, good), the second on #aaa (k-A, poisoned once)
// and must rotate back to #bbb.
func TestMCPGateway_401RotatesOnce(t *testing.T) {
	up := &fakeMCPUpstream{failKeys: map[string]int{"Bearer k-A": 1}}
	upSrv := httptest.NewServer(http.HandlerFunc(up.serve))
	t.Cleanup(upSrv.Close)

	srv := newMCPTestProxy(t, "zhipu", []string{"k-A", "k-B"}, map[string]configdomain.MCPServer{
		"zs": {Provider: "zhipu", URL: upSrv.URL},
	})
	if r := mcpPost(t, srv.URL+"/mcp/zs", "", mcpCallBody); r.StatusCode != 200 {
		t.Fatalf("call 1 = %d", r.StatusCode)
	}
	resp := mcpPost(t, srv.URL+"/mcp/zs", "", mcpCallBody)
	if resp.StatusCode != 200 {
		t.Fatalf("call 2 (after rotation) = %d", resp.StatusCode)
	}
	resp.Body.Close()
	counts := map[string]int{}
	for _, a := range up.auths() {
		counts[a]++
	}
	if counts["Bearer k-A"] != 1 {
		t.Fatalf("poisoned key hit %d times, want exactly 1: %v", counts["Bearer k-A"], up.auths())
	}
	if counts["Bearer k-B"] != 2 {
		t.Fatalf("good key hit %d times, want 2: %v", counts["Bearer k-B"], up.auths())
	}
}

// TestMCPGateway_NotFoundDisabledAndMethods: unknown names, disabled servers
// and wrong methods are terminal client errors, not LLM 502s.
func TestMCPGateway_NotFoundDisabledAndMethods(t *testing.T) {
	up := &fakeMCPUpstream{}
	upSrv := httptest.NewServer(http.HandlerFunc(up.serve))
	t.Cleanup(upSrv.Close)

	disabled := false
	srv := newMCPTestProxy(t, "zhipu", []string{"k-A"}, map[string]configdomain.MCPServer{
		"on":  {Provider: "zhipu", URL: upSrv.URL},
		"off": {Provider: "zhipu", URL: upSrv.URL, Enabled: &disabled},
	})
	if r := mcpPost(t, srv.URL+"/mcp/ghost", "", mcpCallBody); r.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown = %d", r.StatusCode)
	}
	if r := mcpPost(t, srv.URL+"/mcp/off", "", mcpCallBody); r.StatusCode != http.StatusNotFound {
		t.Fatalf("disabled = %d", r.StatusCode)
	}
	if r := mcpPost(t, srv.URL+"/mcp/on/extra", "", mcpCallBody); r.StatusCode != http.StatusNotFound {
		t.Fatalf("nested path = %d", r.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/mcp/on", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("PUT = %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestMCPGateway_ForwardAuthGate: /mcp/ sits behind the same api-keys gate as
// the LLM forward surface.
func TestMCPGateway_ForwardAuthGate(t *testing.T) {
	up := &fakeMCPUpstream{}
	upSrv := httptest.NewServer(http.HandlerFunc(up.serve))
	t.Cleanup(upSrv.Close)

	t.Setenv("HOME", t.TempDir())
	writeMCPKeys(t, "zhipu", "k-A")
	dir, _ := os.UserHomeDir()
	keysFile := filepath.Join(dir, ".model-proxy", "api_keys")
	if err := os.WriteFile(keysFile, []byte("sk-team-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &configdomain.Config{
		Listen: "127.0.0.1:0",
		Providers: map[string]configdomain.Provider{
			"zhipu": {Provider: "zhipu", OpenAIBaseURL: "http://127.0.0.1:1", Models: []string{"m"}},
		},
		Web: configdomain.WebConfig{Auth: configdomain.WebAuthConfig{APIKeysFile: keysFile}},
		MCP: map[string]configdomain.MCPServer{"zs": {Provider: "zhipu", URL: upSrv.URL}},
	}
	p := newTestProxy(t, cfg)
	srv := httptest.NewServer(http.HandlerFunc(p.Handler))
	t.Cleanup(srv.Close)

	if r := mcpPost(t, srv.URL+"/mcp/zs", "", mcpCallBody); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no key = %d, want 401", r.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp/zs", strings.NewReader(mcpCallBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer sk-team-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("with key = %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestMCPGateway_SSEStreamsUnbuffered: the first upstream SSE frame must
// reach the client while the upstream is still holding the response open —
// proving the gateway flushes instead of buffering to EOF.
func TestMCPGateway_SSEStreamsUnbuffered(t *testing.T) {
	proceed := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl, _ := w.(http.Flusher)
		fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n\n")
		fl.Flush()
		<-proceed // hold the stream open; the first frame is already out
		fmt.Fprintf(w, "data: {}\n\n")
	}))
	t.Cleanup(up.Close)

	srv := newMCPTestProxy(t, "zhipu", []string{"k-A"}, map[string]configdomain.MCPServer{
		"zs": {Provider: "zhipu", URL: up.URL, Timeout: "10s"},
	})
	resp := mcpPost(t, srv.URL+"/mcp/zs", "", mcpInitBody)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	defer resp.Body.Close()
	// Bounded read: if the gateway buffered, this deadline expires.
	type result struct {
		line string
		err  error
	}
	got := make(chan result, 1)
	go func() {
		line, err := bufio.NewReader(resp.Body).ReadString('\n')
		got <- result{line, err}
	}()
	select {
	case r := <-got:
		if r.err != nil || !strings.Contains(r.line, "event:") && !strings.Contains(r.line, "data:") {
			t.Fatalf("first frame = %q err=%v", r.line, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first SSE frame did not stream through unbuffered")
	}
	close(proceed)
}

// TestMCPGateway_SessionAccountRemovedFailsClosed: after a reload that drops
// the session's account from the pool, requests on the old session fail
// closed (410) instead of drifting to another credential.
func TestMCPGateway_SessionAccountRemovedFailsClosed(t *testing.T) {
	up := &fakeMCPUpstream{sessionID: "up-1"}
	upSrv := httptest.NewServer(http.HandlerFunc(up.serve))
	t.Cleanup(upSrv.Close)

	home := t.TempDir()
	t.Setenv("HOME", home)
	writeMCPKeys(t, "zhipu", "k-A", "k-B") // pool: #aaa (k-A), #bbb (k-B)

	// Programmatic config for construction; YAML twin for Reload (which loads
	// from disk).
	cfg := &configdomain.Config{
		Listen: "127.0.0.1:0",
		Providers: map[string]configdomain.Provider{
			"zhipu": {Provider: "zhipu", OpenAIBaseURL: "http://127.0.0.1:1", Models: []string{"m"}},
		},
		MCP: map[string]configdomain.MCPServer{"zs": {Provider: "zhipu", URL: upSrv.URL}},
	}
	yamlPath := filepath.Join(home, "config.yaml")
	yaml := fmt.Sprintf(`listen: 127.0.0.1:0
providers:
  zhipu:
    provider_id: zhipu
    openai_base_url: http://127.0.0.1:1
    models: [m]
mcp:
  zs:
    provider: zhipu
    url: %s
`, upSrv.URL)
	if err := os.WriteFile(yamlPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	p := newTestProxy(t, cfg)
	srv := httptest.NewServer(http.HandlerFunc(p.Handler))
	t.Cleanup(srv.Close)

	// RR first pick lands wherever the stable account-id hashes sort; read the
	// bound credential from the upstream instead of assuming an order.
	resp := mcpPost(t, srv.URL+"/mcp/zs", "", mcpInitBody)
	localSID := resp.Header.Get("Mcp-Session-Id")
	resp.Body.Close()
	if localSID == "" {
		t.Fatal("no local session minted")
	}
	up.mu.Lock()
	boundAuth := up.hits[0].auth
	up.mu.Unlock()
	var remaining string
	switch boundAuth {
	case "Bearer k-A":
		remaining = "k-B"
	case "Bearer k-B":
		remaining = "k-A"
	default:
		t.Fatalf("unexpected bound credential %q", boundAuth)
	}

	// Drop the session's account from the pool and reload.
	writeMCPKeys(t, "zhipu", remaining)
	if err := p.Reload(yamlPath); err != nil {
		t.Fatalf("reload: %v", err)
	}
	resp = mcpPost(t, srv.URL+"/mcp/zs", localSID, mcpCallBody)
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("stale session = %d, want 410", resp.StatusCode)
	}
	resp.Body.Close()
	// The failed request must not have reached upstream with the other account.
	up.mu.Lock()
	defer up.mu.Unlock()
	for i := range up.hits {
		if i > 0 {
			t.Fatalf("stale session reached upstream after account removal: %+v", up.hits)
		}
	}
}

// TestMCPGateway_RequestLogKind: MCP exchanges land in the request log with
// kind="mcp", the JSON-RPC method, the /mcp/ path and the account, using the
// same JSONL sink as LLM traffic.
func TestMCPGateway_RequestLogKind(t *testing.T) {
	up := &fakeMCPUpstream{sessionID: "up-1"}
	upSrv := httptest.NewServer(http.HandlerFunc(up.serve))
	t.Cleanup(upSrv.Close)

	t.Setenv("HOME", t.TempDir())
	writeMCPKeys(t, "zhipu", "k-A")
	cfg := &configdomain.Config{
		Listen: "127.0.0.1:0",
		Providers: map[string]configdomain.Provider{
			"zhipu": {Provider: "zhipu", OpenAIBaseURL: "http://127.0.0.1:1", Models: []string{"m"}},
		},
		MCP: map[string]configdomain.MCPServer{"zs": {Provider: "zhipu", URL: upSrv.URL}},
	}
	p := newTestProxy(t, cfg)
	logDir := t.TempDir()
	p.initRequestLog(configdomain.RequestLogConfig{Enabled: true, Dir: logDir})
	if p.reqLog == nil {
		t.Fatal("request log not initialized")
	}
	p.reqLogStarted = p.lifecycle.Run(func(<-chan struct{}) { p.reqLog.Run() })
	srv := httptest.NewServer(http.HandlerFunc(p.Handler))
	t.Cleanup(srv.Close)

	resp := mcpPost(t, srv.URL+"/mcp/zs", "", mcpInitBody)
	localSID := resp.Header.Get("Mcp-Session-Id")
	resp.Body.Close()
	mcpPost(t, srv.URL+"/mcp/zs", localSID, mcpCallBody).Body.Close()

	// Bounded poll: the single-writer sink flushes on its own schedule.
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
		if strings.Count(content, `"kind":"mcp"`) >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("mcp records never landed in request log:\n%s", content)
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, want := range []string{`"kind":"mcp"`, `"protocol":"mcp"`, `"method":"initialize"`, `"method":"tools/call"`, `"path":"/mcp/zs"`, `"exposed":"zs"`} {
		if !strings.Contains(content, want) {
			t.Errorf("request log missing %s\n%s", want, content)
		}
	}
	if !strings.Contains(content, `"provider":"zhipu"`) {
		t.Errorf("request log missing account projection\n%s", content)
	}
	// Credentials must never reach the log.
	if strings.Contains(content, "k-A") || strings.Contains(content, "Bearer") {
		t.Errorf("credential leaked into request log\n%s", content)
	}
}

// TestMCPGateway_RequestLogSplitStream: with request_log.mcp_split on, MCP
// exchanges land in the dedicated mcp- stream under mcp_dir and NEVER in the
// requests- stream (which goes back to LLM-only traffic).
func TestMCPGateway_RequestLogSplitStream(t *testing.T) {
	up := &fakeMCPUpstream{sessionID: "up-1"}
	upSrv := httptest.NewServer(http.HandlerFunc(up.serve))
	t.Cleanup(upSrv.Close)

	t.Setenv("HOME", t.TempDir())
	writeMCPKeys(t, "zhipu", "k-A")
	cfg := &configdomain.Config{
		Listen: "127.0.0.1:0",
		Providers: map[string]configdomain.Provider{
			"zhipu": {Provider: "zhipu", OpenAIBaseURL: "http://127.0.0.1:1", Models: []string{"m"}},
		},
		MCP: map[string]configdomain.MCPServer{"zs": {Provider: "zhipu", URL: upSrv.URL}},
	}
	p := newTestProxy(t, cfg)
	reqDir, mcpDir := t.TempDir(), t.TempDir()
	p.initRequestLog(configdomain.RequestLogConfig{Enabled: true, Dir: reqDir, MCPSplit: true, MCPDir: mcpDir})
	if p.reqLog == nil || p.mcpReqLog == nil {
		t.Fatal("split request log not initialized")
	}
	p.reqLogStarted = p.lifecycle.Run(func(<-chan struct{}) { p.reqLog.Run() })
	p.mcpReqLogStarted = p.lifecycle.Run(func(<-chan struct{}) { p.mcpReqLog.Run() })
	srv := httptest.NewServer(http.HandlerFunc(p.Handler))
	t.Cleanup(srv.Close)

	resp := mcpPost(t, srv.URL+"/mcp/zs", "", mcpInitBody)
	localSID := resp.Header.Get("Mcp-Session-Id")
	resp.Body.Close()
	mcpPost(t, srv.URL+"/mcp/zs", localSID, mcpCallBody).Body.Close()

	// Bounded poll: the single-writer sink flushes on its own schedule.
	deadline := time.Now().Add(5 * time.Second)
	var mcpContent, reqContent string
	for {
		mcpContent = readAllLogFiles(t, mcpDir)
		reqContent = readAllLogFiles(t, reqDir)
		if strings.Count(mcpContent, `"kind":"mcp"`) >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("mcp records never landed in the split stream:\n%s", mcpContent)
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, want := range []string{`"kind":"mcp"`, `"method":"initialize"`, `"method":"tools/call"`, `"path":"/mcp/zs"`} {
		if !strings.Contains(mcpContent, want) {
			t.Errorf("split stream missing %s\n%s", want, mcpContent)
		}
	}
	if strings.Contains(mcpContent, "k-A") || strings.Contains(mcpContent, "Bearer") {
		t.Errorf("credential leaked into split stream\n%s", mcpContent)
	}
	if reqContent != "" {
		t.Errorf("requests- stream received mcp traffic despite mcp_split:\n%s", reqContent)
	}
}

// readAllLogFiles concatenates every .log file in dir (empty when none exist
// yet — the sink creates files lazily).
func readAllLogFiles(t *testing.T, dir string) string {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	var b strings.Builder
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".log") {
			data, _ := os.ReadFile(filepath.Join(dir, e.Name()))
			b.Write(data)
		}
	}
	return b.String()
}

// TestMCPGateway_RequestLogAttribution: MCP records carry the client
// attribution — agent from the initialize clientInfo (a UA-less client) or
// the UA, session from the client's session-header allowlist value or the
// local MCP session id.
func TestMCPGateway_RequestLogAttribution(t *testing.T) {
	up := &fakeMCPUpstream{sessionID: "up-at-1"}
	upSrv := httptest.NewServer(http.HandlerFunc(up.serve))
	t.Cleanup(upSrv.Close)

	t.Setenv("HOME", t.TempDir())
	writeMCPKeys(t, "zhipu", "k-A")
	cfg := &configdomain.Config{
		Listen: "127.0.0.1:0",
		Providers: map[string]configdomain.Provider{
			"zhipu": {Provider: "zhipu", OpenAIBaseURL: "http://127.0.0.1:1", Models: []string{"m"}},
		},
		MCP: map[string]configdomain.MCPServer{"zs": {Provider: "zhipu", URL: upSrv.URL}},
	}
	p := newTestProxy(t, cfg)
	logDir := t.TempDir()
	p.initRequestLog(configdomain.RequestLogConfig{Enabled: true, Dir: logDir})
	p.reqLogStarted = p.lifecycle.Run(func(<-chan struct{}) { p.reqLog.Run() })
	srv := httptest.NewServer(http.HandlerFunc(p.Handler))
	t.Cleanup(srv.Close)

	// A UA-less client: initialize declares codex-mcp-client; the follow-up
	// carries only the session id. A client session header (allowlist) rides
	// both requests and wins the session_id field.
	const clientSession = "codex-thread-7"
	postWithIdentity := func(session, body string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/mcp/zs", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("x-session-id", clientSession)
		if session != "" {
			req.Header.Set("Mcp-Session-Id", session)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	initBody := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"codex-mcp-client","version":"0.1"}}}`
	resp := postWithIdentity("", initBody)
	localSID := resp.Header.Get("Mcp-Session-Id")
	resp.Body.Close()
	if localSID == "" {
		t.Fatal("no session minted — attribution via session cannot be tested")
	}
	postWithIdentity(localSID, mcpCallBody).Body.Close()

	deadline := time.Now().Add(5 * time.Second)
	var lines []string
	for {
		content := readAllLogFiles(t, logDir)
		lines = strings.Split(content, "\n")
		hits := 0
		for _, l := range lines {
			if strings.Contains(l, `"kind":"mcp"`) {
				hits++
			}
		}
		if hits >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("mcp records never landed:\n%s", content)
		}
		time.Sleep(20 * time.Millisecond)
	}
	var initRec, callRec map[string]any
	for _, l := range lines {
		if !strings.Contains(l, `"kind":"mcp"`) {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(l), &rec); err != nil {
			continue
		}
		switch rec["method"] {
		case "initialize":
			initRec = rec
		case "tools/call":
			callRec = rec
		}
	}
	if initRec == nil || callRec == nil {
		t.Fatalf("missing records (init present=%v call present=%v)", initRec != nil, callRec != nil)
	}
	for name, rec := range map[string]map[string]any{"initialize": initRec, "tools/call": callRec} {
		if got := rec["agent"]; got != "codex" {
			t.Errorf("%s record agent = %v, want codex (clientInfo/session binding)", name, got)
		}
		if got := rec["session_id"]; got != clientSession {
			t.Errorf("%s record session_id = %v, want the client session header %q", name, got, clientSession)
		}
	}
	// Session attribution without the allowlist header: the local MCP session
	// id groups the exchanges instead.
	resp2 := mcpPost(t, srv.URL+"/mcp/zs", localSID, mcpCallBody)
	resp2.Body.Close()
	deadline = time.Now().Add(5 * time.Second)
	for {
		content := readAllLogFiles(t, logDir)
		if strings.Count(content, `"method":"tools/call"`) >= 2 {
			if !strings.Contains(content, `"session_id":"`+localSID+`"`) {
				t.Fatalf("no record carries the local session id %q:\n%s", localSID, content)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("second tools/call never landed:\n%s", content)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func mustRead(resp *http.Response) string {
	defer resp.Body.Close()
	buf := &strings.Builder{}
	chunk := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(chunk)
		buf.Write(chunk[:n])
		if err != nil {
			break
		}
	}
	return buf.String()
}

func readBodyString(resp *http.Response) (string, error) {
	defer resp.Body.Close()
	return mustRead2(resp)
}

func mustRead2(resp *http.Response) (string, error) {
	buf := &strings.Builder{}
	chunk := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(chunk)
		buf.Write(chunk[:n])
		if err != nil {
			return buf.String(), nil
		}
	}
}

// TestMCPGateway_EnvStaticHeaders: env-referenced static headers reach the
// upstream on every request (third-party keyed services).
func TestMCPGateway_EnvStaticHeaders(t *testing.T) {
	t.Setenv("TEST_MCP_KEY", "tvly-test-1")
	up := &fakeMCPUpstream{}
	upSrv := httptest.NewServer(http.HandlerFunc(up.serve))
	t.Cleanup(upSrv.Close)

	srv := newMCPTestProxy(t, "zhipu", []string{"k-A"}, map[string]configdomain.MCPServer{
		"tv": {URL: upSrv.URL, Auth: "none", Headers: map[string]string{"Authorization": "env:TEST_MCP_KEY"}},
	})
	resp := mcpPost(t, srv.URL+"/mcp/tv", "", mcpCallBody)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	resp.Body.Close()
	if got := up.auths()[0]; got != "tvly-test-1" {
		t.Fatalf("static header = %q (note: raw value, not Bearer)", got)
	}
}

// TestMCPGateway_GuardSecrets: guard.mcp_secrets scans outbound MCP bodies —
// block rejects with 400 (upstream never sees it), redact rewrites before
// forwarding, log passes through; all audited by pattern NAME only.
func TestMCPGateway_GuardSecrets(t *testing.T) {
	const secretBody = `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search","arguments":{"q":"` + guardFixtureKey + `"}}}`
	newGuardedProxy := func(t *testing.T, action string) (*httptest.Server, *fakeMCPUpstream) {
		t.Helper()
		up := &fakeMCPUpstream{}
		upSrv := httptest.NewServer(http.HandlerFunc(up.serve))
		t.Cleanup(upSrv.Close)
		t.Setenv("HOME", t.TempDir())
		writeMCPKeys(t, "zhipu", "k-A")
		cfg := &configdomain.Config{
			Listen: "127.0.0.1:0",
			Providers: map[string]configdomain.Provider{
				"zhipu": {Provider: "zhipu", OpenAIBaseURL: "http://127.0.0.1:1", Models: []string{"m"}},
			},
			Guard: configdomain.GuardConfig{MCPSecrets: action},
			MCP:   map[string]configdomain.MCPServer{"zs": {Provider: "zhipu", URL: upSrv.URL}},
		}
		p := newTestProxy(t, cfg)
		srv := httptest.NewServer(http.HandlerFunc(p.Handler))
		t.Cleanup(srv.Close)
		return srv, up
	}

	// block: 400, upstream untouched.
	srv, up := newGuardedProxy(t, "block")
	resp := mcpPost(t, srv.URL+"/mcp/zs", "", secretBody)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("block = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
	if len(up.auths()) != 0 {
		t.Fatal("blocked body reached upstream")
	}

	// redact: upstream receives [REDACTED], never the fixture bytes.
	srv, up = newGuardedProxy(t, "redact")
	resp = mcpPost(t, srv.URL+"/mcp/zs", "", secretBody)
	if resp.StatusCode != 200 {
		t.Fatalf("redact = %d", resp.StatusCode)
	}
	resp.Body.Close()
	if got := readAllString(up); !strings.Contains(got, "[REDACTED]") || strings.Contains(got, "IOSFODNN7") {
		t.Fatalf("redact upstream body = %q", got)
	}

	// log: passes through verbatim.
	srv, up = newGuardedProxy(t, "log")
	resp = mcpPost(t, srv.URL+"/mcp/zs", "", secretBody)
	if resp.StatusCode != 200 {
		t.Fatalf("log = %d", resp.StatusCode)
	}
	resp.Body.Close()
	if got := readAllString(up); !strings.Contains(got, "IOSFODNN7") {
		t.Fatalf("log should pass body verbatim, upstream = %q", got)
	}

	// off (default): no scan at all.
	srv, up = newGuardedProxy(t, "")
	resp = mcpPost(t, srv.URL+"/mcp/zs", "", secretBody)
	if resp.StatusCode != 200 {
		t.Fatalf("off = %d", resp.StatusCode)
	}
	resp.Body.Close()
	if got := readAllString(up); !strings.Contains(got, "IOSFODNN7") {
		t.Fatalf("off should pass body verbatim, upstream = %q", got)
	}
}

// readAllString concatenates all request bodies the fake upstream saw.
func readAllString(up *fakeMCPUpstream) string {
	up.mu.Lock()
	defer up.mu.Unlock()
	var b strings.Builder
	for _, body := range up.bodies {
		b.WriteString(body)
		b.WriteByte('\n')
	}
	return b.String()
}

// TestMCPGateway_LegacySSE: the legacy HTTP+SSE transport — GET rewrites the
// endpoint event back to the gateway and binds the POST channel to the same
// account; the session dies with the GET stream.
func TestMCPGateway_LegacySSE(t *testing.T) {
	var mu sync.Mutex
	var postURL, postAuth, postBody string
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			fl, _ := w.(http.Flusher)
			fmt.Fprintf(w, "event: endpoint\ndata: /messages?sessionId=up-42\n\n")
			fl.Flush()
			<-release // hold the stream like a real legacy server
			return
		}
		mu.Lock()
		postURL = r.URL.String()
		postAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		postBody = string(body)
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(up.Close)

	srv := newMCPTestProxy(t, "zhipu", []string{"k-A"}, map[string]configdomain.MCPServer{
		"legacy": {Provider: "zhipu", URL: up.URL + "/sse", Transport: "sse"},
	})

	// GET: the endpoint event must come back pointing at the gateway.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/mcp/legacy", nil)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET = %d", resp.StatusCode)
	}
	// Read until the endpoint event's data line.
	var endpointLine string
	br := bufio.NewReader(resp.Body)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("stream read: %v", err)
		}
		if strings.HasPrefix(line, "data: ") {
			endpointLine = strings.TrimSpace(line)
			break
		}
	}
	if !strings.HasPrefix(endpointLine, "data: /mcp/legacy?mps=") {
		t.Fatalf("endpoint not rewritten to gateway: %q", endpointLine)
	}
	localID := strings.TrimPrefix(endpointLine, "data: /mcp/legacy?mps=")

	// POST to the rewritten URL: must reach the upstream's named endpoint
	// (/messages?sessionId=up-42) with the session's account auth.
	post := mcpPost(t, srv.URL+"/mcp/legacy?mps="+localID, "", mcpCallBody)
	if post.StatusCode != http.StatusAccepted {
		t.Fatalf("POST = %d", post.StatusCode)
	}
	post.Body.Close()
	mu.Lock()
	if postURL != "/messages?sessionId=up-42" {
		t.Fatalf("upstream POST URL = %q", postURL)
	}
	if postAuth != "Bearer k-A" {
		t.Fatalf("upstream POST auth = %q", postAuth)
	}
	if !strings.Contains(postBody, "tools/call") {
		t.Fatalf("upstream POST body = %q", postBody)
	}
	mu.Unlock()

	// Closing the GET stream kills the legacy session.
	close(release)
	resp.Body.Close()
	deadline = time.Now().Add(2 * time.Second)
	for {
		post = mcpPost(t, srv.URL+"/mcp/legacy?mps="+localID, "", mcpCallBody)
		post.Body.Close()
		mu.Lock()
		urlBefore := postURL
		mu.Unlock()
		_ = urlBefore
		break
	}
	// After stream end the session is gone: the mps id no longer resolves, so
	// the request must NOT carry the upstream correlation (fails closed as a
	// sessionless request against the sse URL, never to /messages).
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := postURL
		mu.Unlock()
		if got == "/messages?sessionId=up-42" {
			t.Fatal("stale mps session still reached the upstream endpoint")
		}
		break
	}
}

// TestMCPGateway_LiveEvents: MCP exchanges publish start/end live events with
// a stable request_id, protocol "mcp", and the committed provider — the Live
// monitor covers the gateway like the LLM surface.
func TestMCPGateway_LiveEvents(t *testing.T) {
	up := &fakeMCPUpstream{sessionID: "up-1"}
	upSrv := httptest.NewServer(http.HandlerFunc(up.serve))
	t.Cleanup(upSrv.Close)

	t.Setenv("HOME", t.TempDir())
	writeMCPKeys(t, "zhipu", "k-A")
	cfg := &configdomain.Config{
		Listen: "127.0.0.1:0",
		Providers: map[string]configdomain.Provider{
			"zhipu": {Provider: "zhipu", OpenAIBaseURL: "http://127.0.0.1:1", Models: []string{"m"}},
		},
		MCP: map[string]configdomain.MCPServer{"zs": {Provider: "zhipu", URL: upSrv.URL}},
	}
	p := newTestProxy(t, cfg)
	srv := httptest.NewServer(http.HandlerFunc(p.Handler))
	t.Cleanup(srv.Close)

	mcpPost(t, srv.URL+"/mcp/zs", "", mcpInitBody).Body.Close()
	mcpPost(t, srv.URL+"/mcp/ghost", "", mcpCallBody).Body.Close()

	var starts, ends []string
	for _, e := range p.events.Snapshot() {
		if e.Protocol != "mcp" {
			continue
		}
		switch e.Type {
		case "start":
			starts = append(starts, e.RequestID+":"+e.Exposed)
		case "end":
			if e.Exposed != "zs" || e.Status != 200 || e.Provider != "zhipu" {
				t.Fatalf("end event = %+v", e)
			}
			ends = append(ends, e.RequestID)
		}
	}
	if len(starts) != 1 || !strings.HasSuffix(starts[0], ":zs") {
		t.Fatalf("start events = %v (unknown server must not publish)", starts)
	}
	if len(ends) != 1 {
		t.Fatalf("end events = %v", ends)
	}
	// Stable pairing: same request_id on start and end.
	if !strings.HasPrefix(starts[0], ends[0]+":") {
		t.Fatalf("start/end request_id mismatch: %v vs %v", starts, ends)
	}
}

// TestMCPGateway_StatsSurfaced: terminal MCP exchanges are counted on the
// gateway's own stats channel and surfaced via /api/mcp — separate from the
// LLM metrics store (no token pollution).
func TestMCPGateway_StatsSurfaced(t *testing.T) {
	up := &fakeMCPUpstream{}
	upSrv := httptest.NewServer(http.HandlerFunc(up.serve))
	t.Cleanup(upSrv.Close)
	t.Setenv("HOME", t.TempDir())
	writeMCPKeys(t, "zhipu", "k-A")
	cfg := &configdomain.Config{
		Listen: "127.0.0.1:0",
		Providers: map[string]configdomain.Provider{
			"zhipu": {Provider: "zhipu", OpenAIBaseURL: "http://127.0.0.1:1", Models: []string{"m"}},
		},
		MCP: map[string]configdomain.MCPServer{"fc": {Provider: "zhipu", URL: upSrv.URL}},
	}
	p := newTestProxy(t, cfg)
	srv := httptest.NewServer(http.HandlerFunc(p.Handler))
	t.Cleanup(srv.Close)

	mcpPost(t, srv.URL+"/mcp/fc", "", mcpInitBody).Body.Close()
	mcpPost(t, srv.URL+"/mcp/fc", "", mcpCallBody).Body.Close()

	// 404s for unknown names are rejected before mcpLog's named funnel (the
	// stats channel counts named exchanges only), so two calls land here.
	w, _ := NewWebServer(p, "test-config.yaml"), p
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/mcp", nil)
	mux := http.NewServeMux()
	w.Register(mux)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/mcp status = %d: %s", rec.Code, rec.Body.String())
	}
	var surface struct {
		Servers []struct {
			Name         string `json:"name"`
			Calls        uint64 `json:"calls"`
			Errors       uint64 `json:"errors"`
			AvgLatencyMs uint64 `json:"avg_latency_ms"`
		} `json:"servers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &surface); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(surface.Servers) != 1 || surface.Servers[0].Name != "fc" {
		t.Fatalf("surface = %+v", surface)
	}
	got := surface.Servers[0]
	if got.Calls != 2 || got.Errors != 0 {
		t.Fatalf("calls/errors = %d/%d, want 2/0", got.Calls, got.Errors)
	}
}

// TestMCPGateway_CrossOriginRedirectNoLeak: an upstream answering 307 to a
// FOREIGN host must fail the exchange instead of re-sending the injected
// credential (auth_header / Authorization / static headers) to the redirect
// target. Regression for the default redirect policy leaking keys cross-origin.
func TestMCPGateway_CrossOriginRedirectNoLeak(t *testing.T) {
	leaked := make(chan string, 1)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked <- r.Header.Get("X-Agent-Plan-Key")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/mcp", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirector.Close)

	srv := newMCPTestProxy(t, "volcengine", []string{"plan-key-1"}, map[string]configdomain.MCPServer{
		"dp": {Provider: "volcengine", URL: redirector.URL, AuthHeader: "X-Agent-Plan-Key"},
	})
	resp := mcpPost(t, srv.URL+"/mcp/dp", "", mcpCallBody)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("redirected call = %d, want 502 (cross-origin redirect refused)", resp.StatusCode)
	}
	// The exchange is fully over; had the redirect been followed, the target
	// would have recorded the key before the gateway answered.
	select {
	case key := <-leaked:
		t.Fatalf("credential leaked to redirect target: %q", key)
	default:
	}
}

// TestMCPGateway_ReinitializeRefreshesUpstreamID: a stateful upstream that
// rotates its Mcp-Session-Id on re-initialize rebinds the session — later
// requests address the NEW upstream id, not the stale one (which 404s).
func TestMCPGateway_ReinitializeRefreshesUpstreamID(t *testing.T) {
	up := &fakeMCPUpstream{sessionID: "up-1", reinitSessionID: "up-2"}
	upSrv := httptest.NewServer(http.HandlerFunc(up.serve))
	t.Cleanup(upSrv.Close)

	srv := newMCPTestProxy(t, "zhipu", []string{"k-A"}, map[string]configdomain.MCPServer{
		"zs": {Provider: "zhipu", URL: upSrv.URL},
	})
	resp := mcpPost(t, srv.URL+"/mcp/zs", "", mcpInitBody)
	localSID := resp.Header.Get("Mcp-Session-Id")
	resp.Body.Close()
	if localSID == "" {
		t.Fatal("no local session minted")
	}
	// Re-initialize on the established session: the upstream rotates its id.
	resp = mcpPost(t, srv.URL+"/mcp/zs", localSID, mcpInitBody)
	if got := resp.Header.Get("Mcp-Session-Id"); got != localSID {
		t.Fatalf("re-initialize local id = %q, want stable %q", got, localSID)
	}
	resp.Body.Close()
	// The next call must carry the ROTATED upstream id.
	resp = mcpPost(t, srv.URL+"/mcp/zs", localSID, mcpCallBody)
	resp.Body.Close()
	up.mu.Lock()
	defer up.mu.Unlock()
	last := up.hits[len(up.hits)-1]
	if last.session != "up-2" {
		t.Fatalf("upstream session after rotation = %q, want up-2", last.session)
	}
}

// TestMCPGateway_LiveProviderIsServingAccount: after the one-shot 401
// rotation, the live end event names the account that actually SERVED, not
// the one that failed (first-commit-wins on the writer must not record the
// pre-rotation pick).
func TestMCPGateway_LiveProviderIsServingAccount(t *testing.T) {
	up := &fakeMCPUpstream{failKeys: map[string]int{"Bearer k-A": 1}}
	upSrv := httptest.NewServer(http.HandlerFunc(up.serve))
	t.Cleanup(upSrv.Close)

	t.Setenv("HOME", t.TempDir())
	writeMCPKeys(t, "zhipu", "k-A", "k-B") // pool: #aaa (k-A), #bbb (k-B)
	cfg := &configdomain.Config{
		Listen: "127.0.0.1:0",
		Providers: map[string]configdomain.Provider{
			"zhipu": {Provider: "zhipu", OpenAIBaseURL: "http://127.0.0.1:1", Models: []string{"m"}},
		},
		MCP: map[string]configdomain.MCPServer{"zs": {Provider: "zhipu", URL: upSrv.URL}},
	}
	p := newTestProxy(t, cfg)
	srv := httptest.NewServer(http.HandlerFunc(p.Handler))
	t.Cleanup(srv.Close)

	// Two sessionless calls: the poisoned key (k-A) 401s exactly once and the
	// call rotates to the good account (see TestMCPGateway_401RotatesOnce).
	// Whichever virtual id holds k-A, BOTH calls end served by the same good
	// account — so both end events must name the same provider. Pre-fix, the
	// rotating call's end recorded the FAILED pre-rotation pick and the two
	// events diverged.
	mcpPost(t, srv.URL+"/mcp/zs", "", mcpCallBody).Body.Close()
	mcpPost(t, srv.URL+"/mcp/zs", "", mcpCallBody).Body.Close()
	counts := map[string]int{}
	for _, a := range up.auths() {
		counts[a]++
	}
	if counts["Bearer k-A"] != 1 {
		t.Fatalf("poisoned key hit %d times, want exactly 1 (rotation did not happen)", counts["Bearer k-A"])
	}

	var ends []string
	for _, e := range p.events.Snapshot() {
		if e.Protocol == "mcp" && e.Type == "end" {
			ends = append(ends, e.Provider)
		}
	}
	if len(ends) != 2 {
		t.Fatalf("mcp end events = %v", ends)
	}
	if ends[0] == "" || ends[0] != ends[1] {
		t.Fatalf("end event providers diverged: %v (the rotated call recorded the failed account)", ends)
	}
}

// TestMCPGateway_LiveEventAttribution: the live end event carries the same
// resolved attribution the request log records — the clientInfo-derived agent
// (a UA-less client) and the session id (allowlist header or the local MCP
// session id). The start event can only know the UA label.
func TestMCPGateway_LiveEventAttribution(t *testing.T) {
	up := &fakeMCPUpstream{sessionID: "up-live-1"}
	upSrv := httptest.NewServer(http.HandlerFunc(up.serve))
	t.Cleanup(upSrv.Close)

	t.Setenv("HOME", t.TempDir())
	writeMCPKeys(t, "zhipu", "k-A")
	cfg := &configdomain.Config{
		Listen: "127.0.0.1:0",
		Providers: map[string]configdomain.Provider{
			"zhipu": {Provider: "zhipu", OpenAIBaseURL: "http://127.0.0.1:1", Models: []string{"m"}},
		},
		MCP: map[string]configdomain.MCPServer{"zs": {Provider: "zhipu", URL: upSrv.URL}},
	}
	p := newTestProxy(t, cfg)
	srv := httptest.NewServer(http.HandlerFunc(p.Handler))
	t.Cleanup(srv.Close)

	initBody := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"codex-mcp-client","version":"0.1"}}}`
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/mcp/zs", strings.NewReader(initBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("x-session-id", "live-thread-9")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	localSID := resp.Header.Get("Mcp-Session-Id")
	resp.Body.Close()
	if localSID == "" {
		t.Fatal("no session minted")
	}

	var endEvent *observeevents.Event
	deadline := time.Now().Add(5 * time.Second)
	for endEvent == nil {
		for _, e := range p.events.Snapshot() {
			if e.Protocol == "mcp" && e.Type == "end" && e.Exposed == "zs" {
				cp := e
				endEvent = &cp
			}
		}
		if endEvent != nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if endEvent == nil {
		t.Fatal("mcp end event never published")
	}
	if endEvent.Agent != "codex" {
		t.Errorf("end event agent = %q, want codex (clientInfo resolution)", endEvent.Agent)
	}
	if endEvent.SessionID != "live-thread-9" {
		t.Errorf("end event session_id = %q, want the allowlist header value", endEvent.SessionID)
	}
}
