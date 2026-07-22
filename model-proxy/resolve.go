package main

import (
	"hash/fnv"
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
// Responsibility boundary: the resolver maps IDs and, for the one-virtual Pick
// path, pre-filters on health (circuit/rate-limit) so a best-effort caller
// doesn't pin a dead account. It does NOT take the circuit-breaker half-open
// slot — that stays in each caller (forward's takeHalfOpenSlot/recordSuccess/
// recordFailure, Fusion's circuit gate), because those are coupled to failover.
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
// no extra providers-presence filter is applied here; Pick checks providers
// itself when it needs a runnable guarantee.
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
// fail over across a parent's accounts. No health filter: routing gates live in
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

// Pick returns one HEALTHY runnable virtual for a config target, for one-virtual
// callers (Fusion panel members + synthesizer, Shadow).
//
//   - Session-sticky: when the target is a pooled parent and stickyKey is
//     non-empty, the same key maps to the same account (stable hash) — so one
//     conversation reuses one account and stays cache-warm. An empty stickyKey
//     (e.g. Shadow, fire-and-forget) round-robins via the shared spread counter.
//   - Account failover: if the chosen virtual is circuit-open or rate-limited,
//     Pick scans its siblings for a healthy one — stickiness degrades to failover
//     ONLY when the sticky account is down.
//   - ok=false when no virtual is healthy or the provider has no built impl
//     (unknown / not logged in); the caller treats the target as unavailable.
//
// Health is a pre-filter read under healthMu (race-free — the same lock mutates
// these fields); the caller still does its own authoritative gating.
func (r *resolver) Pick(t RouteTarget, stickyKey string) (RouteTarget, bool) {
	vids := r.virtualsOf(t.Provider)
	if len(vids) == 0 {
		return RouteTarget{}, false
	}
	if _, pooled := r.poolIndex[t.Provider]; !pooled {
		if !r.built(vids[0]) || !r.healthy(vids[0], t.Model) {
			return RouteTarget{}, false
		}
		rt := t
		rt.Provider = vids[0]
		return rt, true
	}
	// Pooled: pick a start index (sticky hash, else spread), then scan siblings
	// for the first healthy built one.
	start := r.pickStart(t.Provider, len(vids), stickyKey)
	for k := 0; k < len(vids); k++ {
		vid := vids[(start+k)%len(vids)]
		if r.built(vid) && r.healthy(vid, t.Model) {
			rt := t
			rt.Provider = vid
			return rt, true
		}
	}
	return RouteTarget{}, false
}

// pickStart returns the virtual index to try first: a stable hash of stickyKey
// (session-sticky) when non-empty, else the shared per-parent spread counter
// (round-robin). Both under healthMu (spreadCtr lives there).
func (r *resolver) pickStart(parent string, n int, stickyKey string) int {
	if stickyKey != "" {
		return int(stickyHash(stickyKey) % uint64(n))
	}
	r.p.healthMu.Lock()
	defer r.p.healthMu.Unlock()
	start := int(r.p.spreadCtr[parent] % uint64(n))
	r.p.spreadCtr[parent]++
	return start
}

// healthy reports whether a virtual may be tried for `model` — the SAME rule as
// tryTarget's gate: circuit closed or half-open with no probe in flight, not
// rate-limited, AND the (virtual, model) pair is not in its model-lockout window.
// The model-lock check matters because Fusion panel/judge legs + Shadow route
// through Pick (not tryTarget): without it they would keep requesting a model
// the main path already locked (recordModelFailure) until the lockout expired.
// The health fields are mutated under healthMu (recordSuccess/Failure/RateLimit),
// so they are read under the same lock; modelLockedLocked is the lock-holding
// variant that fits here.
func (r *resolver) healthy(virtual, model string) bool {
	now := time.Now()
	r.p.healthMu.Lock()
	defer r.p.healthMu.Unlock()
	h := r.p.health[virtual]
	return (h == nil || h.available(now)) && !r.p.modelLockedLocked(virtual, model, now)
}

// built reports whether a virtual id has a runtime provider impl.
func (r *resolver) built(vid string) bool {
	_, ok := r.providers[vid]
	return ok
}

// stickyHash is a stable 64-bit hash of a session key, used to map a session to a
// stable pool slot (deterministic across calls → same session, same account).
func stickyHash(s string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(s))
	return h.Sum64()
}
