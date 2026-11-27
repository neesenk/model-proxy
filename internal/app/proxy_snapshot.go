package app

import (
	"context"
	"model-proxy/internal/accounts"
	"model-proxy/internal/observe/logx"
	runtimestate "model-proxy/internal/runtime"
	"os"
	"time"

	"model-proxy/internal/catalog"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/pricing"
	"model-proxy/internal/provider"
	"model-proxy/internal/providerbuild"
)

// cfgSnapshot returns the current config under a brief read lock. Used by the
// quota tracker (which reads cfg asynchronously from its poll goroutine).
func (p *Proxy) cfgSnapshot() *configdomain.Config {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cfg
}

// pricingRetryBackoff is how long a failed or fruitless pricing refresh keeps
// the next one from starting. An unreachable source must cost one background
// attempt per interval, not one per request.
const pricingRetryBackoff = time.Minute

// pricingSnapshot returns the best pricing catalog available RIGHT NOW: the
// last refresh's result, else the disk cache seeded at boot, else an empty
// catalog. It never waits on the network. A catalog that is missing or older
// than the TTL starts one background refresh (single flight, with a backoff
// after failures); the callers that came first are served the previous
// catalog and pick the fresh one up on their next call. Before this, a stale
// or missing cache made every /api/sessions and /api/analytics request fetch
// the source synchronously: with the source unreachable each of them waited
// out the fetch timeout (2026-11-27, measured 5s per request in a sandbox
// without a route to the pricing source; up to the 10s client timeout).
func (p *Proxy) pricingSnapshot() *pricing.Catalog {
	if p == nil {
		return nil
	}
	cfg := p.cfgSnapshot()
	if cfg == nil || !cfg.Pricing.IsEnabled() {
		return nil
	}
	p.refreshPricingIfDue(pricingRefreshOptions(cfg), false)
	p.pricingMu.Lock()
	catalog := p.pricing
	p.pricingMu.Unlock()
	if catalog == nil {
		return pricing.Empty()
	}
	return catalog
}

// pricingRefreshOptions describes one config generation's pricing source.
func pricingRefreshOptions(cfg *configdomain.Config) pricing.RefreshOptions {
	return pricing.RefreshOptions{
		CacheFile: pricing.CachePath(accounts.HomeDir()),
		Endpoint:  cfg.Pricing.ResolvedSourceURL(),
		Fetch:     pricing.FetchHTTP,
		TTL:       cfg.Pricing.TTLDuration(),
		Warnings:  os.Stderr,
	}
}

// initPricing seeds the snapshot from the disk cache (a local read, no
// network) and, when that is missing or stale, starts the background refresh
// right away — a restart never leaves its first pricing-needing request to
// discover the state. Called at boot and reload.
func (p *Proxy) initPricing() {
	cfg := p.cfgSnapshot()
	if cfg == nil || !cfg.Pricing.IsEnabled() {
		return
	}
	options := pricingRefreshOptions(cfg)
	if cached, _ := pricing.LoadCached(options.CacheFile); cached != nil {
		p.pricingMu.Lock()
		if p.pricing == nil || cached.FetchedAt.After(p.pricing.FetchedAt) {
			p.pricing = cached
		}
		p.pricingMu.Unlock()
	}
	p.refreshPricingIfDue(options, false)
}

