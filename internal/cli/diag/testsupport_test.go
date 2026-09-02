package diag

import (
	"model-proxy/internal/provider"
)

// testProviderID gives CLI wire-record tests a credential-independent upstream.
const testProviderID = "test-static"

func init() {
	provider.Register(testProviderID, func(cfg *provider.Config, providerName string) (provider.Provider, error) {
		staticCfg := *cfg
		staticCfg.ProviderID = "static"
		return provider.New(&staticCfg, providerName)
	})
}
