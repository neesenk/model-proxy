// proxy_lifecycle.go — process-owned runtime services lifecycle (start, shutdown/drain order).
package app

import (
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/observe/logx"
	observestats "model-proxy/internal/observe/stats"
	"time"
)

// startRuntimeServices initializes and starts process-owned optional services.
// Direct NewProxy callers stay lightweight until this method is called.
func (p *Proxy) StartRuntimeServices(cfg *configdomain.Config) {
	p.initStats(cfg.Stats)
	p.lifecycle.Run(func(stop <-chan struct{}) {
		p.statsFlushLoop(stop)
	})

	p.initRequestLog(cfg.RequestLog)
	if p.reqLog != nil {
		p.reqLogStarted = p.lifecycle.Run(func(<-chan struct{}) {
			p.reqLog.Run()
		})
		if p.reqLogIndex != nil {
			p.reqLogIndexStarted = p.lifecycle.Run(func(<-chan struct{}) {
				p.reqLogIndex.Run()
			})
		}
	}
	if p.mcpReqLog != nil {
		p.mcpReqLogStarted = p.lifecycle.Run(func(<-chan struct{}) {
			p.mcpReqLog.Run()
		})
	}

	// Security audit log (guard.audit): reload-owned. The boot generation
	// reconciles here; Reload reconciles again per generation (audit off→on
	// starts it, on→off drains+stops it, audit_path changes swap the file).
	p.reconcileSecLog(cfg)

	// Startup catalog loading remains synchronous so the first request gets the
	// best available routing metadata, matching the previous daemon behavior.
	p.initCatalog()
	// Same shape for pricing: the disk cache seeds the snapshot now, and a
	// missing or stale one starts refreshing in the background — the request
	// path never fetches it (see pricingSnapshot).
	p.initPricing()
}

func (p *Proxy) refreshCatalogAsync() {
	p.lifecycle.Run(func(<-chan struct{}) {
		p.initCatalog()
	})
}

// closeRuntimeServices establishes one shutdown order:
// reject new Proxy-owned tasks → drain the security audit log → drain request
// log → wait for periodic loops/refreshes → final stats/state flushes.
func (p *Proxy) closeRuntimeServices() {
	p.lifecycle.BeginStop()
	// Reap stdio MCP children first: they write no logs, and killing them
	// early unblocks any in-flight stdio Call so request goroutines can exit
	// before the log drain.
	if p.mcpStdio != nil {
		p.mcpStdio.killAll()
	}
	// Stop the AI adjudication workers BEFORE the audit logger drains:
	// verdict-side audit records must not enqueue into a drained logger
	// (jobs past the drain deadline are the documented shutdown-loss window,
	// same direction as the seclog swap window).
	if p.adjudication != nil {
		p.adjudication.Close(5 * time.Second)
	}
	// Drain the security audit log before the request log: both must finish
	// writing before Close returns, and no producer may outlive either. Only
	// the CURRENT generation's logger is drained here — reconcileSecLog already
	// drained+stopped every swapped-out one.
	p.mu.RLock()
	secLog, secLogRunning := p.secLog, p.secLogRunning
	p.mu.RUnlock()
	if secLogRunning && secLog != nil {
		secLog.Shutdown()
	}
	if p.reqLogStarted {
		p.reqLog.Shutdown()
	}
	// The split MCP stream drains alongside the main log (same restart-only
	// lifecycle; no indexer follows it, so no ordering constraint).
	if p.mcpReqLogStarted {
		p.mcpReqLog.Shutdown()
	}
	// Drain order matters: the indexer's Shutdown runs one final reconcile
	// after the log writer drained, so the last committed records are indexed
	// before the database closes.
	if p.reqLogIndexStarted {
		p.reqLogIndex.Shutdown()
	}
	p.lifecycle.Wait()

	if p.flusher != nil {
		p.flusher.FlushForShutdown(observestats.StatsShutdownFlushTimeout)
	}
	if p.stats != nil {
		if err := p.stats.Close(); err != nil {
			logx.Warnf("[stats] close failed: %v", err)
		}
	}
	if p.responsesState != nil {
		p.responsesState.Close()
	}
	if p.quota != nil {
		p.quota.Stop()
		if p.quota.Path != "" {
			if err := p.quota.Persist(); err != nil {
				logx.Warnf("[quota] final persist on close failed: %v", err)
			}
		}
	}
}
