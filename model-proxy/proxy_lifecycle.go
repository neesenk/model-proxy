package main

import (
	"log"
	observestats "model-proxy/internal/observe/stats"
)

// startRuntimeServices initializes and starts process-owned optional services.
// Direct NewProxy callers stay lightweight until this method is called.
func (p *Proxy) startRuntimeServices(cfg *Config) {
	p.initStats(cfg.Stats)
	p.lifecycle.Run(func(stop <-chan struct{}) {
		p.statsFlushLoop(stop)
	})

	p.initRequestLog(cfg.RequestLog)
	if p.reqLog != nil {
		p.reqLogStarted = p.lifecycle.Run(func(<-chan struct{}) {
			p.reqLog.Run()
		})
	}

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
// reject new Proxy-owned tasks → wait finite log-producing work → drain request
// log → wait for periodic loops/refreshes → final stats/state flushes.
func (p *Proxy) closeRuntimeServices() {
	p.lifecycle.BeginStop()
	p.lifecycle.WaitBeforeLogDrain()
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
