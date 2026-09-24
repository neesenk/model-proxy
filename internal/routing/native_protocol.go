package routing

import (
	"sort"

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

// isChatClientProtocol reports whether proto is one of the chat protocols
// every takeover-configured agent speaks (anthropic|openai|responses).
// Anything else is a different API family with no chat-protocol conversion
// (today: decisions — fail-closed stubs in internal/protocol), so it can
// never be served to those clients.
func isChatClientProtocol(proto string) bool {
	switch proto {
	case "anthropic", "openai", "responses":
		return true
	}
	return false
}

// ChatReachableRoutes filters a route table down to the exposed models a
// chat-protocol client can actually call. A model survives while ANY of its
// targets is servable over a client-facing chat protocol or abstains
// (unknown native set — it may still convert or passthrough); a model whose
// every target natively speaks only protocols outside
// anthropic|openai|responses (decisions-only providers, e.g. typesafe's jev)
// is unreachable through every takeover client and is dropped, its exposed
// name returned in dropped so callers can report why it disappeared.
// Static knowledge only (NativeProtocols without probe verdicts): explicit
// protocol: and ProtocolHint win, matching offline consumers' resolution
// order. A provider whose chat legs the live probe later denies is a
// per-model reachability question, not this filter's.
func ChatReachableRoutes(cfg *configdomain.Config, routes map[string][]configdomain.RouteTarget) (map[string][]configdomain.RouteTarget, []string) {
	kept := make(map[string][]configdomain.RouteTarget, len(routes))
	var dropped []string
	for exposed, targets := range routes {
		reachable := len(targets) == 0 // nothing to serve — abstain, not dead
		for _, t := range targets {
			set := NativeProtocols(cfg, t)
			if len(set) == 0 {
				reachable = true // abstains — may still convert or passthrough
				break
			}
			for proto := range set {
				if isChatClientProtocol(proto) {
					reachable = true
					break
				}
			}
			if reachable {
				break
			}
		}
		if reachable {
			kept[exposed] = targets
		} else {
			dropped = append(dropped, exposed)
		}
	}
	sort.Strings(dropped)
	return kept, dropped
}
