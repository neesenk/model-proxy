// proxy_mcp_stdio.go — stdio MCP backends (transport: stdio): one local child
// process per client session (pinned) or per (route session, backend) sub-
// session, newline-delimited JSON-RPC bridged onto the /mcp/ HTTP surface.
// Ownership: children live in mcpStdioRegistry (process-lifetime), die with
// their session (session-table OnEvict), and are all reaped at Proxy.Close —
// they write no logs, so Close kills them early. The wire conn itself is
// internal/mcp.StdioConn; this file is the composition-root integration.
package app

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	configdomain "model-proxy/internal/config"
	mcpkg "model-proxy/internal/mcp"
	"model-proxy/internal/provider"
)

// mcpStdioPerServerCap bounds live children per server (each holds a process;
// unbounded sessions must not fork-bomb the host).
const mcpStdioPerServerCap = 8

// mcpStdioRegistry owns the live stdio children, keyed by session id (pinned)
// or routeSessionID+"\x00"+server (route subs).
type mcpStdioRegistry struct {
	mu    sync.Mutex
	conns map[string]*mcpStdioEntry
}

type mcpStdioEntry struct {
	server string
	conn   *mcpkg.StdioConn
}

func newMCPStdioRegistry() *mcpStdioRegistry {
	return &mcpStdioRegistry{conns: map[string]*mcpStdioEntry{}}
}

func (r *mcpStdioRegistry) get(key string) (*mcpkg.StdioConn, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.conns[key]
	if !ok {
		return nil, false
	}
	return e.conn, true
}

// put registers a child, enforcing the per-server cap.
func (r *mcpStdioRegistry) put(key, server string, conn *mcpkg.StdioConn) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, e := range r.conns {
		if e.server == server {
			count++
		}
	}
	if count >= mcpStdioPerServerCap {
		return fmt.Errorf("mcp %q: too many live stdio sessions (%d cap)", server, mcpStdioPerServerCap)
	}
	r.conns[key] = &mcpStdioEntry{server: server, conn: conn}
	return nil
}

// kill terminates and unregisters one child (no-op for unknown keys).
func (r *mcpStdioRegistry) kill(key string) {
	r.mu.Lock()
	e, ok := r.conns[key]
	if ok {
		delete(r.conns, key)
	}
	r.mu.Unlock()
	if ok {
		e.conn.Close()
	}
}

// killPrefix terminates every child whose key carries the prefix (route
// session eviction kills all its sub-session children).
func (r *mcpStdioRegistry) killPrefix(prefix string) {
	r.mu.Lock()
	var dead []*mcpkg.StdioConn
	for k, e := range r.conns {
		if strings.HasPrefix(k, prefix) {
			dead = append(dead, e.conn)
			delete(r.conns, k)
		}
	}
	r.mu.Unlock()
	for _, conn := range dead {
		conn.Close()
	}
}

// killAll reaps every child (Proxy.Close).
func (r *mcpStdioRegistry) killAll() {
	r.mu.Lock()
	dead := make([]*mcpkg.StdioConn, 0, len(r.conns))
	for k, e := range r.conns {
		dead = append(dead, e.conn)
		delete(r.conns, k)
	}
	r.mu.Unlock()
	for _, conn := range dead {
		conn.Close()
	}
}

// mcpOnMCPSessionEvict is the session-table OnEvict callback: pinned children
// die with their session; route sub children die with the route session.
func (p *Proxy) mcpOnMCPSessionEvict(s mcpkg.Session) {
	p.mcpStdio.kill(s.ID)
	if s.Route != nil {
		p.mcpStdio.killPrefix(s.ID + "\x00")
	}
}

// mcpStdioStart launches a stdio child for one server/account pair with the
// resolved environment. The MCP handshake is the CALLER's job (pinned relays
// the client's initialize; routes use the canned one).
func (p *Proxy) mcpStdioStart(snap RuntimeSnapshot, srv configdomain.MCPServer, account string) (*mcpkg.StdioConn, error) {
	accountKey := ""
	if account != "" {
		prov := snap.Providers[account]
		if prov == nil {
			return nil, fmt.Errorf("account %q not in this generation's providers", account)
		}
		kr, ok := prov.(provider.KeyReporter)
		if !ok {
			return nil, fmt.Errorf("provider %q cannot supply a raw API key (apikey providers only)", srv.Provider)
		}
		key, err := kr.ReportKey()
		if err != nil {
			return nil, err
		}
		accountKey = key
	}
	env, err := configdomain.ResolveMCPStdioEnv(srv, accountKey)
	if err != nil {
		return nil, err
	}
	return mcpkg.StartStdio(srv.Command, env)
}

