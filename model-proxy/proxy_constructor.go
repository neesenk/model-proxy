package main

import (
	"fmt"
	"log"
	"model-proxy/internal/app"
	responsecache "model-proxy/internal/cache"
	"model-proxy/internal/observe/counters"
	runtimestate "model-proxy/internal/runtime"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"model-proxy/internal/fusion"
	observeevents "model-proxy/internal/observe/events"
	"model-proxy/internal/protocol"
	"model-proxy/internal/shadow"
	"model-proxy/provider"
)

func NewProxy(cfg *Config) *Proxy {
	home, _ := os.UserHomeDir()
	p := newProxyWithStatePath(cfg, filepath.Join(home, ".model-proxy", "quota_state.json"))
	// Production only: probe wire capabilities asynchronously at boot (the
	// injectable constructor leaves probing off; tests drive it synchronously).
	p.wireProbe = true
	p.startWireCapProbe()
	return p
}

// newProxyWithStatePath is the injectable constructor used by tests so every
// Proxy owns an isolated state file before the tracker loads or starts.
func newProxyWithStatePath(cfg *Config, qpath string) *Proxy {
	built := buildProviders(cfg)
	p := &Proxy{
		lifecycle: newProxyLifecycle(),
		cfg:       cfg,
		providers: built.Providers,
		client:    &http.Client{Timeout: 0},
		poolIndex: built.PoolIndex,
		parentOf:  built.ParentOf,
	}
	p.runtimeState.ReplaceGeneration(1)
	p.configGeneration.Store(1)
	p.implicitRoutes, p.routeWarnings = synthesizeImplicitRoutesFrom(cfg, built.Eligible)
	p.expandedRoutes = p.buildExpandedRoutes()
	// Config-time routing hazards (reasoning-replay models behind conversion,
	// missing protocol: on hint providers): appended to the warnings channel
	// (/api/status + `models` CLI) AND logged — the operator should see them at
	// boot, not only when they open the dashboard.
	if hw := app.ConfigRoutingWarnings(cfg, p.expandedRoutes); len(hw) > 0 {
		p.routeWarnings = append(p.routeWarnings, hw...)
		for _, w := range hw {
			log.Printf("[startup] ⚠ %s", w)
		}
	}
	// The tracker reads cfg/providers asynchronously via the snapshot closures
	// (each takes p.mu.RLock), so reloads are picked up without recreating it.
	p.quota = runtimestate.NewQuotaTracker(qpath,
		func() *Config { return p.cfgSnapshot() },
		func() map[string]provider.Provider { return p.providerSnapshot() },
		&p.runtimeState)
	p.quota.Generation = p.configGeneration.Load
	p.quota.FullSnapshot = p.snapshotPersistedState
	p.quota.Start()
	p.metrics = counters.NewMetricsStore()
	// SSE token counter. Persistence (baseline restore + per-minute flush) is
	// projected by statsFlusher into internal/observe/stats.Store, opened only
	// by lifecycle services so direct-NewProxy tests stay in-memory.
	p.tokens = counters.NewTokenCounter()
	// Per-agent counters (detected from the client UA). Flushed alongside the
	// minute buckets by the same flusher; nil-stats tests keep them in-memory.
	p.agents = counters.NewAgentCounter()
	// Exact-match response cache. nil unless cache.enabled is set in config, so
	// the default (off) path and direct-NewProxy tests pay zero overhead.
	p.cache = newResponseCache(cfg.Cache)
	p.responsesState = protocol.NewResponsesStateStore(protocol.ResponsesStatePath(qpath))
	// Live request monitor hub (SSE /api/events). Always on — empty unless a Web
	// UI client subscribes; publish is non-blocking so it never stalls forward.
	p.events = observeevents.NewHub()
	// Fusion orchestration observability registry (recent runs + per-workflow
	// aggregates + daily budget counters). Like the event hub, reload does NOT
	// rebuild it — aggregates and today's budget survive config edits.
	p.fusionReg = fusion.NewRegistry()
	// Detached Shadow runtime (sample rate, concurrency gate, shared client). Stored
	// in an atomic pointer so reload can swap the whole bundle race-free; each
	// dispatch loads it once and uses that snapshot, so in-flight shadow goroutines
	// finish on the old bundle while new traffic follows the reloaded config.
	p.shadow.Store(shadow.NewRuntime(shadow.Options{
		SampleRate:    cfg.ShadowSampleRate,
		MaxConcurrent: cfg.ShadowMaxConcurrent,
		Timeout:       cfg.Scheduling.Timeout(),
	}))
	// Restore the per-route sticky selections persisted before the last restart,
	// so the proxy resumes parking on the same providers (prompt-cache-friendly).
	// Gated like the health restore below: a file with a MISMATCHING config
	// fingerprint belongs to a different config (or a test binary sharing the
	// state file) — its route→provider parks must not leak over (route names
	// like "m1" collide across configs even when providers don't). Legacy
	// files without a fingerprint keep the historical restore behavior.
	fp := healthConfigFingerprint(cfg)
	fpMatch := p.quota.LoadedHealthFP == "" || p.quota.LoadedHealthFP == fp
	if p.quota.LoadedHealthFP != "" && !fpMatch {
		// Quota snapshots are provider/account observations too. A changed provider
		// identity or endpoint must not inherit the old file's scheduling tier.
		p.quota.ClearForGeneration(p.configGeneration.Load())
	}
	if loaded := p.quota.LoadedSticky; len(loaded) > 0 && fpMatch {
		p.runtimeState.RestoreSticky(loaded)
	}
	// Restore frozen health state (rate-limit/circuit cooldowns, model lockouts,
	// learned param blocklist) persisted before the last restart — ONLY when the
	// file's config fingerprint matches the current config: health is keyed by
	// provider name, so without the gate a different config (or a test binary
	// sharing the state file) would inherit cooldowns onto unrelated same-named
	// providers. Only future-dated cooldowns are applied — expired ones
	// self-heal by being dropped. A restored circuit gets a full failure count
	// so its next failure re-opens it immediately (same semantics as before).
	if loaded := p.quota.LoadedHealth; len(loaded) > 0 && p.quota.LoadedHealthFP != "" && p.quota.LoadedHealthFP == fp {
		p.runtimeState.RestoreHealth(loaded, time.Now(), cfg.Scheduling.Threshold())
	}
	// Restore wire capability verdicts (independent of the health fingerprint:
	// capabilities are endpoint properties). A verdict is honored only while
	// its recorded base_url still matches the current config — an endpoint
	// change invalidates it and triggers a re-probe at the next boot probe.
	if loaded := p.quota.LoadedWireCaps; len(loaded) > 0 {
		baseURLs := make(map[string]string, len(cfg.Providers))
		for name, prov := range cfg.Providers {
			baseURLs[name] = prov.OpenAIBaseURL
		}
		p.wireCaps.RestoreMatching(loaded, baseURLs)
	}
	return p
}