// refreshPricingIfDue starts one background refresh when the catalog is
// missing or older than the TTL and no refresh is in flight; after a failed
// or fruitless refresh it waits for the backoff so an unreachable source is
// not hammered. A reload that changed the source, the cache file or the TTL
// re-evaluates from scratch (the backoff of the old source is dropped).
func (p *Proxy) refreshPricingIfDue(options pricing.RefreshOptions, force bool) {
	if p.pricingLoader == nil {
		return
	}
	key := options.Endpoint + "\x00" + options.CacheFile + "\x00" + options.TTL.String()
	now := time.Now()
	p.pricingMu.Lock()
	if key != p.pricingKey {
		p.pricingKey, p.pricingRetryAt = key, time.Time{}
	}
	due := force || p.pricing == nil || now.Sub(p.pricing.FetchedAt) >= options.TTL
	if !due || p.pricingRefreshing || now.Before(p.pricingRetryAt) {
		p.pricingMu.Unlock()
		return
	}
	p.pricingRefreshing = true
	p.pricingMu.Unlock()
	// The lifecycle owns the task (stop/wait at shutdown). A lifecycle that is
	// already stopping admits no task: the request path then keeps serving the
	// last catalog, which is all a draining process needs.
	if !p.lifecycle.Run(func(stop <-chan struct{}) {
		stopped := options
		stopped.Fetch = pricingFetchUntilStop(stop)
		p.refreshPricing(stopped)
	}) {
		p.pricingMu.Lock()
		p.pricingRefreshing = false
		p.pricingMu.Unlock()
	}
}

// pricingFetchUntilStop is the refresh's fetch: it aborts as soon as stop
// closes, so shutdown never waits out the pricing source's timeout.
func pricingFetchUntilStop(stop <-chan struct{}) pricing.FetchFunc {
	return func(endpoint, etag string) (int, []byte, string, error) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			select {
			case <-stop:
				cancel()
			case <-ctx.Done():
			}
		}()
		return pricing.FetchHTTPContext(ctx, endpoint, etag)
	}
}

// refreshPricing is the background task: it installs the loader's result and
// records whether the source answered. EnsureFresh reports a stale cache
// served after a failed fetch as a success — so staleness, not the error
// alone, decides the backoff.
func (p *Proxy) refreshPricing(options pricing.RefreshOptions) {
	catalog, err := p.pricingLoader(options)
	p.pricingMu.Lock()
	defer p.pricingMu.Unlock()
	p.pricingRefreshing = false
	switch {
	case err != nil || catalog == nil:
		p.pricingRetryAt = time.Now().Add(pricingRetryBackoff)
	case time.Since(catalog.FetchedAt) >= options.TTL:
		p.pricing = catalog
		p.pricingRetryAt = time.Now().Add(pricingRetryBackoff)
	default:
		p.pricing = catalog
		p.pricingRetryAt = time.Time{}
	}
}

// priceOverrides returns the current config `prices:` overrides for the handler.
func (p *Proxy) priceOverrides() map[string]configdomain.PriceConfig {
	cfg := p.cfgSnapshot()
	if cfg == nil {
		return nil
	}
	return cfg.Prices
}

// pricingAliases returns the provider alias index for price fallback:
// pricing.AliasKey(provider, upstreamModel) → exposed name, from each
// provider's config `alias:` map. Same cfgSnapshot discipline as
// priceOverrides (brief RLock, no live-map escape: the result is rebuilt).
func (p *Proxy) pricingAliases() map[string]string {
	cfg := p.cfgSnapshot()
	if cfg == nil {
		return nil
	}
	var out map[string]string
	for name, prov := range cfg.Providers {
		for model, alias := range prov.Alias {
			if alias == "" || alias == model {
				continue
			}
			if out == nil {
				out = map[string]string{}
			}
			out[pricing.AliasKey(name, model)] = alias
		}
	}
	return out
}

// detachedPricing returns the current pricing catalog plus a detached COPY of
// the configured price overrides (config `prices:` values are generation-
// immutable, but callers must never receive the live map). Shared by the
// admin ports so the PriceConfig → pricing.Override conversion has exactly one
// owner.
func (p *Proxy) detachedPricing() (map[string]pricing.Override, *pricing.Catalog, map[string]string) {
	catalog := p.pricingSnapshot()
	overrides := p.priceOverrides()
	var detached map[string]pricing.Override
	if overrides != nil {
		detached = make(map[string]pricing.Override, len(overrides))
		for model, price := range overrides {
			detached[model] = pricing.Override{
				Input:      price.Input,
				Output:     price.Output,
				CacheRead:  price.CacheRead,
				CacheWrite: price.CacheWrite,
			}
		}
	}
	return detached, catalog, p.pricingAliases()
}

