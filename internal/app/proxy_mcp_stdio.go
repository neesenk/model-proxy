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
	"context"
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
	mu     sync.Mutex
	conns  map[string]*mcpStdioEntry
	spawns map[string]*mcpSpawnLock // in-flight lazy spawns, keyed like conns
}

type mcpStdioEntry struct {
	server string
	conn   *mcpkg.StdioConn
}

// mcpSpawnLock serializes lazy spawns for one registry key (refcounted, so
// the map entry is dropped once the last racer leaves).
type mcpSpawnLock struct {
	mu   sync.Mutex
	refs int
}

func newMCPStdioRegistry() *mcpStdioRegistry {
	return &mcpStdioRegistry{conns: map[string]*mcpStdioEntry{}, spawns: map[string]*mcpSpawnLock{}}
}

// lockSpawn serializes lazy child spawns for one key (route sub-session
// ensure): the first caller spawns and handshakes; racers wait, then re-check
// and reuse the winner's conn. Without it, concurrent ensures fork duplicate
// children whose loser either leaks or is killed while the racing caller is
// still mid-flight on it. The returned func releases the lock.
func (r *mcpStdioRegistry) lockSpawn(key string) func() {
	r.mu.Lock()
	l := r.spawns[key]
	if l == nil {
		l = &mcpSpawnLock{}
		r.spawns[key] = l
	}
	l.refs++
	r.mu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		r.mu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(r.spawns, key)
		}
		r.mu.Unlock()
	}
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

// put registers a child, enforcing the per-server cap. Overwriting an
// existing key (two concurrent ensures racing one sub-session) reaps the
// replaced child OUTSIDE the lock — Close blocks in Wait and must never
// serialize the registry.
func (r *mcpStdioRegistry) put(key, server string, conn *mcpkg.StdioConn) error {
	r.mu.Lock()
	_, replacing := r.conns[key]
	count := 0
	for _, e := range r.conns {
		if e.server == server {
			count++
		}
	}
	if count >= mcpStdioPerServerCap && !replacing {
		r.mu.Unlock()
		return fmt.Errorf("mcp %q: too many live stdio sessions (%d cap)", server, mcpStdioPerServerCap)
	}
	old := r.conns[key]
	r.conns[key] = &mcpStdioEntry{server: server, conn: conn}
	r.mu.Unlock()
	if old != nil {
		old.conn.Close()
	}
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
func (p *Proxy) serveMCPStdio(w http.ResponseWriter, r *http.Request, name string, srv configdomain.MCPServer, snap RuntimeSnapshot, started time.Time, requestID string, ident *mcpIdentity) {
	if r.Method == http.MethodGet {
		http.Error(w, "mcp stdio: GET server-stream is not supported", http.StatusMethodNotAllowed)
		return
	}
	localSID := r.Header.Get("Mcp-Session-Id")

	if r.Method == http.MethodDelete {
		if localSID != "" {
			// Sessions are bound to their server (same ownership rule as the
			// HTTP pinned path): an id minted by another /mcp/ name must not
			// kill this server's child — treat as unknown session.
			if s, ok := p.mcpSessions.Get(localSID); ok {
				if s.Server != name {
					http.Error(w, "mcp: unknown session", http.StatusNotFound)
					return
				}
				p.mcpStdio.kill(localSID)
				p.mcpSessions.Delete(localSID)
			}
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
			p.mcpLog(name, account, frame, r.Method, http.StatusBadGateway, started, requestID, body, nil, 0, false, ident)
			return
		}
		initCtx, initCancel := context.WithTimeout(r.Context(), srv.MCPTimeoutDuration())
		resp, err := conn.Call(initCtx, body)
		initCancel()
		if err != nil {
			conn.Close()
			http.Error(w, fmt.Sprintf("mcp %q: stdio initialize: %v", name, err), http.StatusBadGateway)
			p.mcpLog(name, account, frame, r.Method, http.StatusBadGateway, started, requestID, body, nil, 0, false, ident)
			return
		}
		sid := p.mcpSessions.Put(name, account, "")
		// Bind the declared client identity onto the stdio session.
		p.mcpSessions.SetClient(sid, mcpkg.ParseClientInfo(body))
		if err := p.mcpStdio.put(sid, name, conn); err != nil {
			conn.Close()
			p.mcpSessions.Delete(sid)
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		// Initialized notification: required by stateful servers.
		conn.Call(r.Context(), mcpNotifInitBody) //nolint — best-effort
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", sid)
		w.WriteHeader(http.StatusOK)
		w.Write(resp)
		p.mcpLog(name, account, frame, r.Method, http.StatusOK, started, requestID, body, resp, int64(len(resp)), false, ident)
		return
	}

	// Everything else needs an established session — owned by THIS server
	// (same rule as the HTTP pinned path: a foreign /mcp/ id 404s here instead
	// of reaching another server's child process).
	if localSID == "" {
		respBody := mcpkg.BuildErrorResponse(frame.ID, -32600, "mcp stdio: session required — POST initialize first")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write(respBody)
		return
	}
	if s, ok := p.mcpSessions.Get(localSID); !ok || s.Server != name {
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
	callCtx, callCancel := context.WithTimeout(r.Context(), srv.MCPTimeoutDuration())
	resp, err := conn.Call(callCtx, body)
	callCancel()
	if err != nil {
		// Dead child: drop the session so the client re-initializes instead
		// of retrying a corpse.
		p.mcpStdio.kill(localSID)
		p.mcpSessions.Delete(localSID)
		http.Error(w, fmt.Sprintf("mcp %q: stdio call: %v", name, err), http.StatusBadGateway)
		p.mcpLog(name, "", frame, r.Method, http.StatusBadGateway, started, requestID, body, nil, 0, false, ident)
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
	p.mcpLog(name, "", frame, r.Method, http.StatusOK, started, requestID, body, resp, int64(len(resp)), false, ident)
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
