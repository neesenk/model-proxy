// proxy_reload.go — SIGHUP reload generation swap, plus route compilation (explicit/derived route build and pool fan-out) shared by startup and reload.
package app

import (
	"fmt"
	"model-proxy/internal/accounts"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/observe/logx"
	"model-proxy/internal/providerbuild"
	"model-proxy/internal/routing"
	"model-proxy/internal/shadow"
	"model-proxy/internal/upstreamproxy"
	"time"
)

// ReloadAppliedWarning means the new config is already live, but a required
// post-swap durability step failed. Callers must surface it without rolling the
// config file back (which would diverge disk from the already-swapped runtime).
type ReloadAppliedWarning struct{ Err error }

func (e *ReloadAppliedWarning) Error() string { return "reload applied with warning: " + e.Err.Error() }
func (e *ReloadAppliedWarning) Unwrap() error { return e.Err }

func (p *Proxy) Reload(configPath string) error {
	cfg, err := configdomain.LoadConfig(configPath)
	if err != nil {
		return err
	}
	// Re-apply the credentials mode (pools + OAuth) before any pool I/O (same
	// as the constructor): a `credentials:` change takes effect on this reload.
	accounts.SetProcessCredentialsMode(cfg.CredentialsMode())
	if note := accounts.CredentialMismatchNote(cfg.Credentials); note != "" {
		logx.Warnf("[reload] ⚠ %s", note)
	}
	built := providerbuild.BuildProviders(cfg, accounts.NewStore(accounts.HomeDir()), providerbuild.BuildOpts())
	// Build the guard scanner OUTSIDE the lock (regexp compilation + secret
	// variant precomputation); the lock below only swaps the immutable pointer.
	// Fail-closed: a scanner that cannot be built rejects the whole reload, so
	// a generation never runs with less protection than its config declares
	// (validate rejects bad extra_patterns before this point).
	scanner, err := buildGuardScanner(cfg, built.Secrets)
	if err != nil {
		return fmt.Errorf("build guard scanner: %w", err)
	}
	newDerived := routing.DeriveRoutesFrom(cfg)
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
	p.derivedRoutes = newDerived
	p.expandedRoutes = p.buildExpandedRoutes()
	p.routeKeys = routeKeySet(p.expandedRoutes)
	hw := routing.ConfigRoutingWarnings(cfg, p.expandedRoutes)
	p.routeWarnings = hw
	// Rebuild the cache from the new config (pure in-memory, no goroutine/file
	// lifecycle to drain — safe to swap). cache.enabled toggled via reload now
	// takes effect immediately. Stores share process-lifetime counters, including
	// late lookups by old snapshots; only response entries are generation-owned.
	p.cache = p.newResponseCache(cfg.Cache)
	// Swap the guard scanner with the same generation: in-flight requests keep
	// their snapshot's scanner; new requests see the new credential set
	// (login adds protection, logout drops it, immediately at reload). The
	// pool/OAuth secret subsets are stored alongside so the refresh loop's
	// in-place re-syncs always rebuild from THIS generation's build.
	p.guardScanner = scanner
	// S2 auth sources follow the config generation (validate guarantees both
	// files exist for non-loopback listens; loopback may have either unset).
	p.applyAuthSources(cfg)
	p.guardPoolSecrets = built.PoolSecrets
	p.guardOAuthSecrets = built.OAuthSecrets
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
	// Re-validate model-level protocol verdicts against the new generation's
	// fingerprints: entries whose protocol-relevant config changed are dropped
	// now (under the same lock the snapshot readers serialize on), and the
	// fingerprint map arms ModelStore.Put's stale-generation guard so a still
	// in-flight pre-reload probe pass cannot write its verdicts back.
	p.modelCaps.Restore(p.modelCaps.Snapshot(), protocolFingerprints(cfg))
	p.mu.Unlock()
	// Re-publish the config-level global proxy for the automatic chain used by
	// non-forwarding outbound calls (see NewProxyWithStatePath).
	upstreamproxy.SetDefaultProxy(cfg.Proxy)
	for _, w := range hw {
		logx.Warnf("[reload] ⚠ %s", w)
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
		logx.Warnf("[reload] request_log.enabled is true but logging is not active (reload cannot start it); restart the daemon to enable request logging")
	}
	// The security audit log IS reload-owned (unlike request_log): reconcile
	// the logger with the new generation — audit off→on starts it now, on→off
	// drains+stops it, an audit_path change swaps to the new file. In-flight
	// requests keep their snapshot's logger until it drains.
	p.reconcileSecLog(cfg)
	return appliedWarning
}

// buildExpandedRoutes delegates to routing.BuildExpandedRoutes with the
// pool fan-out from the unified routing resolver. Caller holds p.mu (write) —
// in NewProxy / reload, after buildProviders has populated poolIndex.
func (p *Proxy) buildExpandedRoutes() map[string][]configdomain.RouteTarget {
	return routing.BuildExpandedRoutes(p.cfg, p.derivedRoutes, p.expandTarget)
}

// routeKeySet derives the schedule view's route-name key set from the expanded
// route map. Built once per generation so the request hot path can share it.
func routeKeySet(expanded map[string][]configdomain.RouteTarget) map[string]bool {
	keys := make(map[string]bool, len(expanded))
	for k := range expanded {
		keys[k] = true
	}
	return keys
}

// expandTarget fans a single route target out across a pooled provider's
// virtuals via the unified routing resolver front door.
func (p *Proxy) expandTarget(t configdomain.RouteTarget) []configdomain.RouteTarget {
	return newResolver(p, p.providers, p.poolIndex).Expand(t)
}
