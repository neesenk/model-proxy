package app

import ()

// buildExpandedRoutes delegates to internal/BuildExpandedRoutes with the
// pool fan-out from the unified routing resolver. Caller holds p.mu (write) —
// in NewProxy / reload, after buildProviders has populated poolIndex.
func (p *Proxy) buildExpandedRoutes() map[string][]RouteTarget {
	return BuildExpandedRoutes(p.cfg, p.implicitRoutes, p.expandTarget)
}

// expandTarget fans a single route target out across a pooled provider's
// virtuals via the unified routing resolver front door.
func (p *Proxy) expandTarget(t RouteTarget) []RouteTarget {
	return newResolver(p, p.providers, p.poolIndex).Expand(t)
}

// loggedInProviders delegates to internal/app with the production account store.
func loggedInProviders(cfg *Config) map[string]bool {
	return LoggedInProviders(cfg, AccountStore())
}

// synthesizeImplicitRoutesFrom is the pure, testable core in internal/app.
func synthesizeImplicitRoutesFrom(cfg *Config, loggedIn map[string]bool) (map[string]RouteTarget, []string) {
	return SynthesizeImplicitRoutesFrom(cfg, loggedIn)
}

// synthesizeImplicitRoutes derives login status then delegates to the pure core.
func synthesizeImplicitRoutes(cfg *Config) (map[string]RouteTarget, []string) {
	return SynthesizeImplicitRoutes(cfg, AccountStore())
}
