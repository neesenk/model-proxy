package main

import (
	"fmt"
	"log"
	"model-proxy/internal/app"
	"time"

	"model-proxy/internal/shadow"
)

// reloadAppliedWarning means the new config is already live, but a required
// post-swap durability step failed. Callers must surface it without rolling the
// config file back (which would diverge disk from the already-swapped runtime).
type reloadAppliedWarning struct{ err error }

func (e *reloadAppliedWarning) Error() string { return "reload applied with warning: " + e.err.Error() }
func (e *reloadAppliedWarning) Unwrap() error { return e.err }

func (p *Proxy) reload(configPath string) error {
	cfg, err := LoadConfig(configPath)
	if err != nil {
		return err
	}
	built := buildProviders(cfg)
	newImplicit, newWarnings := synthesizeImplicitRoutesFrom(cfg, built.Eligible)
	// Switch config and runtime state as one generation. Persist snapshots take
	// the same lock order, and request mutations carry the generation captured by
	// forward, so an old in-flight request cannot repopulate the cleared maps.
	p.mu.Lock()
	generation := p.configGeneration.Add(1)
	p.cfg = cfg
	p.providers = built.Providers
	// Rebuild the pool index + expanded routes from the single buildProviders
	// pass. Doing this under the write lock means request readers (which take
	// the read lock) see a consistent cfg/providers/poolIndex/expandedRoutes.
	p.poolIndex = built.PoolIndex
	p.parentOf = built.ParentOf
	p.implicitRoutes = newImplicit
	p.expandedRoutes = p.buildExpandedRoutes()
	hw := app.ConfigRoutingWarnings(cfg, p.expandedRoutes)
	p.routeWarnings = append(newWarnings, hw...)
	// Rebuild the cache from the new config (pure in-memory, no goroutine/file
	// lifecycle to drain — safe to swap). cache.enabled toggled via reload now
	// takes effect immediately.
	p.cache = newResponseCache(cfg.Cache)
	// Rebuild the shadow dispatch bundle so shadow_sample_rate /
	// shadow_max_concurrent / client-timeout changes take effect at once — without
	// this, disabling shadow (sample_rate: 0) keeps firing paid requests until
	// restart. Swapped atomically; in-flight shadow goroutines finish on the old bundle.
	p.shadow.Store(shadow.NewRuntime(shadow.Options{
		SampleRate:    cfg.ShadowSampleRate,
		MaxConcurrent: cfg.ShadowMaxConcurrent,
		Timeout:       cfg.Scheduling.Timeout(),
	}))
	p.runtimeState.ReplaceGeneration(generation)
	p.mu.Unlock()
	for _, w := range hw {
		log.Printf("[reload] ⚠ %s", w)
	}
	// Persist the cleared state synchronously so empty-health + the new
	// fingerprint land on disk now (survives a crash right after reload — the
	// documented contract). The async poll below refreshes quota snapshots for
	// newly added providers; pollAsync is tracked + stop-aware so it can't
	// outlive Close (no persist after the final flush).
	var appliedWarning error
	if p.quota != nil {
		persistErr := p.quota.Persist()
		p.quota.PollAsync(time.Now())
		if persistErr != nil {
			appliedWarning = &reloadAppliedWarning{err: fmt.Errorf("persist cleared runtime state: %w", persistErr)}
		}
	}
	// Refresh the models.dev catalog best-effort so newly configured model names
	// resolve their context window / modalities for request-aware routing. A
	// failure leaves the previous catalog in place (never fails the reload).
	p.refreshCatalogAsync()
	// Re-probe wire capabilities for providers whose verdict is missing or
	// whose base_url changed (existing verdicts survive reload — wireCaps is
	// not part of the cleared health state).
	p.startWireCapProbe()
	// request_log is NOT rebuilt on reload (the logger owns a background
	// goroutine + open file; restarting it mid-flight needs careful drain). So
	// a config that enables/tunes request_log via SIGHUP won't take effect until
	// restart. Warn when the operator clearly expects logging but it isn't
	// active, so this isn't a silent no-op.
	if cfg.RequestLog.Enabled && p.reqLog == nil {
		log.Printf("[reload] request_log.enabled is true but logging is not active (reload cannot start it); restart the daemon to enable request logging")
	}
	return appliedWarning
}
