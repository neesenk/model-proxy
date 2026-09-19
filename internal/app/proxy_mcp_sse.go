// proxy_mcp_sse.go — legacy HTTP+SSE transport (protocol 2024-11-05) on the
// pinned /mcp/ surface. The client GETs /mcp/<name>: the gateway opens the
// upstream SSE stream, rewrites the first endpoint event's POST URL back to
// /mcp/<name>?mps=<local session>, and binds both channels to one account
// (the session entry stores the full upstream endpoint URL). The legacy
// session dies with its GET stream. Aggregated routes do not support legacy
// sse backends (rejected at config validation).
package app

import (
	"fmt"
	"net/http"
	"time"

	configdomain "model-proxy/internal/config"
	mcpkg "model-proxy/internal/mcp"
)

// serveMCPLegacySSE handles GET /mcp/<name> for transport: sse servers.
func (p *Proxy) serveMCPLegacySSE(w http.ResponseWriter, r *http.Request, name string, srv configdomain.MCPServer, snap RuntimeSnapshot, started time.Time, requestID string, ident *mcpIdentity) {
	account := ""
	if srv.MCPAuthMode() == "provider" {
		var ok bool
		account, ok = p.mcpAccountGate(w, snap, name, srv)
		if !ok {
			return
		}
	}
	resp, capBytes, err := p.mcpDo(r.Context(), snap, srv, account, mcpCallOpts{
		method:        http.MethodGet,
		clientHeaders: r.Header,
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("mcp %q: upstream: %v", name, err), http.StatusBadGateway)
		p.mcpLog(name, account, mcpkg.Frame{}, r.Method, http.StatusBadGateway, started, requestID, nil, nil, 0, false, ident)
		return
	}
	defer resp.Body.Close()
	mcpkg.CopyUpstreamHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if resp.StatusCode != http.StatusOK {
		captured, total, truncated := mcpStreamResponse(w, resp.Body, capBytes)
		p.mcpLog(name, account, mcpkg.Frame{}, r.Method, resp.StatusCode, started, requestID, nil, captured, total, truncated, ident)
		return
	}
	// Rewrite the upstream's endpoint event back to the gateway, binding the
	// POST channel to this stream's account via a local session.
	var sid string
	rewrite := func(data string) string {
		upstreamURL := mcpkg.ResolveEndpointURL(srv.URL, data)
		if upstreamURL == "" {
			return data
		}
		sid = p.mcpSessions.Put(name, account, upstreamURL)
		return "/mcp/" + name + "?mps=" + sid
	}
	captured, total, truncated := mcpStreamResponse(w, mcpkg.NewEndpointRewriter(resp.Body, rewrite), capBytes)
	if sid != "" {
		// The legacy session's correlation lives on the GET stream; once it
		// ends (client disconnect or upstream close), POSTs to it are dead.
		p.mcpSessions.Delete(sid)
	}
	p.mcpLog(name, account, mcpkg.Frame{}, r.Method, resp.StatusCode, started, requestID, nil, captured, total, truncated, ident)
}
