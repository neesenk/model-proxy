package main

import (
	runtimestate "model-proxy/internal/runtime"
	"model-proxy/provider"
)

func newStandaloneQuotaTracker(
	path string,
	cfg func() *Config,
	provs func() map[string]provider.Provider,
) *quotaTracker {
	return newQuotaTracker(path, cfg, provs, runtimestate.NewManager(0))
}

func (t *quotaTracker) setSnapshot(name string, snapshot *provider.QuotaSnapshot) {
	t.runtime.SetQuota(name, snapshot, 0)
}

func (t *quotaTracker) snapshot(name string) *provider.QuotaSnapshot {
	return t.runtime.Quota(name)
}
