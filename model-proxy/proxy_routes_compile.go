package main

import (
	"fmt"
	"sort"
	"strings"

	"model-proxy/internal/accounts"
	"model-proxy/provider"
)

// buildExpandedRoutes returns routes with pooled targets fanned out to their
// virtual children: a target whose provider is a pooled parent (key present in
// poolIndex) is replaced by its N virtuals, each with the SAME Model + Priority
// as the original; non-pooled targets pass through unchanged. Routes with no
// pooled targets are returned as-is (same slice contents).
//
// Caller holds p.mu (write) — in NewProxy / reload, after buildProviders has
// populated poolIndex. forward + scheduleStatus read the result via the
// expandedRoutes field instead of cfg.Routes, so the fan-out is transparent to
// the scheduling/circuit code (which operates on provider names).
func (p *Proxy) buildExpandedRoutes() map[string][]RouteTarget {
	out := make(map[string][]RouteTarget, len(p.cfg.Routes))
	for exposed, targets := range p.cfg.Routes {
		var exp []RouteTarget
		for _, t := range targets {
			exp = append(exp, p.expandTarget(t)...)
		}
		out[exposed] = exp
	}
	// Merge implicit routes (auto-derived for unrouted models served by a logged-in
	// provider). Explicit routes win; implicit targets the parent so pool fan-out
	// applies via expandTarget too.
	for exposed, t := range p.implicitRoutes {
		if _, explicit := out[exposed]; explicit {
			continue
		}
		out[exposed] = p.expandTarget(t)
	}
	return out
}

// expandTarget fans a single route target out across a pooled provider's virtuals
// (same Model/Priority/Protocol); non-pooled targets pass through unchanged.
// Thin wrapper over the unified resolver (resolve.go) so routing goes through
// the same config-target → runnable-virtual front door as Fusion and Shadow.
func (p *Proxy) expandTarget(t RouteTarget) []RouteTarget {
	return newResolver(p, p.providers, p.poolIndex).Expand(t)
}

// loggedInProviders returns the provider parents whose authoritative account
// snapshot can produce a runtime credential. Used to decide which providers can
// serve an implicit route; corrupt/empty plural pools and static legacy files
// remain fail-closed exactly as they do in buildProviders.
func loggedInProviders(cfg *Config) map[string]bool {
	out := map[string]bool{}
	for name, prov := range cfg.Providers {
		if prov.Provider == "aqp" || prov.Provider == "codex" {
			// OAuth/SSO login state belongs to their own stores; an unrelated
			// API-key pool must never make them eligible for implicit routes.
			continue
		}
		snapshot, err := accountStore().LoadSnapshot(name, prov.Provider)
		if err != nil || len(snapshot.Pool.Accounts) == 0 {
			continue
		}
		if snapshot.Source == accounts.SourcePlural ||
			(snapshot.Source == accounts.SourceLegacy && prov.Provider != "static") {
			out[name] = true
		}
	}
	return out
}

// synthesizeImplicitRoutesFrom is the pure, testable core. For each model name
// that is NOT already an explicit route key AND is served by ≥1 logged-in
// provider, it creates a single-target implicit route to the alphabetically-first
// logged-in provider that serves it; if >1 logged-in provider serves it, the
// others are dropped and a warning is emitted. Explicit routes always win.
func synthesizeImplicitRoutesFrom(cfg *Config, loggedIn map[string]bool) (implicit map[string]RouteTarget, warnings []string) {
	// model → sorted list of logged-in providers that serve it
	claims := map[string][]string{}
	for name, prov := range cfg.Providers {
		if !loggedIn[name] {
			continue
		}
		for _, m := range prov.Models {
			claims[m] = append(claims[m], name)
		}
	}
	implicit = map[string]RouteTarget{}
	for model, provs := range claims {
		if _, explicit := cfg.Routes[model]; explicit {
			continue // explicit route wins
		}
		sort.Strings(provs)
		tgt := RouteTarget{Provider: provs[0], Model: model, Priority: 1}
		// Fill the wire-protocol hint for providers whose API shape differs from
		// the client's (codex: responses) — without it an anthropic/chat client
		// would send an unconverted body to a responses-only upstream.
		if hint := provider.ProtocolHint(cfg.Providers[provs[0]].Provider, model); hint != "" {
			tgt.Protocol = hint
		}
		implicit[model] = tgt
		if len(provs) > 1 {
			warnings = append(warnings, fmt.Sprintf("model %q served by %d logged-in providers (%s); auto-routing to %s — add an explicit route to choose",
				model, len(provs), strings.Join(provs, ", "), provs[0]))
		}
	}
	return implicit, warnings
}

// The backend protocol for a route target is resolved by
// (*Proxy).resolvedBackendProto (wirecap.go): declared protocol: >
// ProtocolHint > wire probe verdict > client-protocol passthrough. Used by
// forward, fusion, and shadow so the resolution rule is one place.

// synthesizeImplicitRoutes derives login status then delegates to the pure core.
func synthesizeImplicitRoutes(cfg *Config) (map[string]RouteTarget, []string) {
	return synthesizeImplicitRoutesFrom(cfg, loggedInProviders(cfg))
}
