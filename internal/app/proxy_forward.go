// proxy_forward.go — the thin app-side shim into internal/forward: one snapshot
// capture, one Services assembly, one Serve call. The pipeline itself (routing,
// guard, cache, failover, fusion orchestration) lives in internal/forward.
package app

import (
	"net/http"
	"time"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/forward"
	"model-proxy/internal/targetexec"
)

// forward proxies a request to the upstream selected by the route for the
// requested model (see internal/forward.Serve for the routing/failover
// contract). This shim owns the two app-side red lines: the runtime snapshot
// is captured ONCE here (the lock is NOT held during forwarding, which streams
// for minutes on SSE), and every process-lifetime dependency reaches the
// pipeline through the assembled Services bundle — the pipeline never sees
// Proxy and never takes p.mu.
func (p *Proxy) forward(proto string, w http.ResponseWriter, r *http.Request, requestID string) {
	forward.Serve(p.forwardServices(), proxyRouteState{proxy: p}, p.SnapshotRuntime(), proto, w, r, requestID)
}

// forwardServices assembles the process-lifetime dependency bundle for the
// forward pipeline. Built per request (a cheap struct of stable pointers +
// ports): optional services like the request log are attached after Proxy
// construction, so a construction-time bundle would freeze nils.
func (p *Proxy) forwardServices() forward.Services {
	return forward.Services{
		Client:         p.client,
		ClientFor:      p.clientFor,
		Metrics:        p.metrics,
		Tokens:         p.tokens,
		Agents:         p.agents,
		Events:         p.events,
		SessionScan:    p.sessionScan,
		ResponsesState: p.responsesState,
		FusionReg:      p.fusionReg,
		ReqLog:         p.reqLog,
		SessionHeaders: p.sessionHeaders(),
		NewHealthGate: func(parentOf map[string]string) targetexec.HealthGate {
			return proxyHealthGate{proxy: p, parentOf: parentOf}
		},
		NewEffects: func(generation uint64) targetexec.Effects {
			return targetExecutionEffects{proxy: p, generation: generation}
		},
		Schedule: func(cfg *Config, parentOf map[string]string, exposed, sessionKey string, targets []RouteTarget, routeKeys map[string]bool, generation uint64) []RouteTarget {
			return p.schedule(cfg, parentOf, exposed, sessionKey, targets, routeKeys, generation)
		},
		ShadowDispatch:      p.dispatchShadowAfterCommit,
		ResolveBackendProto: p.resolvedBackendProto,
		ResolverState:       p,
	}
}

// sessionHeaders resolves the client session-header allowlist for terminal
// (pre-start) live events; the main pipeline reads the same value from the
// per-request config snapshot.
func (p *Proxy) sessionHeaders() []string {
	if cfg := p.cfgSnapshot(); cfg != nil {
		return cfg.RequestLog.ResolvedSessionHeaders()
	}
	return configdomain.DefaultSessionHeaders
}

// proxyRouteState adapts Proxy's pin/cooldown scheduling queries to
// forward.RouteState. The generation-tagged logic itself stays in
// proxy_schedule.go / proxy_read_endpoints.go.
type proxyRouteState struct {
	proxy *Proxy
}

func (s proxyRouteState) PinForces(exposed string, ordered []RouteTarget, parentOf map[string]string) bool {
	return s.proxy.pinForces(exposed, ordered, parentOf)
}

func (s proxyRouteState) CooldownState(targets []RouteTarget, now time.Time, quotaMaxAge time.Duration) (allDown, allRateLimited bool, earliest time.Time) {
	return s.proxy.cooldownState(targets, now, quotaMaxAge)
}

func (s proxyRouteState) HasRecoveredUntried(targets []RouteTarget, tried map[string]bool, now time.Time, quotaMaxAge time.Duration) bool {
	return s.proxy.hasRecoveredUntried(targets, tried, now, quotaMaxAge)
}

func (s proxyRouteState) QuotaFreshnessMaxAge(cfg *Config) time.Duration {
	return s.proxy.quotaFreshnessMaxAge(cfg)
}
