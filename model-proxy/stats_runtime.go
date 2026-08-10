package main

import (
	"context"
	"log"
	obscounters "model-proxy/internal/observe/counters"
	"time"

	observestats "model-proxy/internal/observe/stats"
)

func (p *Proxy) statsFlushLoop(stop <-chan struct{}) {
	if p.flusher == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	for {
		timer := time.NewTimer(observestats.UntilNextMinute(time.Now()))
		select {
		case <-timer.C:
			p.flusher.FlushContextCycle(ctx, time.Now())
			if ctx.Err() != nil {
				return
			}
		case <-stop:
			timer.Stop()
			return
		}
	}
}

// initStats binds startup-only config to the long-lived Store, imports the
// legacy token file once, restores cumulative hot counters, and seeds the diff
// baseline. Stats deliberately survives config reload generations.
func (p *Proxy) initStats(config StatsConfig) {
	path := config.ResolvedDBPath()
	store, err := observestats.Open(observestats.Options{
		Path: path, Retention: config.RetentionDuration(),
	})
	if err != nil {
		log.Printf("[stats] open failed (%s): %v - running without persisted stats", path, err)
		return
	}
	p.stats = store

	if count, err := store.ImportLegacyTokens(observestats.LegacyTokensPath(homeDir())); err != nil {
		log.Printf("[stats] legacy token_usage.json migration failed: %v", err)
	} else if count > 0 {
		log.Printf("[stats] imported %d entries from legacy token_usage.json", count)
	}

	baseline, err := store.LoadCumulative()
	if err != nil {
		log.Printf("[stats] load baseline failed: %v", err)
		baseline = map[observestats.Key]observestats.Counters{}
	}
	for key, base := range baseline {
		runtimeKey := obscounters.PMKey{Provider: key.Provider, Model: key.Model}
		p.metrics.Seed(runtimeKey, obscounters.ProviderMetricsSnapshot{
			Requests: base.Requests, Failovers: base.Failovers,
			RateLimited429: base.RateLimited429, Failures: base.Failures,
			LastRequestAt: base.LastRequestAt, LatencySum: base.LatencySum,
			TTFTSum: base.TTFTSum,
		})
		p.tokens.Seed(runtimeKey, obscounters.TokenUsage{
			Input: base.Input, Output: base.Output,
			CacheCreation: base.CacheCreation, CacheRead: base.CacheRead,
			Requests: base.TokenRequests,
		})
	}
	p.flusher = observestats.NewFlusher(store, p.metrics, p.tokens, p.agents, baseline)
}
