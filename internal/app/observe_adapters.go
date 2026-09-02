// observe_adapters.go — adapters from resolved config into the observe leaves: request-log initialization, security audit log reconciliation, and stats store bootstrap plus the per-minute flush loop.
package app

import (
	"context"
	"model-proxy/internal/accounts"
	"model-proxy/internal/observe/logx"
	"model-proxy/internal/observe/requestlog"
	"model-proxy/internal/observe/seclog"
	observestats "model-proxy/internal/observe/stats"
	"path/filepath"
	"time"
)

// initRequestLog adapts resolved configuration values into the process-owned
// logger. The goroutine itself remains owned by proxyLifecycle.
func (p *Proxy) initRequestLog(config RequestLogConfig) {
	if !config.Enabled {
		return
	}
	p.reqLog = requestlog.New(requestlog.Options{
		Directory:    config.ResolvedDir(),
		MaxFileSize:  config.MaxFileSizeBytes(),
		MaxBodyBytes: config.MaxBodyBytesValue(),
		Retention:    config.RetentionDuration(),
	})
	logx.Infof(
		"[request_log] enabled -> %s (max_file_size %d bytes, max_body %d bytes, retention %s)",
		config.ResolvedDir(),
		config.MaxFileSizeBytes(),
		config.MaxBodyBytesValue(),
		config.RetentionDuration(),
	)
}

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
			logx.Warnf("[seclog] init failed: %v — security audit logger unchanged", err)
			return
		}
		if !p.lifecycle.Run(func(<-chan struct{}) { next.Run() }) {
			// The lifecycle is stopping: nothing may outlive Close.
			logx.Warnf("[seclog] lifecycle stopping — security audit logger not swapped")
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

// The guard-hit audit enqueue (auditGuardHit) lives in internal/forward
// (AuditGuardHit) next to the live guard pass that calls it.

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
		accounts.HomeDir(),
		p.metrics,
		p.tokens,
		p.agents,
	)
	p.stats = result.Store
	p.flusher = result.Flusher
}
