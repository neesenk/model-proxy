package main

import (
	"encoding/json"
	"sort"
	"time"

	"model-proxy/provider"
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
	counters   map[string]providerMetricsSnapshot
	cache      map[string]any
	warnings   []string
}

func (view proxyReadView) dashboard(now time.Time) dashboardView {
	p := view.proxy

	// Reload-owned values are captured together, then their locks are released
	// before health/quota/component snapshots are taken.
	p.mu.RLock()
	listen := p.cfg.Listen
	warnings := append([]string(nil), p.routeWarnings...)
	cache := p.cache
	p.mu.RUnlock()

	p.healthMu.Lock()
	health := make(map[string]any, len(p.health))
	for name, state := range p.health {
		circuitState := "closed"
		switch {
		case now.Before(state.circuitOpenUntil):
			circuitState = "open"
		case state.halfOpenInFlight:
			circuitState = "half_open"
		}
		entry := map[string]any{
			"circuit_state": circuitState,
			"available":     state.available(now),
		}
		if now.Before(state.circuitOpenUntil) {
			entry["circuit_until"] = state.circuitOpenUntil.UTC().Format(time.RFC3339)
		}
		if now.Before(state.rateLimitedUntil) {
			entry["rate_limited_until"] = state.rateLimitedUntil.UTC().Format(time.RFC3339)
			entry["rate_limit_kind"] = state.rateLimitKind.String()
		}
		health[name] = entry
	}
	modelLocks := map[string][]map[string]any{}
	for key, entry := range p.modelLocks {
		if !now.Before(entry.lockedUntil) {
			continue
		}
		modelLocks[key.provider] = append(modelLocks[key.provider], map[string]any{
			"model": key.model,
			"until": entry.lockedUntil.UTC().Format(time.RFC3339),
		})
	}
	p.healthMu.Unlock()
	for _, locks := range modelLocks {
		sort.Slice(locks, func(i, j int) bool {
			return locks[i]["model"].(string) < locks[j]["model"].(string)
		})
	}

	var quota map[string]any
	if p.quota != nil {
		if snapshots := p.quota.allSnapshots(); snapshots != nil {
			quota = make(map[string]any, len(snapshots))
			for key, snapshot := range snapshots {
				quota[key] = snapshot
			}
		}
	}

	cacheInfo := map[string]any{"enabled": false}
	if hits, misses, entries := cache.stats(); entries > 0 || hits > 0 || cache != nil {
		cacheInfo = map[string]any{
			"enabled": true,
			"hits":    hits,
			"misses":  misses,
			"entries": entries,
		}
	}

	return dashboardView{
		uptime:     time.Since(p.metrics.startedAt()).String(),
		listen:     listen,
		health:     health,
		modelLocks: modelLocks,
		quota:      quota,
		schedule:   json.RawMessage(p.scheduleStatus()),
		counters:   p.metrics.aggregateByProvider(),
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

func (view proxyReadView) runtimeProvider(name string) provider.Provider {
	view.proxy.mu.RLock()
	defer view.proxy.mu.RUnlock()
	return view.proxy.providers[name]
}