// snapshotConfig returns a shallow copy of the current config under a brief
// read lock. Used by the web account-add flow, which passes the snapshot to the
// add cores (addApikeyAccount/addVolcengineAccount) so they see a consistent cfg
// without holding p.mu during their network validation call (usage_url probe).
// The Providers/Routes maps are shared with the live cfg (shallow copy) — that's
// safe because the cores only READ them; a concurrent reload swaps p.cfg to a
// brand-new *Config, it never mutates the maps in place.
func (p *Proxy) snapshotConfig() *configdomain.Config {
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
// the proxy runs without request-aware routing (forwards unchanged). The load
// runs through p.catalogLoader (production: configdomain.LoadModelsCatalog;
// test stub: newTestProxyAt) so the reload-triggered refresh is injectable.
// Direct NewProxy callers (tests) stay offline; tests that need metadata set
// p.catalog directly.
func (p *Proxy) initCatalog() {
	cat, err := p.catalogLoader(accounts.HomeDir(), false)
	if err != nil || cat == nil {
		if err != nil {
			logx.Warnf("[models] catalog load failed: %v - running without request-aware routing", err)
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
	routes map[string][]configdomain.RouteTarget,
	derived map[string][]configdomain.RouteTarget,
) map[string]bool {
	keys := make(map[string]bool, len(routes)+len(derived))
	for route := range routes {
		keys[route] = true
	}
	for route := range derived {
		keys[route] = true
	}
	return keys
}

// healthConfigFingerprint delegates to providerbuild.HealthConfigFingerprint.
func healthConfigFingerprint(cfg *configdomain.Config) string {
	return providerbuild.HealthConfigFingerprint(cfg)
}

// snapshotPersistedState takes the one authoritative persistence snapshot under
// a single generation and the repository lock order p.mu -> runtimeState.
// No reload or request-state mutation can interleave cfg fingerprint,
// health/sticky, or quota snapshots. SnapshotForPersist takes the runtime lock
// exactly once, so quota and health can no longer be observed from different
// generations.
func (p *Proxy) snapshotPersistedState() runtimestate.PersistedFullSnapshot {
	p.mu.RLock()
	RuntimeSnapshot := p.runtimeState.SnapshotForPersist(
		runtimeRouteKeys(p.cfg.Routes, p.derivedRoutes),
		time.Now(),
	)
	providers := make(map[string]runtimestate.PersistedQuotaSnapshot, len(RuntimeSnapshot.Quotas))
	for k, v := range RuntimeSnapshot.Quotas {
		providers[k] = runtimestate.PersistedQuotaSnapshot{
			Billing: v.Billing, RemainingPct: v.RemainingPct,
			Windows: v.Windows, Notes: v.Notes, AsOf: v.AsOf, Err: v.Err,
		}
	}
	s := runtimestate.PersistedFullSnapshot{
		Providers:  providers,
		Sticky:     RuntimeSnapshot.Sticky,
		Health:     RuntimeSnapshot.Health,
		HealthFP:   healthConfigFingerprint(p.cfg),
		Generation: RuntimeSnapshot.Generation,
		WireCaps:   p.wireCapsSnapshot(),
	}
	p.mu.RUnlock()
	return s
}

// Read-only assembly seams for the composition root and its contract tests.
func (p *Proxy) StatsStore() any                    { return p.stats }
func (p *Proxy) Flusher() any                       { return p.flusher }
func (p *Proxy) Lifecycle() *runtimestate.Lifecycle { return p.lifecycle }

// CatalogSnapshot returns the current models.dev catalog under a brief read
// lock (nil when unavailable).
func (p *Proxy) CatalogSnapshot() *catalog.Catalog { return p.catalogSnapshot() }
