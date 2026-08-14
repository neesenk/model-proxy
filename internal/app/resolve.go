package app

import (
	"time"

	"model-proxy/internal/provider"
	"model-proxy/internal/routing"
)

// resolver is the root alias for routing.Resolver; resolution logic lives in
// internal/routing. Proxy satisfies routing.ResolverState via the two
// delegation methods below.
type resolver = routing.Resolver

type resolverState = routing.ResolverState

func newResolver(
	state resolverState,
	providers map[string]provider.Provider,
	poolIndex map[string][]string,
	generations ...uint64,
) *resolver {
	return routing.NewResolver(state, providers, poolIndex, generations...)
}

func (p *Proxy) ResolverSpreadStart(parent string, n int, generation uint64) int {
	return p.runtimeState.ResolverSpreadStart(parent, n, generation)
}

func (p *Proxy) TargetHealthy(virtual, model string, now time.Time) bool {
	return p.runtimeState.TargetHealthy(virtual, model, now)
}