// Close releases every Proxy-owned background component and performs final
// flushes. It is idempotent; lifecycle admission is closed before waiting so a
// concurrent reload cannot add a catalog refresh behind shutdown.
func (p *Proxy) Close() {
	p.closeOnce.Do(p.closeRuntimeServices)
}

// resetStats zeroes all call-statistics state: the in-memory metrics + token
// counters, the persisted SQLite bucket history, and the flusher's diff
// baseline (so the next flush sees zero delta rather than zero-minus-old
// negatives). Drives POST /api/tokens/reset ("reset counters").
func (p *Proxy) resetStats() error {
	// When a flusher exists, delegate to flusher.reset which does the full stats
	// reset (counters + DB + baseline) UNDER the flusher's lock — preventing a
	// concurrent per-minute flush from writing stale deltas to the just-cleared
	// DB (the resetStats vs flush race).
	if p.flusher != nil {
		if err := p.flusher.reset(); err != nil {
			return fmt.Errorf("reset persisted stats: %w", err)
		}
	} else {
		// Non-flusher path (degenerate tests): clear durable state first. If that
		// fails, keep the in-memory totals so a later restart cannot resurrect a
		// history the user was told had been reset.
		if p.stats != nil {
			if err := p.stats.Reset(); err != nil {
				return fmt.Errorf("reset persisted stats: %w", err)
			}
		}
		if p.metrics != nil {
			p.metrics.Reset()
		}
		if p.tokens != nil {
			p.tokens.Reset()
		}
		if p.agents != nil {
			p.agents.Reset()
		}
	}
	// Response cache participates in the user-facing "reset counters" command,
	// but is not stats persistence and therefore stays outside statsFlusher.
	if p.cache != nil {
		p.cache.Reset()
	}
	return nil
}

// newResponseCache adapts resolved application configuration into the
// repository-leaf cache component.
func newResponseCache(config CacheConfig) *responsecache.Store {
	if !config.IsEnabled() {
		return nil
	}
	return responsecache.New(responsecache.Options{
		TTL:          config.TTLDuration(),
		MaxEntries:   config.MaxEntriesValue(),
		MaxBodyBytes: config.MaxBodyBytesValue(),
	})
}
