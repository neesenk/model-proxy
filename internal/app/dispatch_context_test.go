package app

import (
	"testing"

	"model-proxy/internal/catalog"
	"model-proxy/internal/provider"
)

func TestRuntimeSnapshotKeepsOneReloadGeneration(t *testing.T) {
	p := newTestProxy(t, &Config{
		Providers: map[string]Provider{"old": {Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"m": {{Provider: "old", Model: "old-model"}}},
	})
	p.catalog = catalog.New(nil)

	snapshot := p.SnapshotRuntime()
	if snapshot.Cfg != p.cfg ||
		snapshot.Generation != p.configGeneration.Load() ||
		snapshot.Providers["old"] == nil ||
		len(snapshot.ExpandedRoutes["m"]) != 1 ||
		snapshot.Catalog != p.catalog ||
		snapshot.Cache != p.cache {
		t.Fatalf("incomplete runtime snapshot: %+v", snapshot)
	}

	newCfg := &Config{
		Providers: map[string]Provider{"new": {Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"m": {{Provider: "new", Model: "new-model"}}},
	}
	p.mu.Lock()
	p.cfg = newCfg
	p.providers = map[string]provider.Provider{}
	p.expandedRoutes = newCfg.Routes
	p.configGeneration.Add(1)
	p.mu.Unlock()

	if snapshot.Cfg.Providers["old"].Provider != testProviderID ||
		snapshot.ExpandedRoutes["m"][0].Provider != "old" ||
		snapshot.Generation == p.configGeneration.Load() {
		t.Fatalf("captured snapshot changed across reload swap: %+v", snapshot)
	}
}
