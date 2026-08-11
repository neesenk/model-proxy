package app

import (
	"sync/atomic"
	"testing"
	"time"

	"model-proxy/internal/provider"
)

// --- runtimestate.QuotaTracker.stop / pollAfter ---

func TestQuotaTracker_Stop(t *testing.T) {
	dir := t.TempDir()
	cfg := func() *Config { return &Config{} }
	provs := func() map[string]provider.Provider { return nil }
	tr := newStandaloneQuotaTracker(dir+"/q.json", cfg, provs)
	tr.Start()
	// stop must be idempotent and not block.
	tr.Stop()
	tr.Stop() // second stop is a no-op (sync.Once)
}

func TestQuotaTracker_PollAfter(t *testing.T) {
	dir := t.TempDir()
	cfg := func() *Config { return &Config{} }
	var called atomic.Int32
	provs := func() map[string]provider.Provider {
		called.Add(1)
		return nil
	}
	tr := newStandaloneQuotaTracker(dir+"/q.json", cfg, provs)
	tr.Start()
	defer tr.Stop()
	// pollAfter → pollAll → provs(). Measure the delta: start()'s bootstrap poll
	// (10s) and the ticker (5m default) can't fire within this window, so any
	// provs() call after dispatch must come from the pollAfter path.
	before := called.Load()
	tr.PollAfter(50 * time.Millisecond)
	time.Sleep(300 * time.Millisecond) // let the pollAfter-driven poll fire
	if got := called.Load() - before; got < 1 {
		t.Errorf("pollAfter did not trigger a poll: provs called %d more times", got)
	}
}
