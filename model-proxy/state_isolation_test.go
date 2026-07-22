package main

import (
	"encoding/json"
	"os"
	"sync"
	"testing"

	"model-proxy/provider"
)

// TestMain redirects HOME to a throwaway dir for the whole package so no test
// ever reads or writes the real ~/.model-proxy state (quota_state.json, pool
// files, models cache, stats.db). Tests needing their own isolated dir still
// call t.Setenv("HOME", t.TempDir()). This is the package-level enforcement of
// the repo rule (model-proxy/AGENTS.md: tests must not touch the real HOME; the
// rule was on the books but unenforced — most NewProxy tests wrote the real
// state file).
//
// SKIPPED for the TestHelperProcess subprocess (runCLIWithHome re-invokes this
// binary with MP_CLI_HELPER=1 and pins its own HOME via cmd.Env); redirecting
// here would clobber the per-test HOME the parent chose and the CLI could no
// longer find the pool files the test wrote.
func TestMain(m *testing.M) {
	if os.Getenv("MP_CLI_HELPER") == "" {
		dir, err := os.MkdirTemp("", "model-proxy-test-home")
		if err != nil {
			panic(err)
		}
		defer os.RemoveAll(dir)
		os.Setenv("HOME", dir)
	}
	os.Exit(m.Run())
}

// TestQuotaPersist_ConcurrentTrackersNoRenameRace (bug 3): two+ trackers
// sharing one state file — as parallel NewProxy tests under one binary create —
// must not lose a rename to ENOENT. The fixed ".tmp" name guarded only by a
// per-instance mutex made the loser's rename fail (the visible symptom: the
// /api/health/reset handler returned 500 "persist failed: rename ...: no such
// file or directory"); unique same-dir temp files remove the contention and keep
// the final file valid (atomic rename → last writer wins).
func TestQuotaPersist_ConcurrentTrackersNoRenameRace(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/quota_state.json"
	cfg := func() *Config { return &Config{} }
	provs := func() map[string]provider.Provider { return nil }
	var wg sync.WaitGroup
	const trackers, persists = 3, 30
	for i := 0; i < trackers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tr := newQuotaTracker(path, cfg, provs)
			for j := 0; j < persists; j++ {
				if err := tr.persist(); err != nil {
					t.Errorf("persist failed: %v", err)
				}
			}
		}()
	}
	wg.Wait()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("state file missing after persists: %v", err)
	}
	if !json.Valid(data) {
		t.Errorf("state file not valid JSON after concurrent persists: %s", data)
	}
}

// TestProxy_CloseStopsTracker (bug 3): Proxy.Close stops the quota tracker and
// drains its goroutine, so a test proxy doesn't leak a poller that fires a
// persist after the test (and its config generation) is gone. Close is
// idempotent — a second call must not deadlock.
func TestProxy_CloseStopsTracker(t *testing.T) {
	cfg, _ := LoadConfigFromBytes("test", []byte(`listen: 127.0.0.1:0
providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://x}
`))
	p := NewProxy(cfg)
	select {
	case <-p.quota.stopCh:
		t.Fatal("stopCh should be open before Close")
	default:
	}
	p.Close()
	select {
	case <-p.quota.stopCh:
	default:
		t.Fatal("stopCh should be closed after Close")
	}
	p.Close() // idempotent: must not deadlock
}
