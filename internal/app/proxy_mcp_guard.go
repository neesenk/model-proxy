// proxy_mcp_guard.go — the outbound secret scan for MCP gateway bodies
// (guard.mcp_secrets). Compact sibling of the forward guard path: same
// generation-frozen Scanner, same action semantics, same audit red line
// (pattern NAMES only, matched content never enters any record). MCP bodies
// get secrets-only scanning — the sensitive-path rules target coding-agent
// prompts, not JSON-RPC tool calls.
package app

import (
	"model-proxy/internal/forward"
)

// mcpGuardScan applies guard.mcp_secrets to one outbound MCP body. It returns
// the body to forward (redacted when the action says so) and whether the
// request may proceed; a block action reports false and the caller answers
// 400 without revealing what matched.
func (p *Proxy) mcpGuardScan(snap RuntimeSnapshot, name, requestID string, body []byte) ([]byte, bool) {
	action := snap.Cfg.Guard.MCPSecretsAction()
	if action == "off" || snap.Guard == nil || len(body) == 0 {
		return body, true
	}
	names := snap.Guard.Scan(body)
	if len(names) == 0 {
		return body, true
	}
	// Audit against the request's OWN generation logger (late enqueues to a
	// swapped-out logger may drop — same semantics as forward, intentional
	// behaviors item 19).
	forward.AuditGuardHit(snap.SecLog, forward.GuardAuditHit{
		Kind:      "secret",
		RequestID: requestID,
		Proto:     "mcp",
		Exposed:   name,
		Names:     names,
		Action:    action,
	})
	switch action {
	case "block":
		return nil, false
	case "redact":
		return snap.Guard.Redact(body), true
	default: // log
		return body, true
	}
}
