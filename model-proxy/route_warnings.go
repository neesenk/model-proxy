package main

import (
	"fmt"
	"sort"
	"strings"

	"model-proxy/provider"
)

// route_warnings.go — config-time routing hazards surfaced as MARKERS (startup
// log + routeWarnings channel + doctor/config check), not hard errors. Two
// classes today: reasoning-replay models behind protocol conversion (#9), and
// explicit targets missing a protocol declaration on a provider whose wire
// protocol differs (#10).

// reasoningReplayMarkers are model-name substrings (case-insensitive) known to
// REQUIRE reasoning content echoed back in multi-turn tool-call history
// (DeepSeek V4 thinking, Kimi k2-thinking, Xiaomi MiMo, ...). Name-based and
// advisory — a false positive costs one warning line.
var reasoningReplayMarkers = []string{"reasoner", "thinking", "mimo"}

func reasoningReplayModel(model string) bool {
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
//  1. reasoning-replay models behind protocol conversion (target declares
//     protocol:, so clients of the other protocol convert): the converter
//     currently DROPS thinking/reasoning content — multi-turn tool
//     conversations will hard-400 upstream. Marker only until the reasoning
//     replay cache exists (#9).
//  2. explicit targets missing a protocol declaration on a provider whose
//     wire protocol differs (provider.ProtocolHint): clients of the other
//     protocol send malformed bodies.
func configRoutingWarnings(cfg *Config, expanded map[string][]RouteTarget) []string {
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
			// convert_responses.go): mark honestly which client families can't be
			// served. Inert until such a provider exists.
			if note := provider.WireProtocolNote(provID); note != "" {
				out = append(out, fmt.Sprintf("route %q target %s/%s: %s",
					exposed, t.Provider, t.Model, note))
				continue
			}
			if t.Protocol != "" {
				if reasoningReplayModel(t.Model) {
					out = append(out, fmt.Sprintf("route %q target %s/%s: reasoning-required model behind protocol conversion — thinking/reasoning content is currently dropped, multi-turn tool conversations may fail upstream (400); reasoning replay is not yet implemented",
						exposed, t.Provider, t.Model))
				}
				continue
			}
			if hint := provider.ProtocolHint(provID, t.Model); hint != "" {
				out = append(out, fmt.Sprintf("route %q target %s/%s: no protocol: declared, but %s speaks %s — clients of the other protocol will send malformed bodies; add protocol: %s",
					exposed, t.Provider, t.Model, provID, hint, hint))
			}
		}
	}
	return out
}
