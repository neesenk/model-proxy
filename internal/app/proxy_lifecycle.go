package app

import (
	"log"
	observestats "model-proxy/internal/observe/stats"
)

// startRuntimeServices initializes and starts process-owned optional services.
// Direct NewProxy callers stay lightweight until this method is called.
func (p *Proxy) StartRuntimeServices(cfg *Config) {
	p.initStats(cfg.Stats)
	p.lifecycle.Run(func(stop <-chan struct{}) {
		p.statsFlushLoop(stop)
	})

	// Monthly equivalent-cost alerts (budgets:). Conditional: no threshold
	// configured → no background task at all. The loop is stopped and waited
	// by the lifecycle in closeRuntimeServices; it owns no durable state, so
	// there is no final flush.
	p.startBudgetWatcher()

	p.initRequestLog(cfg.RequestLog)
	if p.reqLog != nil {
		p.reqLogStarted = p.lifecycle.Run(func(<-chan struct{}) {
			p.reqLog.Run()
		})
	}

	// Security audit log (guard.audit): reload-owned. The boot generation
	// reconciles here; Reload reconciles again per generation (audit off→on
	// starts it, on→off drains+stops it, audit_path changes swap the file).
	p.reconcileSecLog(cfg)

	// Startup catalog loading remains synchronous so the first request gets the
	// best available routing metadata, matching the previous daemon behavior.
	p.initCatalog()
}

func (p *Proxy) refreshCatalogAsync() {
	p.lifecycle.Run(func(<-chan struct{}) {
		p.initCatalog()
	})
}

// closeRuntimeServices establishes one shutdown order:
// reject new Proxy-owned tasks → wait finite log-producing work → drain the
// security audit log → drain request log → wait for periodic loops/refreshes →
// final stats/state flushes.
func (p *Proxy) closeRuntimeServices() {
	p.lifecycle.BeginStop()
	p.lifecycle.WaitBeforeLogDrain()
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
	p.lifecycle.Wait()

	if p.flusher != nil {
		p.flusher.FlushForShutdown(observestats.StatsShutdownFlushTimeout)
	}
	if p.stats != nil {
		if err := p.stats.Close(); err != nil {
			log.Printf("[stats] close failed: %v", err)
		}
	}
	if p.responsesState != nil {
		p.responsesState.Close()
	}
	if p.quota != nil {
		p.quota.Stop()
		if p.quota.Path != "" {
			if err := p.quota.Persist(); err != nil {
				log.Printf("[quota] final persist on close failed: %v", err)
			}
		}
	}
}
