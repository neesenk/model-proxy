package main

import (
	"time"

	"model-proxy/provider"
)

// resolve.go is the single front door for provider IDENTITY: it turns a config
// RouteTarget's provider (possibly a pooled PARENT name like "zhipu") into one
// or more runnable VIRTUAL provider ids ("zhipu#<account>").
//
// Why this exists: routing expanded pools (expandTarget) but Fusion and Shadow
// looked the parent name up directly in the runtime instance map
// (provs[parent] → nil for a pooled parent), so a route whose target was a
// multi-account provider silently broke Fusion (members judged unavailable,
// synthesizer lacking auth) and Shadow (stopped sampling) the moment a second
// account was added. Every code path now funnels config-target → runnable-virtual
// through this resolver.
//
// Responsibility boundary (deliberate): the resolver maps IDs and reports a
// fresh health hint; it does NOT gate. Circuit-breaker mechanics stay in each
// caller — forward's half-open single-flight (takeHalfOpenSlot/recordSuccess/
// recordFailure) and Fusion's circuit gate — because those are coupled to
// failover. The hint lets Fusion/Shadow skip a clearly-dead virtual without
// re-implementing the check.
type resolver struct {
	p         *Proxy
	providers map[string]provider.Provider // runtime instances (virtual ids only for pooled parents)
	poolIndex map[string][]string          // parent name → sorted virtual ids (only multi-account parents)
}

func newResolver(p *Proxy, providers map[string]provider.Provider, poolIndex map[string][]string) *resolver {
	return &resolver{p: p, providers: providers, poolIndex: poolIndex}
}

// virtualsOf returns the runnable virtual ids for a config provider name: the
// parent's sorted pool if it is a multi-account parent, else the name itself
// (non-pooled / single-account providers are their own runnable id). Matches the
// buildProviders invariant — every id in poolIndex[parent] has a built impl — so
// no extra providers-presence filter is applied here (callers that need a
// runnable guarantee, like Pick, check providers themselves).
func (r *resolver) virtualsOf(provider string) []string {
	if ids, pooled := r.poolIndex[provider]; pooled {
		return ids
	}
	return []string{provider}
}

// Expand returns every runnable virtual for a config target as RouteTargets —
// one if the provider is not pooled, one per account if it is a pooled parent —
// preserving Model/Priority/Protocol. This is the expandTarget semantics,
// centralized; routing (buildExpandedRoutes) uses it to rank by tier/surplus and
// fail over across a parent's accounts. No health hint: routing gates live in
// the forward loop, and expandedRoutes is rebuilt on reload (a baked-in hint
// would be stale).
func (r *resolver) Expand(t RouteTarget) []RouteTarget {
	vids := r.virtualsOf(t.Provider)
	out := make([]RouteTarget, 0, len(vids))
	for _, vid := range vids {
		rt := t
		rt.Provider = vid
		out = append(out, rt)
	}
	return out
}

// Pick returns ONE runnable virtual for a config target, chosen by per-parent
// round-robin (the shared spreadCtr) when pooled so distinct requests spread
// across accounts, plus a fresh health hint. Used by one-virtual callers: Fusion
// panel members, the Fusion synthesizer, and Shadow. ok=false when the picked
// virtual has no built provider impl (unknown provider, or a non-pooled name that
// isn't logged in) — the caller treats the target as unavailable.
func (r *resolver) Pick(t RouteTarget) (resolvedTarget, bool) {
	vids := r.virtualsOf(t.Provider)
	vid := vids[0]
	if len(vids) > 1 {
		vid = r.pickSpread(t.Provider, vids)
	}
	if _, ok := r.providers[vid]; !ok {
		return resolvedTarget{}, false
	}
	rt := t
	rt.Provider = vid
	return resolvedTarget{target: rt, health: r.hintFor(vid)}, true
}

// pickSpread selects one virtual id from a parent's pool via the shared per-parent
// spread counter (under healthMu), matching routing's pool spread so all traffic
// to a parent rotates coherently.
func (r *resolver) pickSpread(parent string, vids []string) string {
	r.p.healthMu.Lock()
	defer r.p.healthMu.Unlock()
	// Modulo the uint64 counter BEFORE the int cast: on 32-bit a counter past
	// ~2³¹ would cast negative and index out of range. Mirrors schedule's spread.
	start := int(r.p.spreadCtr[parent] % uint64(len(vids)))
	r.p.spreadCtr[parent]++
	return vids[start]
}

// resolvedTarget is one runnable virtual plus a snapshot of its circuit/rate-
// limit state, for callers that pick a single virtual (Fusion, Shadow).
type resolvedTarget struct {
	target RouteTarget // Provider is a runnable virtual id
	health healthHint  // fresh snapshot; the caller decides whether to honor it
}

// healthHint is a read-only view of a virtual's circuit/rate-limit state. It is
// advisory (the caller still gates): available means "no reason to skip on
// health"; a false available lets a best-effort caller (Shadow) cheaply avoid a
// doomed request, and lets Fusion mark a member unavailable without each
// re-implementing the check.
type healthHint struct {
	available   bool // circuit closed AND not rate-limited
	circuitOpen bool
	rateLimited bool
}

// hintFor snapshots the circuit/rate-limit state of a virtual id (under healthMu).
// A virtual with no recorded failures (nil entry) is available.
func (r *resolver) hintFor(virtual string) healthHint {
	now := time.Now()
	r.p.healthMu.Lock()
	h := r.p.health[virtual]
	r.p.healthMu.Unlock()
	if h == nil {
		return healthHint{available: true}
	}
	open := now.Before(h.circuitOpenUntil)
	limited := now.Before(h.rateLimitedUntil)
	return healthHint{available: !open && !limited, circuitOpen: open, rateLimited: limited}
}
