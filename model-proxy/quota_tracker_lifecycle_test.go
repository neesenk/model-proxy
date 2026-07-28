package main

import (
	"sync/atomic"
	"testing"
	"time"

	"model-proxy/provider"
)

// --- quotaTracker.stop / pollAfter ---

func TestQuotaTracker_Stop(t *testing.T) {
	dir := t.TempDir()
	cfg := func() *Config { return &Config{} }
	provs := func() map[string]provider.Provider { return nil }
	tr := newQuotaTracker(dir+"/q.json", cfg, provs)
	tr.start()
	// stop must be idempotent and not block.
	tr.stop()
	tr.stop() // second stop is a no-op (sync.Once)
}

func TestQuotaTracker_PollAfter(t *testing.T) {
	dir := t.TempDir()
	cfg := func() *Config { return &Config{} }
	var called atomic.Int32
	provs := func() map[string]provider.Provider {
		called.Add(1)
		return nil
	}
	tr := newQuotaTracker(dir+"/q.json", cfg, provs)
	tr.start()
	defer tr.stop()
	// pollAfter → pollAll → provs(). Measure the delta: start()'s bootstrap poll
	// (10s) and the ticker (5m default) can't fire within this window, so any
	// provs() call after dispatch must come from the pollAfter path.
	before := called.Load()
	tr.pollAfter(50 * time.Millisecond)
	time.Sleep(300 * time.Millisecond) // let the pollAfter-driven poll fire
	if got := called.Load() - before; got < 1 {
		t.Errorf("pollAfter did not trigger a poll: provs called %d more times", got)
	}
}
