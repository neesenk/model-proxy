package routing

import (
	"sort"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/provider"
)

// DeriveRoutesFrom is the pure, testable core of route derivation: every
// provider model is exposed under its exposed name (alias applied) and the
// providers serving it aggregate into one multi-target route. Each target
// carries the provider's priority and keeps the REAL upstream model name (the
// alias only renames the exposed key), plus the wire-protocol hint for
// providers whose API shape differs from the client's (codex: responses).
// Targets are ordered by (priority, provider name) so the table is
// deterministic; explicit cfg.Routes entries override derived ones per name
// (see RouteTable).
func DeriveRoutesFrom(cfg *configdomain.Config) map[string][]configdomain.RouteTarget {
	out := map[string][]configdomain.RouteTarget{}
	for name, prov := range cfg.Providers {
		for _, m := range prov.Models {
			tgt := configdomain.RouteTarget{Provider: name, Model: m, Priority: prov.Priority}
			if hint := provider.ProtocolHint(prov.Provider, m); hint != "" {
				tgt.Protocol = hint
			}
			exposed := prov.ExposedModelName(m)
			out[exposed] = append(out[exposed], tgt)
		}
	}
	for _, targets := range out {
		sort.Slice(targets, func(i, j int) bool {
			if targets[i].Priority != targets[j].Priority {
				return targets[i].Priority < targets[j].Priority
			}
			return targets[i].Provider < targets[j].Provider
		})
	}
	return out
}

// FillTargetPriorities backfills the provider's priority into explicit route
// targets that omit their own (priority is now provider-level; an explicit
// target only sets priority to override it).
func FillTargetPriorities(cfg *configdomain.Config, targets []configdomain.RouteTarget) []configdomain.RouteTarget {
	out := make([]configdomain.RouteTarget, len(targets))
	for i, t := range targets {
		if t.Priority == 0 {
			if prov, ok := cfg.Providers[t.Provider]; ok {
				t.Priority = prov.Priority
			}
		}
		out[i] = t
	}
	return out
}

// RouteTable is the complete callable route table: derived routes for every
// exposed provider model, with explicit cfg.Routes entries overriding the
// derived ones wholesale (fusion targets, protocol overrides, special
// ordering). CLI listing / probing uses this; the app Proxy uses
// BuildExpandedRoutes with the same inputs plus pool fan-out.
func RouteTable(cfg *configdomain.Config) map[string][]configdomain.RouteTarget {
	out := DeriveRoutesFrom(cfg)
	for exposed, targets := range cfg.Routes {
		out[exposed] = FillTargetPriorities(cfg, append([]configdomain.RouteTarget(nil), targets...))
	}
	return out
}

// The backend protocol for a route target is resolved at request time by the
// app's (*Proxy).resolvedBackendProto (internal/app/wirecap.go): declared
// protocol: > ProtocolHint > wire probe verdict > client-protocol passthrough.
// Used by forward, fusion, and shadow so the resolution rule is one place.

// BuildExpandedRoutes returns routes with pooled targets fanned out to their
// virtual children: a target whose provider is a pooled parent (key present in
// poolIndex) is replaced by its N virtuals, each with the SAME Model + Priority
// as the original; non-pooled targets pass through unchanged. Routes with no
// pooled targets are returned as-is (same slice contents). Derived routes
// (auto-aggregated from provider model lists) merge under explicit routes and
// fan out the same way.
func BuildExpandedRoutes(
	cfg *configdomain.Config,
	derived map[string][]configdomain.RouteTarget,
	expand func(configdomain.RouteTarget) []configdomain.RouteTarget,
) map[string][]configdomain.RouteTarget {
	out := make(map[string][]configdomain.RouteTarget, len(cfg.Routes))
	for exposed, targets := range cfg.Routes {
		var exp []configdomain.RouteTarget
		for _, t := range FillTargetPriorities(cfg, targets) {
			exp = append(exp, expand(t)...)
		}
		out[exposed] = exp
	}
	for exposed, ts := range derived {
		if _, explicit := out[exposed]; explicit {
			continue
		}
		var exp []configdomain.RouteTarget
		for _, t := range ts {
			exp = append(exp, expand(t)...)
		}
		out[exposed] = exp
	}
	return out
}

// RouteModelsForProvider returns the sorted distinct upstream model ids that
// routes assign to one provider (explicit + derived targets).
func RouteModelsForProvider(cfg *configdomain.Config, provName string) []string {
	seen := map[string]bool{}
	var out []string
	for _, targets := range RouteTable(cfg) {
		for _, t := range targets {
			if t.Provider == provName && !seen[t.Model] {
				seen[t.Model] = true
				out = append(out, t.Model)
			}
		}
	}
	sort.Strings(out)
	return out
}
