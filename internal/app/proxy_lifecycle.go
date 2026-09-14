// proxy_lifecycle.go — process-owned runtime services lifecycle (start, shutdown/drain order) plus the budget watcher startup.
package app

import (
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/observe/budget"
	observeevents "model-proxy/internal/observe/events"
	"model-proxy/internal/observe/logx"
	observestats "model-proxy/internal/observe/stats"
	"model-proxy/internal/pricing"
	"net/http"
	"time"
)

// startRuntimeServices initializes and starts process-owned optional services.
// Direct NewProxy callers stay lightweight until this method is called.
func (p *Proxy) StartRuntimeServices(cfg *configdomain.Config) {
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
		if p.reqLogIndex != nil {
			p.reqLogIndexStarted = p.lifecycle.Run(func(<-chan struct{}) {
				p.reqLogIndex.Run()
			})
		}
	}

	// Cache-counter persistence (cache_state.json): owned by the lifecycle;
	// per-minute saves plus one final save in closeRuntimeServices. Register
	// even when disabled at boot: reload can enable caching later.
	if p.cacheStatePath != "" {
		p.lifecycle.Run(func(stop <-chan struct{}) {
			p.cacheSaveLoop(stop)
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
	// Final cache-counter flush: the per-minute loop is gone by now (lifecycle
	// wait), so this save is what makes shutdown lose at most the last minute.
	p.saveCacheState()
}

// budgetPorts adapts Proxy state to the budget watcher's narrow copy-by-value
// ports. Every closure returns per-tick copies — never live map references
// into reload-owned state.
func (p *Proxy) budgetPorts() budget.Ports {
	return budget.Ports{
		BudgetState: func() (configdomain.BudgetsConfig, map[string]string, bool) {
			// Capture reload-owned state once; the stats store and pricing
			// carry their own leaf locks, so nothing here nests under p.mu.
			p.mu.RLock()
			defer p.mu.RUnlock()
			if p.cfg == nil || !p.cfg.Budgets.Enabled() || p.stats == nil {
				return configdomain.BudgetsConfig{}, nil, false
			}
			budgets := p.cfg.Budgets
			providers := make(map[string]float64, len(budgets.Providers))
			for name, threshold := range budgets.Providers {
				providers[name] = threshold
			}
			budgets.Providers = providers
			parentOf := make(map[string]string, len(p.parentOf))
			for id, parent := range p.parentOf {
				parentOf[id] = parent
			}
			return budgets, parentOf, true
		},
		QueryAnalytics: func(from, to int64) ([]observestats.AnalyticsBucket, error) {
			return p.stats.QueryAnalytics(from, to, "", "", "month")
		},
		PricingSnapshot: func() (map[string]pricing.Override, *pricing.Catalog, map[string]string) {
			return p.detachedPricing()
		},
		Publish: func(event observeevents.Event) {
			p.events.Publish(event)
		},
	}
}

// startBudgetWatcher starts the per-minute budget alert loop when any
// threshold is configured; it is a no-op otherwise, so an unconfigured proxy
// runs no background task at all. Threshold changes apply on reload (each
// check reads the current config snapshot), but enabling budgets from scratch
// requires a restart — same startup-only rule as request_log.
func (p *Proxy) startBudgetWatcher() {
	cfg := p.cfgSnapshot()
	if cfg == nil || !cfg.Budgets.Enabled() {
		return
	}
	watcher := budget.NewWatcher(p.budgetPorts(), &http.Client{})
	p.budget = watcher
	p.lifecycle.Run(watcher.Loop)
}
