package main

import (
	"net/http"

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
	catalog        *modelsDevCatalog
	cache          *responseCache
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
	force     bool
	cacheKey  string
	origBody  []byte

	writer  http.ResponseWriter
	request *http.Request
}

// targetAttempt is the complete contract for executing one resolved target.
// Both normal routing and Fusion synthesis use this object, making additions to
// the execution pipeline explicit without growing a 20+ argument function.
type targetAttempt struct {
	cfg          *Config
	clientProto  string
	backendProto string
	calledModel  string

	target       RouteTarget
	providerCfg  Provider
	providerImpl provider.Provider
	baseURL      string
	upPath       string
	body         []byte

	writer  http.ResponseWriter
	request *http.Request

	agent    string
	cacheKey string
	force    bool
	cache    *responseCache
	log      forwardLogCtx

	contextRetry        func() []RouteTarget
	lastTarget          bool
	viaResponsesVerdict bool
	responseContext     r2cCtx
	responsesHistory    []any
	responsesSession    string
}
