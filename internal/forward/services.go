// services.go — the process-lifetime dependency bundle and consumer-owned
// ports the forward pipeline runs against, plus the pipeline engine itself.
package forward

import (
	"net/http"
	"time"

	"model-proxy/internal/fusion"
	guardsession "model-proxy/internal/guard/session"
	"model-proxy/internal/observe/counters"
	observeevents "model-proxy/internal/observe/events"
	"model-proxy/internal/observe/requestlog"
	"model-proxy/internal/protocol"
	"model-proxy/internal/routing"
	"model-proxy/internal/targetexec"
)

// Services bundles the process-lifetime dependencies the request pipeline
// needs. internal/app assembles it from Proxy's process services; every field
// is a stable pointer or port that survives reload. Generation-owned values
// (config, providers, routes, cache, guard) never appear here — they ride the
// per-request Snapshot.
type Services struct {
	Client         targetexec.Doer               // upstream HTTP client (default / fallback)
	Metrics        *counters.MetricsStore        // request counters; nil only in degenerate setups
	Tokens         *counters.TokenCounter        // SSE-scanned token usage; nil only in degenerate setups
	Agents         *counters.AgentCounter        // per-agent (UA) counters; nil only in degenerate setups
	Events         *observeevents.Hub            // live request monitor fan-out hub
	SessionScan    *guardsession.Store           // split-exfiltration session windows; nil = off
	ResponsesState *protocol.ResponsesStateStore // previous_response_id replay; nil = off
	FusionReg      *fusion.Registry              // fusion orchestration observability
	ReqLog         *requestlog.Logger            // per-request access log; nil = disabled
	// Adjudicator is the process-lifetime AI second-opinion port for guard
	// pattern hits (nil = channel absent; the config gate lives in the
	// request snapshot's GuardConfig). Also enforces persisted high-verdict
	// session blocks.
	Adjudicator Adjudicator
	// SessionHeaders is the ordered client session-header allowlist
	// (request_log.session_headers) used for the request log and live events.
	SessionHeaders []string

	// ClientFor resolves the upstream client for one route target from the
	// request snapshot's config (app: Proxy.clientFor — per-provider proxy_url
	// → global proxy → env → system → direct). Nil falls back to Client.
	ClientFor func(cfg *Config, parentOf map[string]string, provider string) targetexec.Doer

	// NewHealthGate binds the app-owned health/circuit adapter
	// (targetexec.HealthGate) to the request snapshot's pool-virtual→parent
	// projection, so the wire-verdict 404 correction records under the
	// request's own generation parent.
	NewHealthGate func(parentOf map[string]string) targetexec.HealthGate
	// NewEffects binds app-owned observation (metrics, request log, live
	// events, tokens, agents, attempt quality) to one runtime generation.
	NewEffects func(generation uint64) targetexec.Effects

	// Schedule is the quota-aware scheduler (app: Proxy.schedule). The
	// generation argument binds the sticky/round-robin mutation to the
	// request's own snapshot generation.
	Schedule func(cfg *Config, parentOf map[string]string, exposed, sessionKey string, targets []RouteTarget, routeKeys map[string]bool, generation uint64) []RouteTarget
	// ShadowDispatch is the post-commit shadow hook (app:
	// Proxy.dispatchShadowAfterCommit). Called only after a normal target
	// commits, with that attempt's Commit.
	ShadowDispatch func(runtime Snapshot, proto, backendProto, calledModel, exposed string, primary RouteTarget, primaryRequestID string, commit *targetexec.Commit)
	// ResolveBackendProto is the wire-verdict backend protocol resolution
	// (app: Proxy.resolvedBackendProto): declared protocol, else ProtocolHint,
	// else the probe verdict, else the client protocol.
	ResolveBackendProto func(declared, provName string, provCfg Provider, model, clientProto string, parentOf map[string]string) (proto string, viaResponsesVerdict bool)
	// ResolverState is the pool health/spread view routing.NewResolver needs
	// (app: Proxy). Used by fusion legs, the synthesizer and shadow to pick a
	// runnable virtual out of a pooled parent.
	ResolverState routing.ResolverState
}

