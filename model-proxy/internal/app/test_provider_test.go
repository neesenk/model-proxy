package app

import "model-proxy/provider"

// testProviderID gives proxy behavior tests a credential-independent upstream.
// Historically those tests used static, which accidentally coupled routing,
// lifecycle, cache, Fusion, Shadow, and protocol fixtures to static's account
// storage policy. Keep the same provider behavior by delegating to static's
// constructor, but use a distinct registry key so static remains available only
// to tests that deliberately exercise its plural-pool credential contract.
const testProviderID = "test-static"

func init() {
	provider.Register(testProviderID, func(cfg *provider.Config, providerName string) (provider.Provider, error) {
		staticCfg := *cfg
		staticCfg.ProviderID = "static"
		return provider.New(&staticCfg, providerName)
	})
}
