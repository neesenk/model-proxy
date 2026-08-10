package main

import (
	"log"
	"model-proxy/internal/app"
	cliframework "model-proxy/internal/cli/framework"
	runtimestate "model-proxy/internal/runtime"
	"os"
	"time"

	"model-proxy/internal/catalog"
	"model-proxy/internal/pricing"
	"model-proxy/provider"
)

// cfgSnapshot returns the current config under a brief read lock. Used by the
// quota tracker (which reads cfg asynchronously from its poll goroutine).
func (p *Proxy) cfgSnapshot() *Config {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cfg
}

// pricingSnapshot returns a usable price catalog, refreshing the cache when
// stale (best-effort; offline falls back to the stale cache). Nil-safe (including
// a nil cfg, as in degenerate tests) and thundering-herd-safe. Returns nil when
// pricing is disabled.
func (p *Proxy) pricingSnapshot() *pricing.Catalog {
	if p == nil {
		return nil
	}
	cfg := p.cfgSnapshot()
	if cfg == nil || !cfg.Pricing.IsEnabled() {
		return nil
	}
	p.pricingMu.Lock()
	defer p.pricingMu.Unlock()
	cat, err := pricing.EnsureFresh(pricing.RefreshOptions{
		CacheFile: pricing.CachePath(cliframework.HomeDir()),
		Endpoint:  cfg.Pricing.ResolvedSourceURL(),
		Fetch:     pricing.FetchHTTP,
		TTL:       cfg.Pricing.TTLDuration(),
		Warnings:  os.Stderr,
	})
	if err != nil || cat == nil {
		return pricing.Empty()
	}
	return cat
}

// priceOverrides returns the current config `prices:` overrides for the handler.
func (p *Proxy) priceOverrides() map[string]PriceConfig {
	cfg := p.cfgSnapshot()
	if cfg == nil {
		return nil
	}
	return cfg.Prices
}

// snapshotConfig returns a shallow copy of the current config under a brief
// read lock. Used by the web account-add flow, which passes the snapshot to the
// add cores (addApikeyAccount/addVolcengineAccount) so they see a consistent cfg
// without holding p.mu during their network validation call (usage_url probe).
// The Providers/Routes maps are shared with the live cfg (shallow copy) — that's
// safe because the cores only READ them; a concurrent reload swaps p.cfg to a
// brand-new *Config, it never mutates the maps in place.
func (p *Proxy) snapshotConfig() *Config {
	p.mu.RLock()
	defer p.mu.RUnlock()
	c := *p.cfg
	return &c
}

// catalogSnapshot returns the current models.dev catalog under a brief read lock,
// or nil if unavailable (tests, or the best-effort load failed). Request-aware
// routing (context-window fallback, capability routing) degrades to a no-op when
// nil — the proxy forwards unchanged rather than guessing.
func (p *Proxy) catalogSnapshot() *catalog.Catalog {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.catalog
}

// initCatalog loads the models.dev metadata catalog best-effort (context window
// + modalities for request-aware routing). On failure p.catalog stays nil and
// the proxy runs without request-aware routing (forwards unchanged). Called from
// runProxy only — direct NewProxy callers (tests) stay offline; tests that need
// metadata set p.catalog directly.
func (p *Proxy) initCatalog() {
	cat, err := app.LoadModelsCatalog(cliframework.HomeDir(), false)
	if err != nil || cat == nil {
		if err != nil {
			log.Printf("[models] catalog load failed: %v - running without request-aware routing", err)
		}
		return
	}
	p.mu.Lock()
	p.catalog = cat
	p.mu.Unlock()
}

// providerSnapshot returns the current provider map under a brief read lock.
func (p *Proxy) providerSnapshot() map[string]provider.Provider {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.providers
}

func runtimeRouteKeys(
	routes map[string][]RouteTarget,
	implicit map[string]RouteTarget,
) map[string]bool {
	keys := make(map[string]bool, len(routes)+len(implicit))
	for route := range routes {
		keys[route] = true
	}
	for route := range implicit {
		keys[route] = true
	}
	return keys
}

// healthConfigFingerprint delegates to internal/app.HealthConfigFingerprint.
func healthConfigFingerprint(cfg *Config) string {
	return app.HealthConfigFingerprint(cfg)
}

// snapshotPersistedState takes the one authoritative persistence snapshot under
// a single generation and the repository lock order p.mu -> runtimeState.
// No reload or request-state mutation can interleave cfg fingerprint,
// health/sticky, or quota snapshots. SnapshotForPersist takes the runtime lock
// exactly once, so quota and health can no longer be observed from different
// generations.
func (p *Proxy) snapshotPersistedState() runtimestate.PersistedFullSnapshot {
	p.mu.RLock()
	runtimeSnapshot := p.runtimeState.SnapshotForPersist(
		runtimeRouteKeys(p.cfg.Routes, p.implicitRoutes),
		time.Now(),
	)
	providers := make(map[string]runtimestate.PersistedQuotaSnapshot, len(runtimeSnapshot.Quotas))
	for k, v := range runtimeSnapshot.Quotas {
		providers[k] = runtimestate.PersistedQuotaSnapshot{
			Billing: v.Billing, RemainingPct: v.RemainingPct,
			Windows: v.Windows, AsOf: v.AsOf, Err: v.Err,
		}
	}
	s := runtimestate.PersistedFullSnapshot{
		Providers:  providers,
		Sticky:     runtimeSnapshot.Sticky,
		Health:     runtimeSnapshot.Health,
		HealthFP:   healthConfigFingerprint(p.cfg),
		Generation: runtimeSnapshot.Generation,
		WireCaps:   p.wireCapsSnapshot(),
	}
	p.mu.RUnlock()
	return s
}
