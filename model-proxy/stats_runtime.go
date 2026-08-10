package main

import (
	"context"
	cliframework "model-proxy/internal/cli/framework"
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

// initStats binds startup-only config to the long-lived Store via
// internal/observe/stats.Bootstrap.
func (p *Proxy) initStats(config StatsConfig) {
	result := observestats.Bootstrap(
		config.ResolvedDBPath(),
		config.RetentionDuration(),
		cliframework.HomeDir(),
		p.metrics,
		p.tokens,
		p.agents,
	)
	p.stats = result.Store
	p.flusher = result.Flusher
}
