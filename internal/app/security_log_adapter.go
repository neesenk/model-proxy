package app

import (
	"log"
	"path/filepath"
	"time"

	"model-proxy/internal/accounts"
	"model-proxy/internal/observe/seclog"
)

// reconcileSecLog makes the security audit logger reload-owned: it brings
// p.secLog in line with the given config generation — audit on → a logger for
// the generation's audit_path, audit off → nil, audit_path changed → a new
// logger for the new directory. Startup (StartRuntimeServices) and every
// successful Reload both funnel through here.
//
// Ordering: building the logger and admitting its Run goroutine happen
// OUTSIDE p.mu (mkdir/file I/O runs inside Run; the lock only swaps pointers).
// The old logger is Shutdown (drained) after the swap, outside the lock. An
// in-flight request holding the previous snapshot may still Enqueue into the
// old logger after its Shutdown — Enqueue is a non-blocking offer on a channel
// nobody drains anymore, so those last few records are silently dropped
// (accepted semantics: the audit log is best-effort, never request-blocking).
//
// Concurrency: the write-lock re-check (p.secLog still == the logger we
// decided against, lifecycle not stopping) keeps two racing reloads from
// double-swapping, and keeps a reload that admitted a goroutine just before
// Close's BeginStop from installing a logger Close would never drain (the
// aborted logger is shut down here instead).
func (p *Proxy) reconcileSecLog(cfg *Config) {
	desiredDir := ""
	if cfg.Guard.AuditEnabled() {
		desiredDir = filepath.Dir(cfg.Guard.AuditPathValue(accounts.HomeDir()))
	}

	p.mu.RLock()
	current := p.secLog
	p.mu.RUnlock()
	if current == nil && desiredDir == "" {
		return
	}
	if current != nil && desiredDir != "" && current.Directory() == desiredDir {
		return
	}

	var next *seclog.Logger
	if desiredDir != "" {
		var err error
		next, err = seclog.New(desiredDir, seclog.Options{})
		if err != nil {
			// seclog.New only fails on an empty directory (unreachable via
			// AuditPathValue, defensive). Keep the previous logger state.
			log.Printf("[seclog] init failed: %v — security audit logger unchanged", err)
			return
		}
		if !p.lifecycle.Run(func(<-chan struct{}) { next.Run() }) {
			// The lifecycle is stopping: nothing may outlive Close.
			log.Printf("[seclog] lifecycle stopping — security audit logger not swapped")
			return
		}
	}

	p.mu.Lock()
	stopping := false
	select {
	case <-p.lifecycle.StopChannel():
		stopping = true
	default:
	}
	if stopping || p.secLog != current {
		p.mu.Unlock()
		if next != nil {
			// Aborted after admission: drain and stop the logger we started so
			// its goroutine cannot outlive the lifecycle wait.
			next.Shutdown()
		}
		return
	}
	p.secLog = next
	p.secLogRunning = next != nil
	p.mu.Unlock()

	if current != nil {
		current.Shutdown()
	}
}

// auditGuardHit enqueues one security audit record for a guard hit on the
// request's snapshot logger (nil when the request's generation has audit off).
// The record carries pattern/path NAMES and the action only — matched content
// never enters any field (credential red line).
func auditGuardHit(logger *seclog.Logger, kind string, names []string, action, requestID, agent, proto, exposed string) {
	if logger == nil {
		return
	}
	logger.Enqueue(&seclog.Record{
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
