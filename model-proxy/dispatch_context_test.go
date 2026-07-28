package main

import (
	"testing"

	"model-proxy/internal/catalog"
	"model-proxy/provider"
)

func TestRuntimeSnapshotKeepsOneReloadGeneration(t *testing.T) {
	p := newTestProxy(t, &Config{
		Providers: map[string]Provider{"old": {Provider: "static"}},
		Routes:    map[string][]RouteTarget{"m": {{Provider: "old", Model: "old-model"}}},
	})
	p.catalog = catalog.New(nil)

	snapshot := p.snapshotRuntime()
	if snapshot.cfg != p.cfg ||
		snapshot.generation != p.configGeneration.Load() ||
		snapshot.providers["old"] == nil ||
		len(snapshot.expandedRoutes["m"]) != 1 ||
		snapshot.catalog != p.catalog ||
		snapshot.cache != p.cache {
		t.Fatalf("incomplete runtime snapshot: %+v", snapshot)
	}

	newCfg := &Config{
		Providers: map[string]Provider{"new": {Provider: "static"}},
		Routes:    map[string][]RouteTarget{"m": {{Provider: "new", Model: "new-model"}}},
	}
	p.mu.Lock()
	p.cfg = newCfg
	p.providers = map[string]provider.Provider{}
	p.expandedRoutes = newCfg.Routes
	p.configGeneration.Add(1)
	p.mu.Unlock()

	if snapshot.cfg.Providers["old"].Provider != "static" ||
		snapshot.expandedRoutes["m"][0].Provider != "old" ||
		snapshot.generation == p.configGeneration.Load() {
		t.Fatalf("captured snapshot changed across reload swap: %+v", snapshot)
	}
}
