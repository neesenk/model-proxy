package routing

import (
	"fmt"
	"sort"
	"strings"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/protocol"
	"model-proxy/internal/provider"
)

// route_warnings.go — config-time routing hazards surfaced as MARKERS (startup
// log + app routeWarnings channel + doctor/config check), not hard errors. One
// class today: reasoning-replay models behind anthropic↔chat protocol
// conversion (#9).

// reasoningReplayMarkers are model-name substrings (case-insensitive) known to
// REQUIRE reasoning content echoed back in multi-turn tool-call history
// (DeepSeek V4 thinking, Kimi k2-thinking, Xiaomi MiMo, ...). Name-based and
// advisory — a false positive costs one warning line.
var reasoningReplayMarkers = []string{"reasoner", "thinking", "mimo"}

func ReasoningReplayModel(model string) bool {
	l := strings.ToLower(model)
	for _, m := range reasoningReplayMarkers {
		if strings.Contains(l, m) {
			return true
		}
	}
	return false
}

// configRoutingWarnings inspects expanded (explicit ∪ implicit) routes for:
//
//  1. reasoning-replay models behind anthropic↔chat conversion (target
//     declares protocol: anthropic/openai, so clients of the other protocol
//     convert): that converter currently DROPS thinking/reasoning content —
//     multi-turn tool conversations will hard-400 upstream. Marker only until
//     the reasoning replay cache exists (#9). responses targets preserve
//     reasoning (docs/architecture/protocol-conversion.md), so a
//     protocol:responses declaration does NOT warn.
//
// A target on a provider whose native wire differs from the client's but without
// an explicit protocol: declaration (e.g. codex→responses) does NOT warn: the
// forward path auto-resolves it via resolvedBackendProto/ProtocolHint, so it
// converts without user action.
func ConfigRoutingWarnings(cfg *configdomain.Config, expanded map[string][]configdomain.RouteTarget) []string {
	var out []string
	routes := make([]string, 0, len(expanded))
	for r := range expanded {
		routes = append(routes, r)
	}
	sort.Strings(routes)
	for _, exposed := range routes {
		for _, t := range expanded[exposed] {
			provID := ""
			if prov, ok := cfg.Providers[t.Provider]; ok {
				provID = prov.Provider
			}
			// A provider whose wire protocol our system can neither passthrough
			// nor convert (none today — codex/Responses is now converted in
			// internal/protocol/convert_responses.go): mark honestly which client
			// families can't be served. Inert until such a provider exists.
			if note := provider.WireProtocolNote(provID); note != "" {
				out = append(out, fmt.Sprintf("route %q target %s/%s: %s",
					exposed, t.Provider, t.Model, note))
				continue
			}
			if t.Protocol != "" {
				// Only the anthropic↔chat converter drops thinking/reasoning;
				// a responses target preserves it, so protocol:responses must
				// NOT warn (the message "currently dropped" would be wrong).
				if p, ok := protocol.Parse(t.Protocol); ok && p != protocol.Responses && ReasoningReplayModel(t.Model) {
					out = append(out, fmt.Sprintf("route %q target %s/%s: reasoning-required model behind protocol conversion — thinking/reasoning content is currently dropped, multi-turn tool conversations may fail upstream (400); reasoning replay is not yet implemented",
						exposed, t.Provider, t.Model))
				}
				continue
			}
			// t.Protocol == "": the forward path auto-resolves the backend
			// protocol via ProtocolHint (resolvedBackendProto), so a target on a
			// provider whose native wire differs from the client's (codex→
			// responses) converts without an explicit protocol: declaration. No
			// warning needed.
		}
	}
	return out
}
