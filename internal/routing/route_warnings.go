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
	// Route-policy bands whose target/grade the route does not serve can never
	// apply — surface the typo at startup instead of letting the band silently do
	// nothing. For graded policies a grade: reference only needs to exist in the
	// policy's grades; a target reference must uniquely fall into one grade (and
	// also be served by the route). For non-graded policies the target must simply
	// be served by the route.
	policyRoutes := make([]string, 0, len(cfg.RoutePolicies))
	for r := range cfg.RoutePolicies {
		policyRoutes = append(policyRoutes, r)
	}
	sort.Strings(policyRoutes)
	for _, route := range policyRoutes {
		policy := cfg.RoutePolicies[route]
		for i, band := range policy.Bands {
			if policy.HasGrades() {
				if band.Grade != "" {
					if _, ok := policy.Grades[band.Grade]; ok {
						continue
					}
					out = append(out, fmt.Sprintf("route_policy %q band %d: grade %q is not declared in grades — the band can never match; add the grade or fix the name",
						route, i, band.Grade))
					continue
				}
				if msg := gradedBandTargetWarning(route, i, band.Target, expanded[route], policy.Grades); msg != "" {
					out = append(out, msg)
				}
				continue
			}
			if band.Grade != "" {
				out = append(out, fmt.Sprintf("route_policy %q band %d: grade %q used but route_policy has no grades declared — the band can never match",
					route, i, band.Grade))
				continue
			}
			if bandServed(expanded[route], band.Target) {
				continue
			}
			out = append(out, fmt.Sprintf("route_policy %q band %d: target %s/%s is not served by this route — the band can never match; add the target to the route or fix the name",
				route, i, band.Target.Provider, band.Target.Model))
		}
	}
	return out
}

// bandServed reports whether the route's config-level target list contains the
// band's target. Pooled virtual IDs do not exist at config time, so plain
// provider/model equality is the right check here.
func bandServed(targets []configdomain.RouteTarget, want configdomain.RouteTarget) bool {
	for _, t := range targets {
		if t.Provider == want.Provider && t.Model == want.Model {
			return true
		}
	}
	return false
}

// gradedBandTargetWarning checks whether a target-referencing band in a graded
// policy can ever match. It returns an empty string when the target is fine. A
// target must both be served by the route and uniquely belong to one declared
// grade; validation already rejects ambiguous/ungraded targets, but the warning
// path is defensive.
func gradedBandTargetWarning(route string, i int, target configdomain.RouteTarget, routeTargets []configdomain.RouteTarget, grades map[string][]configdomain.RouteTarget) string {
	if !bandServed(routeTargets, target) {
		return fmt.Sprintf("route_policy %q band %d: target %s/%s is not served by this route — the band can never match; add the target to the route or fix the name",
			route, i, target.Provider, target.Model)
	}
	grade, ambiguous := GradeForTarget(target, grades)
	if ambiguous {
		return fmt.Sprintf("route_policy %q band %d: target %s/%s appears in multiple grades — the band can never match; use grade: <name>",
			route, i, target.Provider, target.Model)
	}
	if grade == "" {
		return fmt.Sprintf("route_policy %q band %d: target %s/%s does not belong to any grade — the band can never match; add it to a grade or use grade: <name>",
			route, i, target.Provider, target.Model)
	}
	return ""
}
