// proxy_mcp.go — the /mcp/ MCP gateway surface (composition root side).
//
// Each request captures ONE RuntimeSnapshot (red line): the mcp: config,
// provider implementations, pool identity and the upstream HTTP client all
// come from that single generation. The handler is a byte-level passthrough
// with three narrow interventions: credential injection (provider pool or
// auth: none), Mcp-Session-Id rewriting with account stickiness (the session
// table is process-lifetime; internal/mcp owns it), and request-log
// projection (kind="mcp"). Tool semantics, failover and aggregation live on
// the routed side (mcp_routes:, separate change).
package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	configdomain "model-proxy/internal/config"
	mcpkg "model-proxy/internal/mcp"
	"model-proxy/internal/observe/counters"
	observeevents "model-proxy/internal/observe/events"
	"model-proxy/internal/observe/requestlog"
	"model-proxy/internal/provider"
)

// mcpLiveWriter captures the committed status and committed provider of one
// MCP exchange for the deferred live end event. Flush is delegated — MCP
// streaming responses must keep their flush-through behavior through the
// wrapper.
type mcpLiveWriter struct {
	http.ResponseWriter
	status   int
	provider string
}

func (w *mcpLiveWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *mcpLiveWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// setProvider records the committed provider/account for the end event
// (first commit wins — failover reports the serving backend).
func (w *mcpLiveWriter) setProvider(provider string) {
	if w.provider == "" {
		w.provider = provider
	}
}

// mcpSetLiveProvider records the committed provider on the live writer when
// the ResponseWriter is one (no-op for unwrapped writers — degenerate paths).
func mcpSetLiveProvider(w http.ResponseWriter, provider string) {
	if lw, ok := w.(*mcpLiveWriter); ok {
		lw.setProvider(provider)
	}
}

// mcpIdentity carries the client-attribution inputs of one MCP exchange:
// the User-Agent-derived agent label, the client's own session header value
// (request_log.session_headers allowlist — same derivation as the LLM
// surface), and the local Mcp-Session-Id. mcpLog resolves them into the
// record's agent/session_id fields (plus the tools/call tool name — same
// write-back lane, consumed by the request-log record and the live end
// event alike).
type mcpIdentity struct {
	uaAgent   string
	clientSID string
	localSID  string
	// resolvedAgent/resolvedSession/resolvedTool carry mcpLog's final
	// attribution back to serveMCP's deferred live end event (it publishes
	// after the handler returned, so the write is visible there). Written
	// once, on the logging goroutine, before the handler returns — no
	// concurrent access.
	resolvedAgent   string
	resolvedSession string
	resolvedTool    string
}

// mcpIdentityFor captures the per-request attribution inputs from the
// snapshot's session-header allowlist and the request's headers. The identity
// is per-request state threaded by pointer through the handler paths.
func (p *Proxy) mcpIdentityFor(r *http.Request, snap RuntimeSnapshot) *mcpIdentity {
	return &mcpIdentity{
		uaAgent:   counters.DetectAgent(r),
		clientSID: requestlog.SessionID(r, snap.Cfg.RequestLog.ResolvedSessionHeaders()),
		localSID:  r.Header.Get("Mcp-Session-Id"),
	}
}

// mcpAgentFor resolves the record's agent label. The MCP-native identity
// wins: clientInfo is the protocol's own client declaration (initialize
// frames carry it directly, later exchanges via the session binding), which
// beats a UA string that may be a bare transport label ("go-http-client").
// The UA remains the fallback — it is the only signal on GET probes and
// stateless pre-initialize exchanges.
func (p *Proxy) mcpAgentFor(ident *mcpIdentity, frame mcpkg.Frame, reqBody []byte) string {
	if frame.Method == "initialize" {
		if name := mcpkg.ParseClientInfo(reqBody); name != "" {
			return counters.AgentFromMCPClient(name)
		}
	}
	if ident.localSID != "" {
		if s, ok := p.mcpSessions.Get(ident.localSID); ok && s.Client != "" {
			return counters.AgentFromMCPClient(s.Client)
		}
	}
	return ident.uaAgent
}

// serveMCP handles /mcp/<name>. Registered in Proxy.Handler before the
// protocol.ForPath fallback, it answers 404 (not the LLM 502) for unknown
// names. The forward-surface api-keys gate has already run (this is not an
// admin endpoint).
func (p *Proxy) serveMCP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	requestID := nextRequestID()
	name := strings.TrimPrefix(r.URL.Path, "/mcp/")
	if name == "" || strings.Contains(name, "/") {
		http.Error(w, fmt.Sprintf("unknown mcp server %q", name), http.StatusNotFound)
		return
	}
	snap := p.SnapshotRuntime()
	ident := p.mcpIdentityFor(r, snap)
	srv, isServer := snap.Cfg.MCP[name]
	isServer = isServer && srv.MCPEffectiveEnabled()
	route, isRoute := snap.Cfg.MCPRoutes[name]
	isRoute = isRoute && route.MCPRouteEffectiveEnabled()
	if !isServer && !isRoute {
		http.Error(w, fmt.Sprintf("unknown mcp server %q", name), http.StatusNotFound)
		return
	}
	// From here this is a live MCP exchange (pinned or route): publish start,
	// and end exactly once on any exit (the deferred closure reads the
	// writer's captured status/provider). The Live monitor's contract mirrors
	// the LLM surface: every start pairs an end with a stable request_id.
	if p.events != nil {
		p.events.Publish(observeevents.Event{Type: "start", Ts: started.UnixMilli(), RequestID: requestID, Agent: ident.uaAgent, Protocol: "mcp", Exposed: name})
		lw := &mcpLiveWriter{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			agent := ident.uaAgent
			if ident.resolvedAgent != "" {
				agent = ident.resolvedAgent
			}
			p.events.Publish(observeevents.Event{
				Type:      "end",
				Ts:        time.Now().UnixMilli(),
				RequestID: requestID,
				Agent:     agent,
				SessionID: ident.resolvedSession,
				Protocol:  "mcp",
				Exposed:   name,
				Provider:  lw.provider,
				Tool:      ident.resolvedTool,
				Status:    lw.status,
				LatencyMs: time.Since(started).Milliseconds(),
			})
		}()
		w = lw
	}
	if isRoute {
		// Aggregated routes (mcp_routes:) share the /mcp/<name> namespace.
		p.serveMCPRoute(w, r, name, route, snap, started, requestID, ident)
		return
	}
	if r.Method != http.MethodPost && r.Method != http.MethodGet && r.Method != http.MethodDelete {
		http.Error(w, "mcp: method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.Method == http.MethodGet && srv.Transport == "sse" {
		// Legacy HTTP+SSE: GET opens the event stream (endpoint rewrite +
		// channel binding lives in proxy_mcp_sse.go).
		p.serveMCPLegacySSE(w, r, name, srv, snap, started, requestID, ident)
		return
	}
	if srv.MCPStdio() {
		// Local child-process backend (one process per session).
		p.serveMCPStdio(w, r, name, srv, snap, started, requestID, ident)
		return
	}
	// Inbound body cap: same bound as the LLM forward surface.
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

	// Session resolution: a client session pins the account AND carries the
	// upstream session id. Sessions are bound to their server — an id issued
	// by another /mcp/ name is unknown here (404 per the MCP session model).
	localSID := r.Header.Get("Mcp-Session-Id")
	if localSID == "" && srv.Transport == "sse" {
		// Legacy sse POSTs carry the local session in the rewritten endpoint
		// URL's query instead of the streamable session header.
		localSID = r.URL.Query().Get("mps")
		ident.localSID = localSID
	}
	var (
		sess    mcpkg.Session
		hasSess bool
	)
	if localSID != "" {
		if s, ok := p.mcpSessions.Get(localSID); ok && s.Server == name {
			sess, hasSess = s, true
		} else if ok {
			// Right id, wrong server: treat as unknown session.
			http.Error(w, "mcp: unknown session", http.StatusNotFound)
			return
		}
		// Unknown/expired local ids are NOT an error: stateless servers never
		// mint one, and clients may race expiry. The request proceeds
		// sessionless (fresh account pick) unless the upstream disagrees.
	}

	// Account selection: session-pinned, else round-robin over the provider's
	// pool. auth: none servers have no account at all.
	accounts := mcpAccounts(snap, srv)
	account := ""
	if srv.MCPAuthMode() == "provider" {
		if len(accounts) == 0 {
			http.Error(w, fmt.Sprintf("mcp %q: %s", name, mcpNoAccountErr(srv)), http.StatusServiceUnavailable)
			return
		}
		if hasSess {
			account = sess.Account
			if !mcpAccountLive(snap, account) {
				// The account vanished from a later generation: fail closed so
				// the client re-initializes instead of drifting to a credential
				// the session was never bound to.
				p.mcpSessions.Delete(localSID)
				http.Error(w, "mcp: session account no longer available — re-initialize", http.StatusGone)
				return
			}
		} else {
			account = accounts[int(p.mcpRR.Add(1))%len(accounts)]
		}
	}

	resp, upstreamBodyCap, err := p.mcpRoundTrip(snap, name, srv, account, sess.UpstreamID, r, body)
	if err == nil && resp.StatusCode == http.StatusUnauthorized && !hasSess && len(accounts) > 1 {
		// One-shot rotation on a sessionless 401 (mirrors BufferedLeg's
		// one-time 401 refresh): the next account may hold a valid key.
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		account = mcpNextAccount(accounts, account)
		resp, upstreamBodyCap, err = p.mcpRoundTrip(snap, name, srv, account, sess.UpstreamID, r, body)
	}
	if srv.MCPAuthMode() == "provider" {
		// Record AFTER the one-shot rotation: the live end event names the
		// account that actually served (the writer is first-commit-wins).
		mcpSetLiveProvider(w, account)
	}
	if err != nil {
		http.Error(w, fmt.Sprintf("mcp %q: upstream: %v", name, err), http.StatusBadGateway)
		p.mcpLog(name, account, frame, r.Method, http.StatusBadGateway, started, requestID, body, nil, 0, false, ident)
		return
	}
	defer resp.Body.Close()

	// Session minting: an upstream Mcp-Session-Id on the initialize response
	// starts a local session pinned to this account. Stateless upstreams (no
	// id — e.g. Firecrawl) never mint one.
	responseSID := ""
	if upSID := resp.Header.Get("Mcp-Session-Id"); upSID != "" {
		switch {
		case hasSess:
			responseSID = localSID // keep the established local id
			if upSID != sess.UpstreamID && srv.Transport != "sse" {
				// The upstream rotated its session id on re-initialize: rebind
				// so later requests don't 404 on the stale id. (Legacy sse
				// stores the full POST endpoint URL in UpstreamID instead.)
				p.mcpSessions.RefreshUpstream(localSID, upSID)
			}
		case r.Method == http.MethodPost && frame.Method == "initialize" && resp.StatusCode < 300:
			responseSID = p.mcpSessions.Put(name, account, upSID)
		}
	}

	if responseSID != "" && frame.Method == "initialize" {
		// Bind the declared client identity (initialize mint AND re-init on
		// an established session — later requests carry only the id).
		p.mcpSessions.SetClient(responseSID, mcpkg.ParseClientInfo(body))
	}
	mcpkg.CopyUpstreamHeaders(w.Header(), resp.Header)
	if responseSID != "" {
		w.Header().Set("Mcp-Session-Id", responseSID)
	}
	w.WriteHeader(resp.StatusCode)
	captured, total, truncated := mcpStreamResponse(w, resp.Body, upstreamBodyCap)

	if r.Method == http.MethodDelete {
		if localSID != "" {
			p.mcpSessions.Delete(localSID)
		}
	}
	p.mcpLog(name, account, frame, r.Method, resp.StatusCode, started, requestID, body, captured, total, truncated, ident)
}

// mcpRoundTrip builds and sends one upstream exchange: allowlisted client
// headers + session id + credential injection, sent through the per-server
// proxy chain. The response body is left open for the caller to stream.
// upstreamBodyCap bounds how much of the response body the request log keeps.
func (p *Proxy) mcpRoundTrip(snap RuntimeSnapshot, name string, srv configdomain.MCPServer, account, upstreamSID string, inbound *http.Request, body []byte) (*http.Response, int, error) {
	opts := mcpCallOpts{
		method:        inbound.Method,
		body:          body,
		sessionID:     upstreamSID,
		clientHeaders: inbound.Header,
	}
	if srv.Transport == "sse" && upstreamSID != "" {
		// Legacy sse: the session's stored "upstream id" is the full POST
		// endpoint URL the upstream named in its endpoint event.
		opts.urlOverride = upstreamSID
		opts.sessionID = ""
	}
	if inbound.Method == http.MethodPost {
		// POST exchanges are time-bounded (search/scrape tools can run for
		// minutes); GET SSE streams and DELETE follow the client connection.
		ctx, cancel := context.WithTimeout(inbound.Context(), srv.MCPTimeoutDuration())
		resp, capBytes, err := p.mcpDo(ctx, snap, srv, account, opts)
		if err != nil {
			cancel()
			return nil, 0, err
		}
		// The timer must outlive the streaming body; cancel on Close.
		resp.Body = &cancelOnCloseBody{ReadCloser: resp.Body, cancel: cancel}
		return resp, capBytes, nil
	}
	return p.mcpDo(inbound.Context(), snap, srv, account, opts)
}

// cancelOnCloseBody releases the POST exchange's timeout timer when the
// response body is fully streamed and closed.
type cancelOnCloseBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelOnCloseBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

// mcpCallOpts describes one upstream MCP exchange for mcpDo. clientHeaders
// is the allowlist copy source (the inbound request's headers; nil skips the
// copy); protoOverride forces MCP-Protocol-Version (route sub-calls use the
// per-backend negotiated version instead of the client's); urlOverride
// replaces srv.URL (legacy sse POSTs go to the upstream-named endpoint URL).
type mcpCallOpts struct {
	method        string
	body          []byte
	sessionID     string
	protoOverride string
	urlOverride   string
	clientHeaders http.Header
}

// mcpDo builds and sends one upstream exchange and returns the open response
// for the caller to stream or drain: allowlisted client headers + session id
// + credential injection, through the per-server proxy chain. The int return
// bounds how much of the response body the request log keeps.
func (p *Proxy) mcpDo(ctx context.Context, snap RuntimeSnapshot, srv configdomain.MCPServer, account string, opts mcpCallOpts) (*http.Response, int, error) {
	targetURL := srv.URL
	if opts.urlOverride != "" {
		targetURL = opts.urlOverride
	}
	upReq, err := http.NewRequestWithContext(ctx, opts.method, targetURL, bytes.NewReader(opts.body))
	if err != nil {
		return nil, 0, err
	}
	if opts.clientHeaders != nil {
		mcpkg.CopyClientHeaders(upReq.Header, opts.clientHeaders)
	}
	if upReq.Header.Get("Accept") == "" {
		upReq.Header.Set("Accept", "application/json, text/event-stream")
	}
	if opts.method == http.MethodPost && upReq.Header.Get("Content-Type") == "" {
		upReq.Header.Set("Content-Type", "application/json")
	}
	if opts.protoOverride != "" {
		upReq.Header.Set("MCP-Protocol-Version", opts.protoOverride)
	}
	if opts.sessionID != "" {
		upReq.Header.Set("Mcp-Session-Id", opts.sessionID)
	}
	if len(srv.Headers) > 0 {
		// Static env-referenced headers (third-party keyed services). Auth
		// injection below runs after them so the declared credential mode
		// always wins on overlap.
		static, err := configdomain.ResolveMCPHeaders(srv)
		if err != nil {
			return nil, 0, err
		}
		for k, v := range static {
			upReq.Header.Set(k, v)
		}
	}
	if srv.MCPAuthMode() == "provider" {
		prov := snap.Providers[account]
		if prov == nil {
			return nil, 0, fmt.Errorf("account %q not in this generation's providers", account)
		}
		if err := mcpInjectAuth(upReq, prov, srv); err != nil {
			return nil, 0, err
		}
		// Provider-level config headers layer on top (mirrors the probe
		// recipe minus ExtraHeaders, which are LLM-chat-specific). env:VAR
		// values resolve at send time like the forward path (a missing
		// variable fails closed).
		if provCfg, ok := configdomain.ProviderConfig(snap.Cfg, snap.ParentOf, account); ok {
			for k, v := range provCfg.Headers {
				resolved, err := configdomain.ResolveHeaderValue(k, v)
				if err != nil {
					return nil, 0, err
				}
				upReq.Header.Set(k, resolved)
			}
		}
	}
	client := p.mcpClientFor(snap, srv, account)
	resp, err := client.Do(upReq)
	if err != nil {
		return nil, 0, err
	}
	capBytes := 0
	// The stream that records the exchange governs the capture cap (the split
	// mcp- logger when active; policy values are shared with the LLM stream).
	if target := p.mcpLogTarget(); target != nil {
		capBytes = target.MaxBodyBytes()
	}
	return resp, capBytes, nil
}

// mcpRoutePost sends one POST exchange to a route backend with the POST
// timeout wrapper (timer released on body Close). Used by the aggregated
// route handler for sub-session initialize, tools/list, tools/call and
// notification fan-out.
func (p *Proxy) mcpRoutePost(snap RuntimeSnapshot, srv configdomain.MCPServer, account string, body []byte, sessionID, protoOverride string, inbound *http.Request) (*http.Response, int, error) {
	ctx, cancel := context.WithTimeout(inbound.Context(), srv.MCPTimeoutDuration())
	resp, capBytes, err := p.mcpDo(ctx, snap, srv, account, mcpCallOpts{
		method:        http.MethodPost,
		body:          body,
		sessionID:     sessionID,
		protoOverride: protoOverride,
		clientHeaders: inbound.Header,
	})
	if err != nil {
		cancel()
		return nil, 0, err
	}
	resp.Body = &cancelOnCloseBody{ReadCloser: resp.Body, cancel: cancel}
	return resp, capBytes, nil
}

// mcpInjectAuth sets the upstream credential: default Authorization: Bearer
// via the provider, or the raw key under a custom header via the KeyReporter
// seam (fail-closed when the provider cannot supply a raw key).
func mcpInjectAuth(upReq *http.Request, prov provider.Provider, srv configdomain.MCPServer) error {
	if srv.AuthHeader == "" || strings.EqualFold(srv.AuthHeader, "Authorization") {
		return prov.AuthHeaders(upReq)
	}
	kr, ok := prov.(provider.KeyReporter)
	if !ok {
		return fmt.Errorf("provider %q cannot supply a raw API key for auth_header %q (apikey providers only)", srv.Provider, srv.AuthHeader)
	}
	key, err := kr.ReportKey()
	if err != nil {
		return err
	}
	upReq.Header.Set(srv.AuthHeader, key)
	return nil
}

// mcpAccounts resolves the live account set for a provider-backed server from
// the request snapshot: pool virtual ids for multi-account parents, the plain
// name otherwise. nil for auth: none servers.
func mcpAccounts(snap RuntimeSnapshot, srv configdomain.MCPServer) []string {
	if srv.MCPAuthMode() != "provider" {
		return nil
	}
	if ids, ok := snap.PoolIndex[srv.Provider]; ok && len(ids) > 0 {
		return ids
	}
	if _, ok := snap.Providers[srv.Provider]; ok {
		return []string{srv.Provider}
	}
	return nil
}

// mcpAccountLive reports whether the pinned account still exists in this
// request's generation.
func mcpAccountLive(snap RuntimeSnapshot, account string) bool {
	_, ok := snap.Providers[account]
	return ok
}

// mcpNextAccount returns the pool entry after account (round-robin step for
// the one-shot 401 rotation).
func mcpNextAccount(accounts []string, account string) string {
	for i, a := range accounts {
		if a == account {
			return accounts[(i+1)%len(accounts)]
		}
	}
	return accounts[0]
}

// mcpNoAccountErr is the shared message for a provider-backed MCP server
// whose pool is empty (upstream-facing variants prefix it with "mcp %q:").
func mcpNoAccountErr(srv configdomain.MCPServer) string {
	return fmt.Sprintf("provider %q has no logged-in account — run `model-proxy login %s`", srv.Provider, srv.Provider)
}

// mcpAccountGate picks a round-robin account for a provider-backed server and
// tags the response with the live-provider header; ok=false after writing the
// empty-pool 503 terminal response. Callers own the MCPAuthMode gate —
// auth: none servers have no account at all.
func (p *Proxy) mcpAccountGate(w http.ResponseWriter, snap RuntimeSnapshot, name string, srv configdomain.MCPServer) (account string, ok bool) {
	accounts := mcpAccounts(snap, srv)
	if len(accounts) == 0 {
		http.Error(w, fmt.Sprintf("mcp %q: %s", name, mcpNoAccountErr(srv)), http.StatusServiceUnavailable)
		return "", false
	}
	account = accounts[int(p.mcpRR.Add(1))%len(accounts)]
	mcpSetLiveProvider(w, account)
	return account, true
}

// mcpClientFor resolves the upstream HTTP client: the server's own proxy_url
// wins, then the backing provider's, then the global chain. Mirrors
// clientFor's semantics with the MCP-specific override layer.
func (p *Proxy) mcpClientFor(snap RuntimeSnapshot, srv configdomain.MCPServer, account string) *http.Client {
	perProvider := srv.ProxyURL
	if perProvider == "" && account != "" {
		if provCfg, ok := configdomain.ProviderConfig(snap.Cfg, snap.ParentOf, account); ok {
			perProvider = provCfg.ProxyURL
		}
	}
	key, proxyFunc, err := p.proxyResolver.Resolve(snap.Cfg.Proxy, perProvider)
	if err != nil {
		// Load-time validation rejects bad proxy settings; keep the shared
		// transport rather than silently downgrading to direct. Wrap it so the
		// MCP credential headers still never follow a cross-origin redirect.
		return &http.Client{Timeout: 0, Transport: p.client.Transport, CheckRedirect: mcpkg.CheckNoCrossOriginRedirect}
	}
	return &http.Client{Timeout: 0, Transport: p.pooledTransport(key, proxyFunc), CheckRedirect: mcpkg.CheckNoCrossOriginRedirect}
}

// mcpStreamResponse copies the upstream body to the client, flushing after
// every write so SSE frames arrive live, while retaining a bounded prefix for
// the request log. Returns (captured prefix, total bytes, truncated).
func mcpStreamResponse(w http.ResponseWriter, src io.Reader, capBytes int) ([]byte, int64, bool) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	var captured []byte
	var total int64
	truncated := false
	for {
		n, err := src.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if _, werr := w.Write(chunk); werr != nil {
				return captured, total, truncated
			}
			if flusher != nil {
				flusher.Flush()
			}
			total += int64(n)
			if capBytes > 0 {
				if len(captured) < capBytes {
					keep := capBytes - len(captured)
					if keep > n {
						keep = n
					}
					captured = append(captured, chunk[:keep]...)
				}
				truncated = total > int64(capBytes)
			}
		}
		if err != nil {
			return captured, total, truncated
		}
	}
}

