package app

import (
	"net/http"
	"time"

	"model-proxy/internal/observe/requestlog"

	responsecache "model-proxy/internal/cache"
	"model-proxy/internal/catalog"
	"model-proxy/internal/guard"
	"model-proxy/internal/provider"
	"model-proxy/internal/shadow"
	"model-proxy/internal/targetexec"
)

// RuntimeSnapshot is one immutable view of reload-swapped runtime dependencies.
// A request captures it once and keeps using the same config generation through
// scheduling, failover, protocol conversion, and any cooldown retry.
type RuntimeSnapshot struct {
	Cfg            *Config
	Generation     uint64
	Providers      map[string]provider.Provider
	PoolIndex      map[string][]string
	ParentOf       map[string]string
	ExpandedRoutes map[string][]RouteTarget
	RouteKeys      map[string]bool
	Catalog        *catalog.Catalog
	Cache          *responsecache.Store
	Shadow         *shadow.Runtime
	// Guard is this generation's outbound secret/path scanner (immutable,
	// swapped atomically with cfg/providers on reload). It carries known-secret
	// values in memory: NEVER serialize, log, or expose it via any DTO/Web API.
	Guard *guard.Scanner
}

// snapshotRuntime captures every reload-owned dependency under one brief read
// lock. Maps are generation-owned and never mutated in place: reload builds new
// maps and swaps the pointers, so an in-flight request may safely retain them.
func (p *Proxy) SnapshotRuntime() RuntimeSnapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return RuntimeSnapshot{
		Cfg:            p.cfg,
		Generation:     p.configGeneration.Load(),
		Providers:      p.providers,
		PoolIndex:      p.poolIndex,
		ParentOf:       p.parentOf,
		ExpandedRoutes: p.expandedRoutes,
		RouteKeys:      p.routeKeys,
		Catalog:        p.catalog,
		Cache:          p.cache,
		Shadow:         p.shadow.Load(),
		Guard:          p.guardScanner,
	}
}

// serveRequest is the stable input to one scheduling/failover pass. It groups
// request identity, protocol/body data, routing inputs, and the runtime snapshot
// instead of threading them as an ever-growing positional parameter list.
type serveRequest struct {
	runtime RuntimeSnapshot

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

// forwardLogCtx aliases the request-log observation scope; field names differ
// (requestID/attempt/exposed/origBody) so root call sites keep their local
// vocabulary while the data-plane mapping lives in internal/observe/requestlog.
type forwardLogCtx struct {
	requestID string
	attempt   int
	exposed   string
	origBody  []byte
	// diagnostics of THIS attempt's request conversion (empty on passthrough)
	diagnostics []targetexec.ConversionDiagnostic
}

// newTargetAttempt is the single assembly point shared by normal routing and
// Fusion synthesis. Request rewriting, state expansion, and protocol conversion
// deliberately remain outside this factory because those operations have
// caller-specific semantics and may fail before an attempt can be executed.
func newTargetAttempt(
	runtime RuntimeSnapshot,
	plan targetexec.Plan,
	exchange targetexec.Exchange,
	scope targetexec.Scope,
	policy targetexec.Policy,
) targetexec.Attempt {
	var scheduling Scheduling
	if runtime.Cfg != nil {
		scheduling = runtime.Cfg.Scheduling
	}
	return targetexec.NewAttempt(
		targetexec.Runtime{
			Scheduling: scheduling,
			Generation: runtime.Generation,
			Cache:      runtime.Cache,
		},
		plan,
		exchange,
		scope,
		policy,
	)
}

// buildRequestLogInput adapts the root forwardLogCtx vocabulary to
// requestlog.BuildInput.
func buildRequestLogInput(
	context forwardLogCtx,
	request *http.Request,
	protocol string,
	calledModel string,
	target RouteTarget,
	response *http.Response,
	startedAt time.Time,
	upstreamRequestBody []byte,
) requestlog.Input {
	diags := make([]requestlog.ConversionDiagnostic, 0, len(context.diagnostics))
	for _, d := range context.diagnostics {
		diags = append(diags, requestlog.ConversionDiagnostic{Code: d.Code, Detail: d.Detail})
	}
	return requestlog.BuildInput(
		requestlog.LogCtx{
			RequestID:   context.requestID,
			Attempt:     context.attempt,
			Exposed:     context.exposed,
			OrigBody:    context.origBody,
			Diagnostics: diags,
		},
		request,
		protocol,
		calledModel,
		target,
		response,
		startedAt,
		upstreamRequestBody,
	)
}
