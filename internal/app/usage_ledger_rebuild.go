package app

// usage_ledger_rebuild.go wires the one-time historical token-ledger rebuild
// (observestats.Store.RebuildTokenLedger) into daemon startup: the request-log
// index is the authoritative usage ledger, and buckets committed before the
// 89630e1 cutoff are rewritten from it.

import (
	"path/filepath"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/observe/logx"
)

func (p *Proxy) rebuildUsageLedger(cfg *configdomain.Config) {
	if p.stats == nil || !cfg.RequestLog.Enabled {
		return
	}
	indexPath := filepath.Join(cfg.RequestLog.Dir, "index.db")
	rebuilt, skipped, err := p.stats.RebuildTokenLedger(indexPath)
	if err != nil {
		logx.Warnf("[startup] ⚠ usage ledger rebuild failed: %v (historical token buckets keep their committed values)", err)
		return
	}
	_ = rebuilt // informational: the store logs the rebuild line itself
	_ = skipped
}
