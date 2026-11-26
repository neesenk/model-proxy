// proxy_mcp_route.go — aggregated MCP routes (mcp_routes:) on the /mcp/
// surface. The proxy OWNS the client session here (unlike the pinned
// passthrough): it synthesizes initialize/tools-list answers, lazily builds
// per-backend sub-sessions (each with its own account pick, upstream session
// and negotiated protocol version), merges tools/list under the declared
// canonical names, and fails tools/call over the ordered targets. Pure wire
// logic (message building, name rewriting, merging, failover classification)
// lives in internal/mcp; this file is the composition-root orchestration.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	configdomain "model-proxy/internal/config"
	mcpkg "model-proxy/internal/mcp"
	"model-proxy/internal/provider"
)

// mcpRouteInitBody is the handshake the proxy sends to route backends.
var mcpRouteInitBody = []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"model-proxy","version":"1.0"}}}`)

var mcpNotifInitBody = []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)

// errMCPSubAccountGone is mcpRouteEnsureSub's verdict on a sub-session whose
// bound account a later generation dropped: the binding (stdio child
// included) is cleared and the exchange that found it fails closed with 410.
var errMCPSubAccountGone = errors.New(mcpSessionAccountGoneMsg)

// serveMCPRoute handles /mcp/<route>. Dispatched from serveMCP after the
// pinned-server lookup misses; the api-keys gate and method check already ran.
func (p *Proxy) serveMCPRoute(w http.ResponseWriter, r *http.Request, name string, route configdomain.MCPRoute, snap RuntimeSnapshot, started time.Time, requestID string, ident *mcpIdentity) {
	if r.Method == http.MethodGet {
		// Aggregated routes have no single upstream to stream from.
		http.Error(w, "mcp route: GET server-stream is not supported on aggregated routes", http.StatusMethodNotAllowed)
		return
	}
	var body []byte
	if r.Method == http.MethodPost {
		capBytes := snap.Cfg.MaxRequestBodyBytesValue()
		limited := http.MaxBytesReader(w, r.Body, capBytes)
		var err error
		body, err = io.ReadAll(limited)
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
	}
	frame := mcpkg.ParseFrame(body)
	localSID := r.Header.Get("Mcp-Session-Id")
	ex := &mcpRouteExchange{name: name, method: r.Method, reqBody: body, frame: frame, started: started, requestID: requestID, ident: ident}

	// DELETE: terminate every initialized sub-session and drop the route
	// session. Best-effort per backend — the local session dies regardless.
	if r.Method == http.MethodDelete {
		if localSID != "" {
			for _, sub := range p.mcpSessions.RouteSubs(localSID) {
				if srv, ok := snap.Cfg.MCP[sub.Server]; ok {
					p.mcpTerminateSub(snap, srv, sub, localSID, r)
				}
			}
			p.mcpSessions.Delete(localSID)
		}
		w.WriteHeader(http.StatusOK)
		p.mcpRouteRecord(ex, http.StatusOK, nil)
		return
	}

	// initialize: the proxy answers itself and mints the route session. The
	// protocol version echoes the client's ask; backends are negotiated
	// separately at sub-session build time.
	if frame.Method == "initialize" {
		sid := p.mcpSessions.PutRoute(name)
		// The proxy owns the client session: bind the declared client
		// identity so the session's later exchanges attribute to it.
		p.mcpSessions.SetClient(sid, mcpkg.ParseClientInfo(body))
		result := mcpkg.BuildInitializeResult(mcpkg.ParseClientProtocol(body), name)
		p.mcpRouteWriteJSON(w, ex, sid, http.StatusOK, mcpkg.BuildResultResponse(frame.ID, result))
		return
	}

	// Everything else requires an established route session.
	if localSID == "" {
		respBody := mcpkg.BuildErrorResponse(frame.ID, -32600, "mcp route: session required — POST initialize first")
		p.mcpRouteWriteJSON(w, ex, "", http.StatusBadRequest, respBody)
		return
	}
	if !p.mcpSessions.RouteSessionValid(localSID) {
		respBody := mcpkg.BuildErrorResponse(frame.ID, -32600, "mcp route: unknown or expired session — re-initialize")
		p.mcpRouteWriteJSON(w, ex, "", http.StatusNotFound, respBody)
		return
	}

	switch frame.Method {
	case "tools/list":
		p.mcpRouteToolsList(w, r, name, route, snap, localSID, frame, body, started, requestID, ident)
	case "tools/call":
		p.mcpRouteToolsCall(w, r, name, route, snap, localSID, frame, body, started, requestID, ident)
	case "ping":
		p.mcpRouteWriteJSON(w, ex, localSID, http.StatusOK, mcpkg.BuildResultResponse(frame.ID, json.RawMessage(`{}`)))
	default:
		if strings.HasPrefix(frame.Method, "notifications/") {
			// Best-effort fan-out to initialized sub-sessions; the client
			// always gets 202 (notification semantics: no response body).
			p.mcpRouteFanOut(snap, localSID, body, r)
			w.Header().Set("Mcp-Session-Id", localSID)
			w.WriteHeader(http.StatusAccepted)
			p.mcpRouteRecord(ex, http.StatusAccepted, nil)
			return
		}
		respBody := mcpkg.BuildErrorResponse(frame.ID, -32601, fmt.Sprintf("mcp route: method %q not supported (tools/list and tools/call only)", frame.Method))
		p.mcpRouteWriteJSON(w, ex, localSID, http.StatusOK, respBody)
	}
}

// mcpRouteExchange is what mcpLog needs to record one route exchange: the
// per-request context shared by every proxy-synthesized terminal of it.
type mcpRouteExchange struct {
	name      string      // exposed route name
	method    string      // HTTP verb of the client's request
	reqBody   []byte      // the client's original frame (nil for a DELETE)
	frame     mcpkg.Frame // parsed reqBody (the zero value for a bodyless DELETE)
	started   time.Time
	requestID string
	ident     *mcpIdentity
}

// mcpRouteRecord projects one terminal of a route exchange into the request
// log and the MCP stats. This is the recording contract of all three faces
// (see serveMCP): every exchange that entered the MCP protocol layer is
// recorded, with the HTTP status the client got (a JSON-RPC error sent over
// 200 is a 200, like a pinned upstream 200). Precheck rejections and
// admission failures are not — on the route face they return before any
// exchange context exists.
func (p *Proxy) mcpRouteRecord(ex *mcpRouteExchange, status int, resp []byte) {
	// The route face is synthesized by the proxy across the route's accounts,
	// so there is no single account to attribute the exchange to.
	p.mcpLog(ex.name, "", ex.frame, ex.method, status, ex.started, ex.requestID, ex.reqBody, resp, int64(len(resp)), false, ex.ident)
}

// mcpRouteWriteJSON writes a proxy-synthesized JSON-RPC message and records
// the terminal: a synthesized response cannot be written without being
// recorded.
func (p *Proxy) mcpRouteWriteJSON(w http.ResponseWriter, ex *mcpRouteExchange, sid string, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	if sid != "" {
		w.Header().Set("Mcp-Session-Id", sid)
	}
	w.WriteHeader(status)
	w.Write(body)
	p.mcpRouteRecord(ex, status, body)
}

// mcpRouteToolsList answers tools/list with the session's aggregated canonical
// surface. A backend that fails its handshake/list is skipped — the route
// degrades to the remaining targets instead of failing wholesale — and the
// result is cached per route session by the session table: a complete
// aggregation for the session's lifetime, a degraded one only until a short
// refresh point (the next access then re-aggregates, single-flight).
//
// An aggregation that found a stale backend binding (a sub-session whose
// account a later generation dropped) is neither cached nor answered as a
// surface with that backend missing: the exchange fails closed with 410, and
// the very next tools/list aggregates again.
func (p *Proxy) mcpRouteToolsList(w http.ResponseWriter, r *http.Request, name string, route configdomain.MCPRoute, snap RuntimeSnapshot, sid string, frame mcpkg.Frame, body []byte, started time.Time, requestID string, ident *mcpIdentity) {
	ex := &mcpRouteExchange{name: name, method: r.Method, reqBody: body, frame: frame, started: started, requestID: requestID, ident: ident}
	mappings := make([]mcpkg.TargetMapping, 0, len(route.Targets))
	for _, t := range route.Targets {
		if srv, ok := snap.Cfg.MCP[t.Server]; ok && srv.MCPEffectiveEnabled() {
			mappings = append(mappings, mcpkg.TargetMapping{Server: t.Server, Tools: t.Tools})
		}
	}
	tools, err := p.mcpSessions.RouteToolsLoad(r.Context(), sid, func() ([]mcpkg.ToolSpec, bool, error) {
		var stale error // a backend's sub-session turned out bound to a dropped account
		tools, complete := mcpkg.AggregateTools(mappings, func(server string) ([]mcpkg.ToolSpec, error) {
			specs, err := p.mcpRouteFetchTools(snap, snap.Cfg.MCP[server], server, sid, r)
			if errors.Is(err, errMCPSubAccountGone) {
				stale = err
			}
			return specs, err
		})
		if stale != nil {
			// Not a snapshot of the route: the backend's tools are "missing"
			// only because its credential stopped being usable.
			return nil, false, stale
		}
		return tools, complete, nil
	})
	if err != nil {
		// The aggregation was aborted by a stale binding, which its fetch
		// already cleared. Answering those tools as missing would hide that a
		// credential of this session stopped being usable.
		p.mcpRouteAccountGone(w, ex)
		return
	}
	result, _ := json.Marshal(map[string]any{"tools": tools})
	p.mcpRouteWriteJSON(w, ex, sid, http.StatusOK, mcpkg.BuildResultResponse(frame.ID, result))
}

// mcpRouteToolsCall routes one call to its backends in target order (sticky
// within the session), failing over on transport/auth/session/rate/server
// faults — never on JSON-RPC business errors.
func (p *Proxy) mcpRouteToolsCall(w http.ResponseWriter, r *http.Request, name string, route configdomain.MCPRoute, snap RuntimeSnapshot, sid string, frame mcpkg.Frame, body []byte, started time.Time, requestID string, ident *mcpIdentity) {
	ex := &mcpRouteExchange{name: name, method: r.Method, reqBody: body, frame: frame, started: started, requestID: requestID, ident: ident}
	canonical := mcpkg.ParseToolCallName(body)
	if canonical == "" {
		respBody := mcpkg.BuildErrorResponse(frame.ID, -32602, "tools/call: params.name is required")
		p.mcpRouteWriteJSON(w, ex, sid, http.StatusOK, respBody)
		return
	}
	// Ordered candidate targets serving this canonical tool (enabled only).
	type candidate struct {
		target configdomain.MCPRouteTarget
		srv    configdomain.MCPServer
	}
	var cands []candidate
	declared := false // some enabled target declares this canonical tool
	quotaBlocked := false
	acctBlocked := false
	for _, t := range route.Targets {
		backendName, ok := t.Tools[canonical]
		if !ok || backendName == "" {
			continue
		}
		srv, ok := snap.Cfg.MCP[t.Server]
		if !ok || !srv.MCPEffectiveEnabled() {
			continue
		}
		declared = true
		if mcpProviderQuotaExhausted(p.runtimeState.Quota(srv.Provider)) {
			// Pre-emptive skip: the provider's shared MCP-tool time window is
			// exhausted (zhipu TIME_LIMIT). The call-time 429 failover remains
			// the backstop for windows the poll has not caught yet.
			quotaBlocked = true
			continue
		}
		if srv.MCPAuthMode() == "provider" {
			if picks, raw := p.mcpSchedulableAccounts(snap, srv); raw > 0 && len(picks) == 0 {
				// Pre-emptive skip: every account of this backend's provider is
				// operator-frozen or in cooldown (the same availability rule as
				// LLM scheduling). Call-time failover stays the backstop.
				//
				// This call never reaches the backend's sub-session, and there
				// is no account to rebuild it on: a binding whose account is
				// frozen must still die now (its stdio child holds the frozen
				// key) — the drop the reuse gate and the notification fan-out
				// apply. A cooldown keeps it.
				p.mcpRouteDropFrozenSub(sid, t.Server)
				acctBlocked = true
				continue
			}
		}
		cands = append(cands, candidate{t, srv})
	}
	if len(cands) == 0 {
		if declared && quotaBlocked {
			// Every backend declaring the tool is quota-exhausted: say so
			// instead of misreporting -32602 "unknown tool".
			respBody := mcpkg.BuildErrorResponse(frame.ID, -32000, fmt.Sprintf("tools/call: all backends for tool %q are quota-exhausted (provider MCP-tool window) — retry after the window resets", canonical))
			p.mcpRouteWriteJSON(w, ex, sid, http.StatusOK, respBody)
			return
		}
		if declared && acctBlocked {
			respBody := mcpkg.BuildErrorResponse(frame.ID, -32000, fmt.Sprintf("tools/call: all backends for tool %q have no schedulable account (frozen or in cooldown) — see `model-proxy status` / `model-proxy unfreeze`", canonical))
			p.mcpRouteWriteJSON(w, ex, sid, http.StatusOK, respBody)
			return
		}
		respBody := mcpkg.BuildErrorResponse(frame.ID, -32602, fmt.Sprintf("tools/call: unknown tool %q", canonical))
		p.mcpRouteWriteJSON(w, ex, sid, http.StatusOK, respBody)
		return
	}
	// Session stickiness: a previously successful backend goes first.
	if sticky, ok := p.mcpSessions.RouteStickyGet(sid, canonical); ok {
		for i := range cands {
			if cands[i].target.Server == sticky && i > 0 {
				cands[0], cands[i] = cands[i], cands[0]
				break
			}
		}
	}
	var lastErr error
	for _, c := range cands {
		out, err := mcpkg.RewriteToolCallName(body, c.target.Tools[canonical])
		if err != nil {
			respBody := mcpkg.BuildErrorResponse(frame.ID, -32602, "tools/call: malformed params")
			p.mcpRouteWriteJSON(w, ex, sid, http.StatusOK, respBody)
			return
		}
		sub, err := p.mcpRouteEnsureSub(snap, c.srv, c.target.Server, sid, r)
		if err != nil {
			if errors.Is(err, errMCPSubAccountGone) {
				// Not a backend fault to fail over past: the client is told
				// that a credential of its session stopped being usable.
				p.mcpRouteAccountGone(w, ex)
				return
			}
			lastErr = err
			continue
		}
		if sub.Protocol == "stdio" {
			// stdio backend: single JSON-RPC message over the child conn,
			// wrapped as an HTTP response for the shared commit path. The
			// exchange is bounded by the backend's timeout: a hung child can
			// never park the caller.
			conn, ok := p.mcpStdio.get(mcpRouteStdioKey(sid, c.target.Server))
			if !ok {
				lastErr = fmt.Errorf("stdio sub-session lost")
				p.mcpSessions.RouteSubDrop(sid, c.target.Server)
				continue
			}
			callCtx, callCancel := context.WithTimeout(r.Context(), c.srv.MCPTimeoutDuration())
			payload, callErr := conn.Call(callCtx, out)
			callCancel()
			if callErr != nil {
				lastErr = callErr
				p.mcpStdio.kill(mcpRouteStdioKey(sid, c.target.Server))
				p.mcpSessions.RouteSubDrop(sid, c.target.Server)
				continue
			}
			resp := mcpStdioSynthResponse(payload)
			p.mcpSessions.RouteStickyPut(sid, canonical, c.target.Server)
			mcpSetLiveProvider(w, c.target.Server)
			mcpkg.CopyUpstreamHeaders(w.Header(), resp.Header)
			w.Header().Set("Mcp-Session-Id", sid)
			w.WriteHeader(resp.StatusCode)
			capBytes := 0
			if target := p.mcpLogTarget(); target != nil {
				capBytes = target.MaxBodyBytes()
			}
			captured, total, truncated := mcpStreamResponse(w, resp.Body, capBytes)
			resp.Body.Close()
			p.mcpLog(name, c.target.Server, frame, r.Method, resp.StatusCode, started, requestID, body, captured, total, truncated, ident)
			return
		}
		resp, capBytes, err := p.mcpRoutePost(snap, c.srv, sub.Account, out, sub.UpstreamID, sub.Protocol, r)
		if err != nil {
			lastErr = err
			continue
		}
		if mcpkg.FailoverStatus(resp.StatusCode) {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnauthorized {
				// Session or credential invalid: force a fresh sub-session on
				// the next call instead of reusing the dead one.
				p.mcpSessions.RouteSubDrop(sid, c.target.Server)
			}
			continue
		}
		// Committed: stream the backend response (success OR business error)
		// and stick the session to this backend for the tool.
		p.mcpSessions.RouteStickyPut(sid, canonical, c.target.Server)
		mcpSetLiveProvider(w, c.target.Server)
		mcpkg.CopyUpstreamHeaders(w.Header(), resp.Header)
		w.Header().Set("Mcp-Session-Id", sid)
		w.WriteHeader(resp.StatusCode)
		captured, total, truncated := mcpStreamResponse(w, resp.Body, capBytes)
		resp.Body.Close()
		p.mcpLog(name, c.target.Server, frame, r.Method, resp.StatusCode, started, requestID, body, captured, total, truncated, ident)
		return
	}
	p.mcpFail(w, name, "", frame, r.Method, started, requestID, body, ident,
		http.StatusBadGateway, fmt.Sprintf("mcp route %q: all %d backend(s) failed for tool %q: %v", name, len(cands), canonical, lastErr), lastErr)
}

// mcpRouteStdioKey is the registry key for a route session's stdio child.
func mcpRouteStdioKey(routeSID, server string) string {
	return routeSID + "\x00" + server
}

// mcpRouteDropSub drops one backend sub-session together with its stdio
// child: a plain RouteSubDrop leaves the child registered (its key is in the
// child's environment) until the session dies.
func (p *Proxy) mcpRouteDropSub(sid, server string) {
	p.mcpSessions.RouteSubDrop(sid, server)
	p.mcpStdio.kill(mcpRouteStdioKey(sid, server))
}

// mcpRouteDropFrozenSub drops the backend's sub-session — its stdio child
// included — when its bound account is operator-frozen. A transient cooldown
// is not a freeze and keeps the binding.
func (p *Proxy) mcpRouteDropFrozenSub(sid, server string) {
	if sub, ok := p.mcpSessions.RouteSubGet(sid, server); ok && sub.Account != "" && p.runtimeState.AccountFrozen(sub.Account) {
		p.mcpRouteDropSub(sid, server)
	}
}

// mcpRouteAccountGone answers the terminal of an exchange that found a
// backend sub-session bound to an account a later generation dropped: the
// pinned faces' 410, recorded. The binding itself was cleared by
// mcpRouteEnsureSub.
func (p *Proxy) mcpRouteAccountGone(w http.ResponseWriter, ex *mcpRouteExchange) {
	http.Error(w, mcpSessionAccountGoneMsg, http.StatusGone)
	p.mcpRouteRecord(ex, http.StatusGone, nil)
}

// mcpRouteReusableSub is the sub-session reuse gate. It returns the
// initialized sub-session when it may be used as is; the zero sub-session
// with a nil error when the caller must build one — none exists, or its
// account was frozen and the binding was dropped for a rebuild on a healthy
// sibling; and errMCPSubAccountGone when the bound account was dropped by a
// later generation (the binding is cleared).
//
// Either way a dropped binding takes its stdio child with it at once: the
// child's environment holds the account's key, and a rebuild that fails —
// no healthy sibling, a replacement that cannot be spawned — must not leave
// it running.
func (p *Proxy) mcpRouteReusableSub(snap RuntimeSnapshot, server, sid string) (mcpkg.SubSession, error) {
	sub, ok := p.mcpSessions.RouteSubGet(sid, server)
	if !ok || !sub.Initialized || sub.Account == "" {
		return sub, nil
	}
	if !mcpAccountLive(snap, sub.Account) {
		p.mcpRouteDropSub(sid, server)
		return mcpkg.SubSession{}, errMCPSubAccountGone
	}
	if p.runtimeState.AccountFrozen(sub.Account) {
		p.mcpRouteDropSub(sid, server)
		return mcpkg.SubSession{}, nil
	}
	return sub, nil
}

// mcpRouteEnsureSub lazily builds the backend sub-session: initialize (with
// account pick for provider-backed servers) + initialized notification.
// Concurrent calls for the same (session, server): HTTP backends may both
// initialize — the last RouteSubPut wins; both upstream sessions are valid,
// so this is benign. stdio spawns are singleflight per registry key — only
// one child is forked and racers reuse it (lockSpawn + re-check).
func (p *Proxy) mcpRouteEnsureSub(snap RuntimeSnapshot, srv configdomain.MCPServer, server, sid string, inbound *http.Request) (mcpkg.SubSession, error) {
	// Reuse gate (mcpRouteReusableSub). An operator freeze drops the binding
	// and it is rebuilt on a healthy sibling right here; transient cooldowns
	// keep it — the call-time failover remains the backstop there. An account
	// a later generation DROPPED is not rebuilt behind the client's back: the
	// exchange fails closed with 410 and a later exchange builds a fresh one.
	sub, err := p.mcpRouteReusableSub(snap, server, sid)
	if err != nil {
		return mcpkg.SubSession{}, err
	}
	if sub.Initialized {
		return sub, nil
	}
	account := ""
	if srv.MCPAuthMode() == "provider" {
		picks, raw := p.mcpSchedulableAccounts(snap, srv)
		if raw == 0 {
			return mcpkg.SubSession{}, errors.New(mcpNoAccountErr(srv))
		}
		if len(picks) == 0 {
			return mcpkg.SubSession{}, errors.New(mcpNoSchedulableErr(srv))
		}
		account = picks[int(p.mcpRR.Add(1))%len(picks)]
	}
	if srv.MCPStdio() {
		// stdio backend: one child per (route session, server); the canned
		// handshake negotiates once, the conn lives in the registry. Spawn is
		// singleflight per key: concurrent ensures must not fork duplicates.
		key := mcpRouteStdioKey(sid, server)
		unlock := p.mcpStdio.lockSpawn(key)
		defer unlock()
		if sub, err = p.mcpRouteReusableSub(snap, server, sid); err != nil {
			return mcpkg.SubSession{}, err
		}
		if sub.Initialized {
			return sub, nil // a racing caller just built it
		}
		conn, err := p.mcpStdioStart(snap, srv, account)
		if err != nil {
			return mcpkg.SubSession{}, err
		}
		initCtx, initCancel := context.WithTimeout(inbound.Context(), srv.MCPTimeoutDuration())
		_, err = conn.Call(initCtx, mcpRouteInitBody)
		initCancel()
		if err != nil {
			conn.Close()
			return mcpkg.SubSession{}, fmt.Errorf("stdio initialize: %w", err)
		}
		_, _ = conn.Call(inbound.Context(), mcpNotifInitBody) // best-effort notification
		if err := p.mcpStdio.put(key, server, conn); err != nil {
			conn.Close()
			return mcpkg.SubSession{}, err
		}
		sub := mcpkg.SubSession{Server: server, Account: account, Protocol: "stdio", Initialized: true}
		// The handshake can outlive the route session (LRU eviction races the
		// initialize exchange): a lost put means the session is gone, its
		// eviction event already fired BEFORE this child registered — nothing
		// will ever kill it. Tear it down here instead of leaking it against
		// the per-server cap until process close.
		if !p.mcpSessions.RouteSubPut(sid, sub) {
			p.mcpStdio.kill(key)
			return mcpkg.SubSession{}, fmt.Errorf("route session %s expired during stdio initialize", sid)
		}
		return sub, nil
	}
	resp, _, err := p.mcpRoutePost(snap, srv, account, mcpRouteInitBody, "", "", inbound)
	if err != nil {
		return mcpkg.SubSession{}, err
	}
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	ct := resp.Header.Get("Content-Type")
	upSID := resp.Header.Get("Mcp-Session-Id")
	status := resp.StatusCode
	resp.Body.Close()
	if err != nil {
		return mcpkg.SubSession{}, err
	}
	if status >= 300 {
		return mcpkg.SubSession{}, fmt.Errorf("initialize: HTTP %d", status)
	}
	protocol := ""
	for _, msg := range mcpkg.SplitRPCMessages(ct, payload) {
		if pr, _, _, err := mcpkg.ParseInitializeResult(msg); err == nil && pr != "" {
			protocol = pr
			break
		}
	}
	if protocol == "" {
		return mcpkg.SubSession{}, fmt.Errorf("initialize: no protocolVersion in response")
	}
	// Initialized notification: required by stateful servers, tolerated by all.
	if nresp, _, err := p.mcpRoutePost(snap, srv, account, mcpNotifInitBody, upSID, protocol, inbound); err == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(nresp.Body, 4096))
		nresp.Body.Close()
	}
	sub = mcpkg.SubSession{Server: server, Account: account, UpstreamID: upSID, Protocol: protocol, Initialized: true}
	p.mcpSessions.RouteSubPut(sid, sub)
	return sub, nil
}

// mcpRouteFetchTools initializes the sub-session (if needed) and fetches the
// backend's tool list.
func (p *Proxy) mcpRouteFetchTools(snap RuntimeSnapshot, srv configdomain.MCPServer, server, sid string, inbound *http.Request) ([]mcpkg.ToolSpec, error) {
	sub, err := p.mcpRouteEnsureSub(snap, srv, server, sid, inbound)
	if err != nil {
		return nil, err
	}
	if sub.Protocol == "stdio" {
		conn, ok := p.mcpStdio.get(mcpRouteStdioKey(sid, server))
		if !ok {
			return nil, fmt.Errorf("stdio sub-session lost")
		}
		listCtx, listCancel := context.WithTimeout(inbound.Context(), srv.MCPTimeoutDuration())
		payload, err := conn.Call(listCtx, []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`))
		listCancel()
		if err != nil {
			return nil, err
		}
		return mcpkg.ParseToolsListResult(payload)
	}
	resp, _, err := p.mcpRoutePost(snap, srv, sub.Account, []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`), sub.UpstreamID, sub.Protocol, inbound)
	if err != nil {
		return nil, err
	}
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	ct := resp.Header.Get("Content-Type")
	status := resp.StatusCode
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	if status >= 300 {
		return nil, fmt.Errorf("tools/list: HTTP %d", status)
	}
	for _, msg := range mcpkg.SplitRPCMessages(ct, payload) {
		if specs, err := mcpkg.ParseToolsListResult(msg); err == nil && specs != nil {
			return specs, nil
		}
	}
	return nil, fmt.Errorf("tools/list: no result in response")
}

// mcpRouteFanOut forwards a notification to every initialized sub-session.
// Fire-and-forget: backend errors never reach the client (202 semantics).
func (p *Proxy) mcpRouteFanOut(snap RuntimeSnapshot, sid string, body []byte, inbound *http.Request) {
	for _, sub := range p.mcpSessions.RouteSubs(sid) {
		srv, ok := snap.Cfg.MCP[sub.Server]
		if !ok || !srv.MCPEffectiveEnabled() {
			continue
		}
		if sub.Account != "" && (!mcpAccountLive(snap, sub.Account) || p.runtimeState.AccountFrozen(sub.Account)) {
			// A binding whose account stopped being usable is no fan-out
			// target: it is dropped with its child here as well, so a
			// notification is never the path that keeps the credential in use.
			p.mcpRouteDropSub(sid, sub.Server)
			continue
		}
		if sub.Protocol == "stdio" {
			if conn, ok := p.mcpStdio.get(mcpRouteStdioKey(sid, sub.Server)); ok {
				_, _ = conn.Call(inbound.Context(), body) // fire-and-forget
			}
			continue
		}
		if resp, _, err := p.mcpRoutePost(snap, srv, sub.Account, body, sub.UpstreamID, sub.Protocol, inbound); err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
		}
	}
}

// mcpTerminateSub forwards the client's DELETE to one backend sub-session.
func (p *Proxy) mcpTerminateSub(snap RuntimeSnapshot, srv configdomain.MCPServer, sub mcpkg.SubSession, sid string, inbound *http.Request) {
	if sub.Protocol == "stdio" {
		p.mcpStdio.kill(mcpRouteStdioKey(sid, sub.Server))
		return
	}
	resp, _, err := p.mcpDo(inbound.Context(), snap, srv, sub.Account, mcpCallOpts{
		method:        http.MethodDelete,
		sessionID:     sub.UpstreamID,
		protoOverride: sub.Protocol,
		clientHeaders: inbound.Header,
	})
	if err == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
	}
}

// mcpProviderQuotaExhausted reports whether the provider's shared MCP-tool
// time window is exhausted (zhipu TIME_LIMIT, DetailLabel "By MCP tool").
// Unknown/unpolled quota fails open — the call-time failover is the
// authoritative backstop; this is only a pre-emptive deprioritization.
func mcpProviderQuotaExhausted(snap *provider.QuotaSnapshot) bool {
	if snap == nil {
		return false
	}
	for _, w := range snap.Windows {
		if w.Kind == "time" && w.DetailLabel == "By MCP tool" && w.RemainingPct == 0 {
			return true
		}
	}
	return false
}
