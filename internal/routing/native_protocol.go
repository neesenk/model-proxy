package routing

import (
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/provider"
)

// native_protocol.go — knowledge about which wire protocols a route target's
// provider can serve WITHOUT conversion. The request-time resolution
// ((*Proxy).resolvedBackendProto, internal/app/wirecap.go) consults live
// wire-probe verdicts; offline consumers (takeover) use the same rules with
// the daemon's persisted probe matrix passed in as a ModelProtocolVerdict
// (nil = static config knowledge only). The layers must never disagree on
// declared knowledge: explicit protocol: and ProtocolHint win everywhere.

// NativeProtocols returns the set of client-facing protocols
// ("anthropic"|"openai"|"responses") this target serves natively, i.e. a
// client already speaking one of them is forwarded byte-level without
// conversion — static config knowledge only (NativeProtocolsWithVerdict
// without probe data). An empty set means "unknown" — callers must treat it
// as abstaining, not as "everything converts".
func NativeProtocols(cfg *configdomain.Config, t configdomain.RouteTarget) map[string]bool {
	return NativeProtocolsWithVerdict(cfg, t, nil)
}

// Tri is a tri-state per-leg probe verdict.
type Tri uint8

const (
	TriUnknown Tri = iota
	TriYes
	TriNo
)

// ModelProtocolVerdict is one model's per-leg protocol support on its
// provider, probed live by the daemon (persisted in model_caps.json) and
// mapped into this dependency-free shape for offline consumers.
type ModelProtocolVerdict struct {
	Chat, Anthropic, Responses Tri
}

// NativeProtocolsWithVerdict resolves like NativeProtocols, enriched by the
// model's live probe verdict (nil = static only). Declared knowledge still
// wins: explicit protocol: and ProtocolHint short-circuit before any verdict
// is consulted. Without them, each probed leg settles its protocol: yes ⇒
// native (even when static rules would not claim it — responses is never
// claimed statically), no ⇒ NOT native (even when its endpoint IS declared —
// a provider's several base URLs do not all serve every model, which is
// exactly what the per-model probe exists for); an unknown leg falls back to
// the static endpoint declaration.
func NativeProtocolsWithVerdict(cfg *configdomain.Config, t configdomain.RouteTarget, v *ModelProtocolVerdict) map[string]bool {
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
	if v == nil {
		out := map[string]bool{}
		if prov.AnthropicBaseURL != "" {
			out["anthropic"] = true
		}
		if prov.OpenAIBaseURL != "" {
			out["openai"] = true
		}
		return out
	}
	out := map[string]bool{}
	settle := func(leg Tri, proto string, declared bool) {
		switch leg {
		case TriYes:
			out[proto] = true
		case TriUnknown:
			if declared {
				out[proto] = true
			}
		}
	}
	settle(v.Chat, "openai", prov.OpenAIBaseURL != "")
	settle(v.Anthropic, "anthropic", prov.AnthropicBaseURL != "")
	settle(v.Responses, "responses", false)
	return out
}
