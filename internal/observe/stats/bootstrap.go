package stats

import (
	"model-proxy/internal/observe/logx"
	"time"

	obscounters "model-proxy/internal/observe/counters"
)

// BootstrapResult carries the opened Store plus the Flusher seeded with the
// cumulative baseline. Both are nil when the store could not be opened.
type BootstrapResult struct {
	Store   *Store
	Flusher *Flusher
}

// Bootstrap opens the durable Store, imports the legacy token file once,
// restores cumulative hot counters into metrics + tokens, and seeds the
// flusher's diff baseline. Stats deliberately survives config reload
// generations, so the caller (Proxy runtime services) owns the returned
// instances across reloads. On open failure it logs and returns zero values —
// the proxy keeps running with in-memory-only counters.
func Bootstrap(
	path string,
	retention time.Duration,
	homeDir string,
	metrics *obscounters.MetricsStore,
	tokens *obscounters.TokenCounter,
	agents *obscounters.AgentCounter,
) BootstrapResult {
	store, err := Open(Options{Path: path, Retention: retention})
	if err != nil {
		logx.Warnf("[stats] open failed (%s): %v - running without persisted stats", path, err)
		return BootstrapResult{}
	}

	if count, err := store.ImportLegacyTokens(LegacyTokensPath(homeDir)); err != nil {
		logx.Warnf("[stats] legacy token_usage.json migration failed: %v", err)
	} else if count > 0 {
		logx.Infof("[stats] imported %d entries from legacy token_usage.json", count)
	}

	baseline, err := store.LoadCumulative()
	if err != nil {
		logx.Warnf("[stats] load baseline failed: %v", err)
		baseline = map[Key]Counters{}
	}
	for key, base := range baseline {
		runtimeKey := obscounters.PMKey{Provider: key.Provider, Model: key.Model}
		metrics.Seed(runtimeKey, obscounters.ProviderMetricsSnapshot{
			Requests: base.Requests, Failovers: base.Failovers,
			RateLimited429: base.RateLimited429, Failures: base.Failures,
			LastRequestAt: base.LastRequestAt, LatencySum: base.LatencySum,
			TTFTSum: base.TTFTSum, DurationSum: base.DurationSum,
		})
		tokens.Seed(runtimeKey, obscounters.TokenUsage{
			Input: base.Input, Output: base.Output,
			CacheCreation: base.CacheCreation, CacheRead: base.CacheRead,
			Requests: base.TokenRequests,
		})
	}
	agentBaseline, err := store.LoadCumulativeAgents()
	if err != nil {
		logx.Warnf("[stats] load agent baseline failed: %v", err)
		agentBaseline = map[AgentKey]AgentCounters{}
	}
	for key, base := range agentBaseline {
		agents.Seed(obscounters.AgentKey{Agent: key.Agent, Provider: key.Provider, Model: key.Model},
			obscounters.AgentCount{
				Requests: base.Requests, Input: base.Input, Output: base.Output,
				CacheCreation: base.CacheCreation, CacheRead: base.CacheRead,
				LatencySum: base.LatencySum, Failures: base.Failures,
			})
	}
	return BootstrapResult{
		Store:   store,
		Flusher: NewFlusher(store, metrics, tokens, agents, baseline, agentBaseline),
	}
}
