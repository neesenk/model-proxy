package main

import (
	"model-proxy/internal/runtime"
	"model-proxy/provider"
)

// newStandaloneQuotaTracker builds a QuotaTracker with an isolated Manager for
// tests (no Proxy lifecycle).
func newStandaloneQuotaTracker(
	path string,
	cfg func() *Config,
	provs func() map[string]provider.Provider,
) *runtime.QuotaTracker {
	return runtime.NewQuotaTracker(path, cfg, provs, runtime.NewManager(0))
}