// serveMCPStdio handles /mcp/<name> for transport: stdio servers: one child
// per client session, JSON-RPC bridged over HTTP (single JSON responses; no
// SSE framing, no GET stream).
func (p *Proxy) serveMCPStdio(w http.ResponseWriter, r *http.Request, name string, srv configdomain.MCPServer, snap RuntimeSnapshot, started time.Time, requestID string) {
	if r.Method == http.MethodGet {
		http.Error(w, "mcp stdio: GET server-stream is not supported", http.StatusMethodNotAllowed)
		return
	}
	localSID := r.Header.Get("Mcp-Session-Id")

	if r.Method == http.MethodDelete {
		if localSID != "" {
			p.mcpStdio.kill(localSID)
			p.mcpSessions.Delete(localSID)
		}
		w.WriteHeader(http.StatusOK)
		return
	}

	capBytes := snap.Cfg.MaxRequestBodyBytesValue()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, capBytes))
	if err != nil {
		http.Error(w, "mcp: request body too large or unreadable", http.StatusRequestEntityTooLarge)
		return
	}
	var ok bool
	body, ok = p.mcpGuardScan(snap, name, requestID, body)
	if !ok {
		http.Error(w, "mcp: request blocked by outbound guard", http.StatusBadRequest)
		return
	}
	frame := mcpkg.ParseFrame(body)

	// initialize: spawn + handshake with the client's own initialize body,
	// relaying the child's result (the honest backend answer) with a local
	// session id.
	if frame.Method == "initialize" {
		account := ""
		if srv.MCPAuthMode() == "provider" {
			var ok bool
			account, ok = p.mcpAccountGate(w, snap, name, srv)
			if !ok {
				return
			}
		}
		conn, err := p.mcpStdioStart(snap, srv, account)
		if err != nil {
			http.Error(w, fmt.Sprintf("mcp %q: stdio start: %v", name, err), http.StatusBadGateway)
			p.mcpLog(name, account, frame, r.Method, http.StatusBadGateway, started, requestID, body, nil, 0, false)
			return
		}
		resp, err := conn.Call(body)
		if err != nil {
			conn.Close()
			http.Error(w, fmt.Sprintf("mcp %q: stdio initialize: %v", name, err), http.StatusBadGateway)
			p.mcpLog(name, account, frame, r.Method, http.StatusBadGateway, started, requestID, body, nil, 0, false)
			return
		}
		sid := p.mcpSessions.Put(name, account, "")
		if err := p.mcpStdio.put(sid, name, conn); err != nil {
			conn.Close()
			p.mcpSessions.Delete(sid)
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		// Initialized notification: required by stateful servers.
		conn.Call(mcpNotifInitBody) //nolint — best-effort
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", sid)
		w.WriteHeader(http.StatusOK)
		w.Write(resp)
		p.mcpLog(name, account, frame, r.Method, http.StatusOK, started, requestID, body, resp, int64(len(resp)), false)
		return
	}

	// Everything else needs an established session.
	if localSID == "" {
		respBody := mcpkg.BuildErrorResponse(frame.ID, -32600, "mcp stdio: session required — POST initialize first")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write(respBody)
		return
	}
	if _, ok := p.mcpSessions.Get(localSID); !ok {
		respBody := mcpkg.BuildErrorResponse(frame.ID, -32600, "mcp: unknown or expired session — re-initialize")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write(respBody)
		return
	}
	conn, ok := p.mcpStdio.get(localSID)
	if !ok {
		respBody := mcpkg.BuildErrorResponse(frame.ID, -32600, "mcp: unknown or expired session — re-initialize")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write(respBody)
		return
	}
	resp, err := conn.Call(body)
	if err != nil {
		// Dead child: drop the session so the client re-initializes instead
		// of retrying a corpse.
		p.mcpStdio.kill(localSID)
		p.mcpSessions.Delete(localSID)
		http.Error(w, fmt.Sprintf("mcp %q: stdio call: %v", name, err), http.StatusBadGateway)
		p.mcpLog(name, "", frame, r.Method, http.StatusBadGateway, started, requestID, body, nil, 0, false)
		return
	}
	if resp == nil {
		// Notification semantics: 202, no body.
		w.Header().Set("Mcp-Session-Id", localSID)
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Mcp-Session-Id", localSID)
	w.WriteHeader(http.StatusOK)
	w.Write(resp)
	p.mcpLog(name, "", frame, r.Method, http.StatusOK, started, requestID, body, resp, int64(len(resp)), false)
}

// mcpStdioSynthResponse wraps one stdio JSON-RPC payload as an HTTP response
// so the route/pinned streaming and logging paths stay uniform with HTTP
// backends.
func mcpStdioSynthResponse(payload []byte) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(payload)),
	}
}
