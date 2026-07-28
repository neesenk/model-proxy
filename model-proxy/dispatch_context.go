package main

import (
	"net/http"

	"model-proxy/internal/protocol"
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
	force     bool
	cacheKey  string
	origBody  []byte

	writer  http.ResponseWriter
	request *http.Request
}

// attemptExchange is the transport exchange for one target attempt. The body is
// already model-rewritten, responses-state-expanded, and protocol-converted by
// the caller; constructing an attempt never mutates it.
type attemptExchange struct {
	request *http.Request
	writer  http.ResponseWriter
	body    []byte
}

// attemptScope carries request identity and observation context that is neither
// part of target planning nor execution policy.
type attemptScope struct {
	calledModel string
	agent       string
	cacheKey    string
	log         forwardLogCtx

	responseContext  protocol.ResponseContext
	responsesHistory []any
	responsesSession string
}

// attemptPolicy contains the few scheduling decisions that affect one target
// execution after planning has completed.
type attemptPolicy struct {
	force        bool
	lastTarget   bool
	contextRetry func() []RouteTarget
}

// targetAttempt is the complete contract for executing one resolved target.
// Runtime- and plan-owned facts have exactly one source: cfg/cache/generation
// come from runtime, while target/provider/protocol/URL/path come from plan.
type targetAttempt struct {
	runtime  runtimeSnapshot
	plan     targetPlan
	exchange attemptExchange
	scope    attemptScope
	policy   attemptPolicy
}

// newTargetAttempt is the single assembly point shared by normal routing and
// Fusion synthesis. Request rewriting, state expansion, and protocol conversion
// deliberately remain outside this factory because those operations have
// caller-specific semantics and may fail before an attempt can be executed.
func newTargetAttempt(
	runtime runtimeSnapshot,
	plan targetPlan,
	exchange attemptExchange,
	scope attemptScope,
	policy attemptPolicy,
) targetAttempt {
	return targetAttempt{
		runtime:  runtime,
		plan:     plan,
		exchange: exchange,
		scope:    scope,
		policy:   policy,
	}
}