// mcpLog projects the exchange into the request log (kind="mcp"). Bodies obey
// the logger's existing truncation policy; the method logged is the JSON-RPC
// method (HTTP verb for GET/DELETE).
func (p *Proxy) mcpLog(name, account string, frame mcpkg.Frame, httpMethod string, status int, started time.Time, requestID string, reqBody, respCaptured []byte, respTotal int64, respTruncated bool, ident *mcpIdentity) {
	// Tool name of a tools/call exchange — parsed once here, then consumed
	// by the per-tool stats, the request-log record and the live end event.
	// Client-facing on purpose: for route exchanges the logged reqBody is
	// the original client frame, so ParseToolCallName yields the canonical
	// route name, not the backend's rewritten name.
	tool := ""
	if frame.Method == "tools/call" {
		tool = mcpkg.ParseToolCallName(reqBody)
	}
	// The per-name call counter is the MCP gateway's own stats channel
	// (deliberately separate from the LLM metrics store — no tokens, no
	// Analytics pollution). Recorded for every terminal exchange regardless
	// of request-log enablement.
	if p.mcpStats != nil {
		latencyMs := time.Since(started).Milliseconds()
		p.mcpStats.Record(name, status, latencyMs)
		if account != "" && account != name {
			p.mcpStats.Record(account, status, latencyMs)
		}
		if tool != "" {
			p.mcpStats.RecordTool(name, tool, status, latencyMs)
			if account != "" && account != name {
				p.mcpStats.RecordTool(account, tool, status, latencyMs)
			}
		}
	}
	// Client attribution: the session-header allowlist wins (clients that
	// stamp their own session id on every call), the local MCP session id
	// groups the exchanges of one client MCP session otherwise. The agent
	// comes from the resolved identity (UA, initialize clientInfo, or the
	// session-bound client label). Resolved BEFORE the logging nil-check:
	// the live end event consumes the write-back regardless of request-log
	// enablement (live events never depend on request_log).
	session := ident.clientSID
	if session == "" {
		session = ident.localSID
	}
	agent := p.mcpAgentFor(ident, frame, reqBody)
	// Hand the resolution to the deferred live end event (publishes after
	// this handler path returns).
	ident.resolvedAgent = agent
	ident.resolvedSession = session
	ident.resolvedTool = tool
	logger := p.mcpLogTarget()
	if logger == nil {
		return
	}
	method := frame.Method
	if method == "" {
		method = httpMethod
		if frame.IsBatch {
			method = httpMethod + " (batch)"
		}
	}
	logger.Enqueue(logger.BuildRecord(requestlog.Input{
		StartedAt:         started,
		RequestID:         requestID,
		Kind:              "mcp",
		Agent:             agent,
		SessionID:         session,
		Protocol:          "mcp",
		Method:            method,
		Path:              "/mcp/" + name,
		Tool:              tool,
		Exposed:           name,
		Provider:          account,
		Status:            status,
		RequestBody:       reqBody,
		ResponseBody:      respCaptured,
		ResponseSize:      respTotal,
		ResponseTruncated: respTruncated,
	}))
}

// mcpLogTarget picks the stream MCP records are written to: the split mcp-
// logger when request_log.mcp_split is on, the shared requests- logger
// otherwise (both nil when request logging is off — nothing is recorded).
func (p *Proxy) mcpLogTarget() *requestlog.Logger {
	if p.mcpReqLog != nil {
		return p.mcpReqLog
	}
	return p.reqLog
}
