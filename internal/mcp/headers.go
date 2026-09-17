package mcp

import "net/http"

// Header forwarding policy for the MCP gateway. The allowlists are
// deliberately narrow: everything the client or upstream says outside them is
// dropped, so provider credentials, cookies and hop-by-hop headers can never
// leak across the boundary in either direction.

// clientToUpstream lists the end-to-end headers copied from the inbound
// client request to the upstream request. Authentication is injected
// separately by the caller; Mcp-Session-Id is rewritten separately.
var clientToUpstream = []string{
	"Accept",
	"Content-Type",
	"MCP-Protocol-Version",
}

// upstreamToClient lists the response headers copied back to the client.
// Mcp-Session-Id is handled separately (rewritten to the local id).
var upstreamToClient = []string{
	"Content-Type",
	"Cache-Control",
	"MCP-Protocol-Version",
}

// CopyClientHeaders copies the allowlisted inbound headers onto the upstream
// request. Auth and session headers are NOT copied here.
func CopyClientHeaders(dst, src http.Header) {
	for _, h := range clientToUpstream {
		if v := src.Get(h); v != "" {
			dst.Set(h, v)
		}
	}
}

// CopyUpstreamHeaders copies the allowlisted upstream response headers onto
// the client response. The session header is excluded — the caller rewrites
// it to the local session id (or drops it).
func CopyUpstreamHeaders(dst, src http.Header) {
	for _, h := range upstreamToClient {
		if v := src.Get(h); v != "" {
			dst.Set(h, v)
		}
	}
}
