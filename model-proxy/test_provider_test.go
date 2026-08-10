package main

import "model-proxy/provider"

// testProviderID mirrors the app-package test provider registration for the
// remaining root CLI/integration tests.
const testProviderID = "test-static"

func init() {
	provider.Register(testProviderID, func(cfg *provider.Config, providerName string) (provider.Provider, error) {
		staticCfg := *cfg
		staticCfg.ProviderID = "static"
		return provider.New(&staticCfg, providerName)
	})
}
