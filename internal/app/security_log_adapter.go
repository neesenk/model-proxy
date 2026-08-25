package app

import (
	"log"
	"path/filepath"
	"time"

	"model-proxy/internal/accounts"
	"model-proxy/internal/observe/seclog"
)

// initSecLog adapts the guard audit configuration into the process-owned
// security audit logger. Startup-only, same semantics as the request log: the
// logger owns a background goroutine + open file, so reload does not recreate
// it (the forward path consults cfg.Guard.AuditEnabled() per generation — a
// reload that turns audit off stops new records immediately; turning it on
// needs a restart). The goroutine itself is owned by proxyLifecycle.
func (p *Proxy) initSecLog(cfg *Config) {
	if !cfg.Guard.AuditEnabled() {
		return
	}
	logger, err := seclog.New(filepath.Dir(cfg.Guard.AuditPathValue(accounts.HomeDir())), seclog.Options{})
	if err != nil {
		log.Printf("[seclog] init failed: %v — security audit logging disabled", err)
		return
	}
	p.secLog = logger
}

// auditGuardHit enqueues one security audit record for a guard hit. The
// record carries pattern/path NAMES and the action only — matched content
// never enters any field (credential red line).
func (p *Proxy) auditGuardHit(cfg *Config, kind string, names []string, action, requestID, agent, proto, exposed string) {
	if p.secLog == nil || !cfg.Guard.AuditEnabled() {
		return
	}
	p.secLog.Enqueue(&seclog.Record{
		Kind:      kind,
		Ts:        time.Now().UnixMilli(),
		RequestID: requestID,
		Agent:     agent,
		Protocol:  proto,
		Exposed:   exposed,
		Names:     names,
		Action:    action,
	})
}
