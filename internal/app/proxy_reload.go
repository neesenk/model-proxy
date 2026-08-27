package app

import (
	"fmt"
	"log"
	"time"

	"model-proxy/internal/shadow"
)

// ReloadAppliedWarning means the new config is already live, but a required
// post-swap durability step failed. Callers must surface it without rolling the
// config file back (which would diverge disk from the already-swapped runtime).
type ReloadAppliedWarning struct{ Err error }

func (e *ReloadAppliedWarning) Error() string { return "reload applied with warning: " + e.Err.Error() }
func (e *ReloadAppliedWarning) Unwrap() error { return e.Err }

func (p *Proxy) Reload(configPath string) error {
	cfg, err := LoadConfig(configPath)
	if err != nil {
		return err
	}
	built := BuildProviders(cfg, AccountStore(), buildOpts())
	// Build the guard scanner OUTSIDE the lock (regexp compilation + secret
	// variant precomputation); the lock below only swaps the immutable pointer.
	// Fail-closed: a scanner that cannot be built rejects the whole reload, so
	// a generation never runs with less protection than its config declares
	// (validate rejects bad extra_patterns before this point).
	scanner, err := buildGuardScanner(cfg, built.Secrets)
	if err != nil {
		return fmt.Errorf("build guard scanner: %w", err)
	}
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
	p.routeKeys = routeKeySet(p.expandedRoutes)
	hw := ConfigRoutingWarnings(cfg, p.expandedRoutes)
	p.routeWarnings = append(newWarnings, hw...)
	// Rebuild the cache from the new config (pure in-memory, no goroutine/file
	// lifecycle to drain — safe to swap). cache.enabled toggled via reload now
	// takes effect immediately.
	p.cache = NewResponseCache(cfg.Cache)
	// Swap the guard scanner with the same generation: in-flight requests keep
	// their snapshot's scanner; new requests see the new credential set
	// (login adds protection, logout drops it, immediately at reload).
	p.guardScanner = scanner
	// S2 auth sources follow the config generation (validate guarantees both
	// files exist for non-loopback listens; loopback may have either unset).
	p.applyAuthSources(cfg)
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
			appliedWarning = &ReloadAppliedWarning{Err: fmt.Errorf("persist cleared runtime state: %w", persistErr)}
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
	// Same startup-only semantics for the security audit log: the forward path
	// consults cfg.Guard.AuditEnabled() per generation (so audit:false via
	// reload stops new records at once), but a logger that was never started
	// cannot be created mid-flight.
	if cfg.Guard.AuditEnabled() && p.secLog == nil {
		log.Printf("[reload] guard.audit is true but the security audit log is not active (reload cannot start it); restart the daemon to enable it")
	}
	return appliedWarning
}
