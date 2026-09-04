package routing

import (
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/provider"
)

// native_protocol.go — static (config-only) knowledge about which wire
// protocols a route target's provider can serve WITHOUT conversion. The
// request-time resolution ((*Proxy).resolvedBackendProto, internal/app/
// wirecap.go) additionally consults live wire-probe verdicts; CLI one-shots
// like takeover run before any probe state exists, so they use this static
// subset. The two must never disagree on declared knowledge: explicit
// protocol: and ProtocolHint win in both.

// NativeProtocols returns the set of client-facing protocols
// ("anthropic"|"openai"|"responses") this target serves natively, i.e. a
// client already speaking one of them is forwarded byte-level without
// conversion. Resolution order mirrors resolvedBackendProto minus probing:
//
//   - explicit target protocol: pins the backend (derived routes already
//     carry ProtocolHint materialized into this field);
//   - ProtocolHint (covers explicit routes that omit the declaration);
//   - declared provider endpoints: anthropic_base_url ⇒ anthropic is
//     natively served (anthropic support is declared, never probed — see
//     docs/architecture/protocol-conversion.md), openai_base_url ⇒ openai
//     chat is natively served. responses is never claimed statically: it is
//     only knowable via probe verdict or hint.
//
// An empty set means "unknown" — callers must treat it as abstaining, not as
// "everything converts".
func NativeProtocols(cfg *configdomain.Config, t configdomain.RouteTarget) map[string]bool {
	if t.Protocol != "" {
		return map[string]bool{t.Protocol: true}
	}
	prov, ok := cfg.Providers[t.Provider]
	if !ok {
		return nil
	}
	if hint := provider.ProtocolHint(prov.Provider, t.Model); hint != "" {
		return map[string]bool{hint: true}
	}
	out := map[string]bool{}
	if prov.AnthropicBaseURL != "" {
		out["anthropic"] = true
	}
	if prov.OpenAIBaseURL != "" {
		out["openai"] = true
	}
	return out
}
