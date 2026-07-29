package main

import (
	"net/http"

	responsecache "model-proxy/internal/cache"
	"model-proxy/internal/catalog"
	"model-proxy/internal/targetexec"
	"model-proxy/provider"
)

// runtimeSnapshot is one immutable view of reload-swapped runtime dependencies.
// A request captures it once and keeps using the same config generation through
// scheduling, failover, protocol conversion, and any cooldown retry.
type runtimeSnapshot struct {
	cfg            *Config
	generation     uint64
	providers      map[string]provider.Provider
	poolIndex      map[string][]string
	parentOf       map[string]string
	expandedRoutes map[string][]RouteTarget
	catalog        *catalog.Catalog
	cache          *responsecache.Store
	shadow         *shadowRuntime
}

// snapshotRuntime captures every reload-owned dependency under one brief read
// lock. Maps are generation-owned and never mutated in place: reload builds new
// maps and swaps the pointers, so an in-flight request may safely retain them.
func (p *Proxy) snapshotRuntime() runtimeSnapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return runtimeSnapshot{
		cfg:            p.cfg,
		generation:     p.configGeneration.Load(),
		providers:      p.providers,
		poolIndex:      p.poolIndex,
		parentOf:       p.parentOf,
		expandedRoutes: p.expandedRoutes,
		catalog:        p.catalog,
		cache:          p.cache,
		shadow:         p.shadow.Load(),
	}
}

// serveRequest is the stable input to one scheduling/failover pass. It groups
// request identity, protocol/body data, routing inputs, and the runtime snapshot
// instead of threading them as an ever-growing positional parameter list.
type serveRequest struct {
	runtime runtimeSnapshot

	proto       string
	upPath      string
	exposed     string
	calledModel string
	sessionKey  string
	agent       string
	requestID   string

	targets   []RouteTarget
	routeKeys map[string]bool
	force     bool // active pin: exclusive and bypasses circuit health
	// forcedProvider is the request-scoped replay override. It is exclusive for
	// routing, but unlike a pin it does not bypass circuit health.
	forcedProvider string
	cacheKey       string
	origBody       []byte

	writer  http.ResponseWriter
	request *http.Request
}

// forwardLogCtx carries stable request identity into target execution. Reload
// generation remains owned by targetexec.Attempt.Runtime and is not duplicated
// in this observation scope.
type forwardLogCtx struct {
	requestID string
	attempt   int
	exposed   string
	origBody  []byte
}

func targetLogContext(context forwardLogCtx) targetexec.LogContext {
	return targetexec.LogContext{
		RequestID:    context.requestID,
		Attempt:      context.attempt,
		Exposed:      context.exposed,
		OriginalBody: context.origBody,
	}
}

// newTargetAttempt is the single assembly point shared by normal routing and
// Fusion synthesis. Request rewriting, state expansion, and protocol conversion
// deliberately remain outside this factory because those operations have
// caller-specific semantics and may fail before an attempt can be executed.
func newTargetAttempt(
	runtime runtimeSnapshot,
	plan targetexec.Plan,
	exchange targetexec.Exchange,
	scope targetexec.Scope,
	policy targetexec.Policy,
) targetexec.Attempt {
	var scheduling Scheduling
	if runtime.cfg != nil {
		scheduling = runtime.cfg.Scheduling
	}
	return targetexec.NewAttempt(
		targetexec.Runtime{
			Scheduling: scheduling,
			Generation: runtime.generation,
			Cache:      runtime.cache,
		},
		plan,
		exchange,
		scope,
		policy,
	)
}