// RouteState is the consumer-owned pin/cooldown query surface the pipeline
// needs from the runtime Manager. internal/app implements it over Proxy's
// generation-tagged scheduling adapters.
type RouteState interface {
	// PinForces reports whether an active pin for `exposed` narrows the route
	// to the pinned provider (exclusive: no cross-route reroute, circuit
	// bypass, cache bypass).
	PinForces(exposed string, ordered []RouteTarget, parentOf map[string]string) bool
	// CooldownState inspects the targets' health/quota for the wait-retry
	// decision: allDown = every target unavailable; allRateLimited = every
	// down reason is rate-limit/quota class; earliest = soonest recovery.
	CooldownState(targets []RouteTarget, now time.Time, quotaMaxAge time.Duration) (allDown, allRateLimited bool, earliest time.Time)
	// HasRecoveredUntried reports the TOCTOU case: a target is available now
	// but was NOT tried in the failed pass — answer with an immediate re-pass.
	HasRecoveredUntried(targets []RouteTarget, tried map[string]bool, now time.Time, quotaMaxAge time.Duration) bool
	// QuotaFreshnessMaxAge is the quota-snapshot freshness window shared by
	// the scheduling skip and the failure classification.
	QuotaFreshnessMaxAge(cfg *Config) time.Duration
}

// pipeline is the request-forwarding engine: one Services bundle plus one
// RouteState port, shared by every request. It never sees internal/app's
// Proxy and never takes its lock — the lock discipline (single snapshot per
// request) is the caller's; the pipeline only consumes the frozen Snapshot.
type pipeline struct {
	svc   Services
	state RouteState
}

// schedule delegates to the injected scheduler port.
func (p pipeline) schedule(cfg *Config, parentOf map[string]string, exposed, sessionKey string, targets []RouteTarget, routeKeys map[string]bool, generation uint64) []RouteTarget {
	return p.svc.Schedule(cfg, parentOf, exposed, sessionKey, targets, routeKeys, generation)
}

// clientFor resolves the per-target upstream client, falling back to the
// bundle-wide default when no resolver is installed.
func (p pipeline) clientFor(cfg *Config, parentOf map[string]string, provider string) targetexec.Doer {
	if p.svc.ClientFor != nil {
		if client := p.svc.ClientFor(cfg, parentOf, provider); client != nil {
			return client
		}
	}
	return p.svc.Client
}

// pinForces reports whether an active pin for `exposed` is in effect over the
// given ordered targets. When true, the route is pinned-exclusive:
// request-aware routing is skipped (no reroute away from the pin) and the
// attempt bypasses the circuit.
func (p pipeline) pinForces(exposed string, ordered []RouteTarget, parentOf map[string]string) bool {
	return p.state.PinForces(exposed, ordered, parentOf)
}

func (p pipeline) cooldownState(targets []RouteTarget, now time.Time, quotaMaxAge time.Duration) (allDown, allRateLimited bool, earliest time.Time) {
	return p.state.CooldownState(targets, now, quotaMaxAge)
}

func (p pipeline) hasRecoveredUntried(targets []RouteTarget, tried map[string]bool, now time.Time, quotaMaxAge time.Duration) bool {
	return p.state.HasRecoveredUntried(targets, tried, now, quotaMaxAge)
}

func (p pipeline) quotaFreshnessMaxAge(cfg *Config) time.Duration {
	return p.state.QuotaFreshnessMaxAge(cfg)
}

// dispatchShadowAfterCommit delegates the post-commit shadow hook to the
// injected port (app owns Shadow policy, sampling and lifecycle admission).
func (p pipeline) dispatchShadowAfterCommit(
	runtime Snapshot,
	proto string,
	backendProto string,
	calledModel string,
	exposed string,
	primary RouteTarget,
	primaryRequestID string,
	commit *targetexec.Commit,
) {
	p.svc.ShadowDispatch(runtime, proto, backendProto, calledModel, exposed, primary, primaryRequestID, commit)
}

// publishTerminalEvent emits a live "end" event for a request that ends before
// the normal start/commit flow (see PublishTerminalEvent).
func (p pipeline) publishTerminalEvent(requestID string, r *http.Request, proto, exposed string, status int) {
	PublishTerminalEvent(p.svc.Events, requestID, r, proto, exposed, status, p.svc.SessionHeaders)
}

// resolvedBackendProto delegates the wire-verdict backend protocol resolution
// to the injected port (app owns the verdict store).
func (p pipeline) resolvedBackendProto(declared, provName string, provCfg Provider, model, clientProto string, parentOf map[string]string) (proto string, viaResponsesVerdict bool) {
	return p.svc.ResolveBackendProto(declared, provName, provCfg, model, clientProto, parentOf)
}
