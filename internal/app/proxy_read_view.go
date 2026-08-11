package app

import (
	"encoding/json"
	obscounters "model-proxy/internal/observe/counters"
	"time"

	"model-proxy/internal/fusion"
	observestats "model-proxy/internal/observe/stats"
	"model-proxy/internal/pricing"
)

// proxyReadView is the read-only application boundary used by Web/API
// handlers. It owns Proxy lock discipline and returns detached snapshots so
// transport code never reaches into runtime maps or mutexes directly.
type proxyReadView struct {
	proxy *Proxy
}

func (p *Proxy) readView() proxyReadView {
	return proxyReadView{proxy: p}
}

type dashboardView struct {
	uptime     string
	listen     string
	health     map[string]any
	modelLocks map[string][]map[string]any
	quota      map[string]any
	schedule   json.RawMessage
	counters   map[string]obscounters.ProviderMetricsSnapshot
	cache      map[string]any
	warnings   []string
}

func (view proxyReadView) dashboard(now time.Time) dashboardView {
	p := view.proxy

	// Capture reload-owned values and the Manager dashboard under the repository
	// lock order so config and generation-scoped state cannot cross generations.
	p.mu.RLock()
	cfg := p.cfg
	listen := cfg.Listen
	warnings := append([]string(nil), p.routeWarnings...)
	cache := p.cache
	expanded := p.expandedRoutes
	parentOf := p.parentOf
	poolIndex := p.poolIndex
	RuntimeSnapshot := p.runtimeState.Dashboard(now)
	p.mu.RUnlock()
	schedule := scheduleStatusFromSnapshot(
		cfg,
		expanded,
		parentOf,
		poolIndex,
		RuntimeSnapshot,
		now,
	)
	health := make(map[string]any, len(RuntimeSnapshot.Providers))
	for name, state := range RuntimeSnapshot.Providers {
		entry := map[string]any{
			"circuit_state": state.CircuitState,
			"available":     state.Available,
		}
		if now.Before(state.CircuitOpenUntil) {
			entry["circuit_until"] = state.CircuitOpenUntil.UTC().Format(time.RFC3339)
		}
		if now.Before(state.RateLimitedUntil) {
			entry["rate_limited_until"] = state.RateLimitedUntil.UTC().Format(time.RFC3339)
			entry["rate_limit_kind"] = state.RateLimitKind.String()
		}
		health[name] = entry
	}
	modelLocks := map[string][]map[string]any{}
	for providerName, locks := range RuntimeSnapshot.ModelLocks {
		for _, lock := range locks {
			modelLocks[providerName] = append(modelLocks[providerName], map[string]any{
				"model": lock.Model,
				"until": lock.LockedUntil.UTC().Format(time.RFC3339),
			})
		}
	}

	var quota map[string]any
	if p.quota != nil {
		if snapshots := RuntimeSnapshot.Quotas; snapshots != nil {
			quota = make(map[string]any, len(snapshots))
			for key, snapshot := range snapshots {
				quota[key] = snapshot
			}
		}
	}

	cacheInfo := map[string]any{"enabled": false}
	if stats := cache.Stats(); stats.Entries > 0 || stats.Hits > 0 || cache != nil {
		cacheInfo = map[string]any{
			"enabled": true,
			"hits":    stats.Hits,
			"misses":  stats.Misses,
			"entries": stats.Entries,
		}
	}

	return dashboardView{
		uptime:     time.Since(p.metrics.StartedAt()).String(),
		listen:     listen,
		health:     health,
		modelLocks: modelLocks,
		quota:      quota,
		schedule:   json.RawMessage(schedule),
		counters:   p.metrics.AggregateByProvider(),
		cache:      cacheInfo,
		warnings:   warnings,
	}
}

func (view proxyReadView) logFile() string {
	view.proxy.mu.RLock()
	defer view.proxy.mu.RUnlock()
	return view.proxy.cfg.LogFile
}

func (view proxyReadView) config() *Config {
	return view.proxy.snapshotConfig()
}

func (view proxyReadView) providerConfig(name string) (Provider, bool) {
	view.proxy.mu.RLock()
	defer view.proxy.mu.RUnlock()
	config, ok := view.proxy.cfg.Providers[name]
	return config, ok
}

func (view proxyReadView) providerConfigs() map[string]Provider {
	view.proxy.mu.RLock()
	defer view.proxy.mu.RUnlock()
	configs := make(map[string]Provider, len(view.proxy.cfg.Providers))
	for name, config := range view.proxy.cfg.Providers {
		configs[name] = config
	}
	return configs
}

func (view proxyReadView) requestLogDirectory() string {
	return view.proxy.reqLog.Directory()
}

func (view proxyReadView) tokenUsageSnapshot() map[obscounters.TokenKey]obscounters.TokenUsage {
	if view.proxy.tokens == nil {
		return nil
	}
	return view.proxy.tokens.Snapshot()
}

func (view proxyReadView) stats(from, to int64, provider, model string, bucketSecs int64) ([]observestats.Bucket, error) {
	if view.proxy.stats == nil {
		return []observestats.Bucket{}, nil
	}
	return view.proxy.stats.QueryRange(from, to, provider, model, bucketSecs)
}

func (view proxyReadView) agentStats(from, to int64, agent, provider, model string, bucketSecs int64) ([]observestats.AgentBucket, error) {
	if view.proxy.stats == nil {
		return []observestats.AgentBucket{}, nil
	}
	return view.proxy.stats.QueryAgents(from, to, agent, provider, model, bucketSecs)
}

func (view proxyReadView) analytics(from, to int64, provider, model, granularity string) ([]observestats.AnalyticsBucket, error) {
	if view.proxy.stats == nil {
		return []observestats.AnalyticsBucket{}, nil
	}
	return view.proxy.stats.QueryAnalytics(from, to, provider, model, granularity)
}

func (view proxyReadView) fusion(workflow string, now time.Time) (map[string]fusion.WorkflowStats, []fusion.Run) {
	return view.proxy.fusionReg.Snapshot(workflow, now)
}

func (view proxyReadView) pins() map[string]pinEntry {
	return view.proxy.listPins()
}

type pricingView struct {
	catalog   *pricing.Catalog
	overrides map[string]pricing.Override
}

func (view proxyReadView) pricing() pricingView {
	catalog := view.proxy.pricingSnapshot()
	overrides := view.proxy.priceOverrides()
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
	return pricingView{catalog: catalog, overrides: detached}
}
