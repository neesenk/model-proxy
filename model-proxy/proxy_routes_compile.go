package main

import (
	"model-proxy/internal/app"
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

// loggedInProviders delegates to internal/app with the production account store.
func loggedInProviders(cfg *Config) map[string]bool {
	return app.LoggedInProviders(cfg, accountStore())
}

// synthesizeImplicitRoutesFrom is the pure, testable core in internal/app.
func synthesizeImplicitRoutesFrom(cfg *Config, loggedIn map[string]bool) (map[string]RouteTarget, []string) {
	return app.SynthesizeImplicitRoutesFrom(cfg, loggedIn)
}

// synthesizeImplicitRoutes derives login status then delegates to the pure core.
func synthesizeImplicitRoutes(cfg *Config) (map[string]RouteTarget, []string) {
	return app.SynthesizeImplicitRoutes(cfg, accountStore())
}
