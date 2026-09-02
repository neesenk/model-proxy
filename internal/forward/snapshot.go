package forward

import (
	responsecache "model-proxy/internal/cache"
	"model-proxy/internal/catalog"
	"model-proxy/internal/guard"
	"model-proxy/internal/observe/seclog"
	"model-proxy/internal/provider"
	"model-proxy/internal/shadow"
)

// Snapshot is one immutable view of reload-swapped runtime dependencies.
// A request captures it once and keeps using the same config generation through
// scheduling, failover, protocol conversion, and any cooldown retry. The
// single-capture red line lives at the caller (internal/app's
// Proxy.SnapshotRuntime takes the lock); Fusion, Shadow and every other async
// branch only ever see this frozen struct.
type Snapshot struct {
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
	// SecLog is this generation's security audit logger (nil when guard.audit
	// is off). Swapped with the generation like Guard: a request audits against
	// the logger of its own generation, and an old logger is drained+stopped at
	// swap time, so late enqueues from in-flight requests may be dropped.
	SecLog *seclog.Logger
}
