package app

import (
	"model-proxy/internal/forward"
)

// RuntimeSnapshot aliases the canonical snapshot type owned by
// internal/forward: one immutable view of reload-swapped runtime dependencies.
// A request captures it once and keeps using the same config generation through
// scheduling, failover, protocol conversion, and any cooldown retry.
type RuntimeSnapshot = forward.Snapshot

// SnapshotRuntime captures every reload-owned dependency under one brief read
// lock. Maps are generation-owned and never mutated in place: reload builds new
// maps and swaps the pointers, so an in-flight request may safely retain them.
// This is the single per-request capture point (red line): the forward
// pipeline, Fusion and Shadow only ever see the frozen struct it returns.
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
		SecLog:         p.secLog,
	}
}
