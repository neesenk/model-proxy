package app

// buildExpandedRoutes delegates to internal/BuildExpandedRoutes with the
// pool fan-out from the unified routing resolver. Caller holds p.mu (write) —
// in NewProxy / reload, after buildProviders has populated poolIndex.
func (p *Proxy) buildExpandedRoutes() map[string][]RouteTarget {
	return BuildExpandedRoutes(p.cfg, p.derivedRoutes, p.expandTarget)
}

// routeKeySet derives the schedule view's route-name key set from the expanded
// route map. Built once per generation so the request hot path can share it.
func routeKeySet(expanded map[string][]RouteTarget) map[string]bool {
	keys := make(map[string]bool, len(expanded))
	for k := range expanded {
		keys[k] = true
	}
	return keys
}

// expandTarget fans a single route target out across a pooled provider's
// virtuals via the unified routing resolver front door.
func (p *Proxy) expandTarget(t RouteTarget) []RouteTarget {
	return newResolver(p, p.providers, p.poolIndex).Expand(t)
}
