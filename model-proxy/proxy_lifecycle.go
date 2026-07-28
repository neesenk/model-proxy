package main

import (
	"log"
	"sync"
	"time"
)

// proxyLifecycle is the single owner for Proxy-level background work. The
// mutex makes admission (WaitGroup.Add) atomic with shutdown, avoiding Add/Wait
// races while reload is scheduling a best-effort refresh.
type proxyLifecycle struct {
	mu       sync.Mutex
	stopping bool
	stop     chan struct{}
	wg       sync.WaitGroup
	// beforeLogDrain tracks finite tasks whose final output is written through
	// requestlog.Logger (currently Shadow evaluations). Close rejects new
	// admissions, waits for this group, and only then drains the logger.
	beforeLogDrain sync.WaitGroup
}

func newProxyLifecycle() *proxyLifecycle {
	return &proxyLifecycle{stop: make(chan struct{})}
}

func (l *proxyLifecycle) run(task func(stop <-chan struct{})) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	if l.stopping {
		l.mu.Unlock()
		return false
	}
	l.wg.Add(1)
	l.mu.Unlock()
	go func() {
		defer l.wg.Done()
		task(l.stop)
	}()
	return true
}

// runBeforeLogDrain admits finite background work that may still enqueue a
// request-log record. Admission shares the lifecycle mutex with beginStop, so
// Add cannot race Wait and no task can start after shutdown begins.
func (l *proxyLifecycle) runBeforeLogDrain(task func()) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	if l.stopping {
		l.mu.Unlock()
		return false
	}
	l.beforeLogDrain.Add(1)
	l.mu.Unlock()
	go func() {
		defer l.beforeLogDrain.Done()
		task()
	}()
	return true
}

func (l *proxyLifecycle) beginStop() {
	if l == nil {
		return
	}
	l.mu.Lock()
	if !l.stopping {
		l.stopping = true
		close(l.stop)
	}
	l.mu.Unlock()
}

func (l *proxyLifecycle) wait() {
	if l != nil {
		l.wg.Wait()
	}
}

func (l *proxyLifecycle) waitBeforeLogDrain() {
	if l != nil {
		l.beforeLogDrain.Wait()
	}
}

// startRuntimeServices initializes and starts process-owned optional services.
// Direct NewProxy callers stay lightweight until this method is called.
func (p *Proxy) startRuntimeServices(cfg *Config) {
	p.initStats(cfg.Stats)
	p.lifecycle.run(func(stop <-chan struct{}) {
		p.statsFlushLoop(stop)
	})

	p.initRequestLog(cfg.RequestLog)
	if p.reqLog != nil {
		p.reqLogStarted = p.lifecycle.run(func(<-chan struct{}) {
			p.reqLog.Run()
		})
	}

	// Startup catalog loading remains synchronous so the first request gets the
	// best available routing metadata, matching the previous daemon behavior.
	p.initCatalog()
}

func (p *Proxy) refreshCatalogAsync() {
	p.lifecycle.run(func(<-chan struct{}) {
		p.initCatalog()
	})
}

// closeRuntimeServices establishes one shutdown order:
// reject new Proxy-owned tasks → wait finite log-producing work → drain request
// log → wait for periodic loops/refreshes → final stats/state flushes.
func (p *Proxy) closeRuntimeServices() {
	p.lifecycle.beginStop()
	p.lifecycle.waitBeforeLogDrain()
	if p.reqLogStarted {
		p.reqLog.Shutdown()
	}
	p.lifecycle.wait()

	if p.flusher != nil {
		p.flusher.flush(time.Now())
	}
	if p.responsesState != nil {
		p.responsesState.Close()
	}
	if p.quota != nil {
		p.quota.stop()
		if p.quota.path != "" {
			if err := p.quota.persist(); err != nil {
				log.Printf("[quota] final persist on close failed: %v", err)
			}
		}
	}
}
