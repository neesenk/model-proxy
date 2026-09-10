package app

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCacheStateRoundTrip: counters saved by one proxy seed a fresh proxy's
// store — restarts (and reloads, which rebuild the store) continue the
// cumulative hit/miss history, including the per-model breakdown. Entries are
// NOT persisted (cached bodies die with the process).
func TestCacheStateRoundTrip(t *testing.T) {
	home := t.TempDir()
	qpath := filepath.Join(home, ".model-proxy", "quota_state.json")
	t.Setenv("HOME", home)

	cfg, err := LoadConfigFromBytes("test", []byte(`listen: 127.0.0.1:0
providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://x}
cache: {enabled: true, ttl: 1h}
`))
	if err != nil {
		t.Fatal(err)
	}
	first := NewProxyWithStatePath(cfg, qpath)
	if first.cache == nil {
		t.Fatal("cache not created despite cache.enabled")
	}
	now := time.Now()
	first.cache.Put("k", "glm-5.2", http.StatusOK, nil, []byte("x"), now)
	if _, ok := first.cache.Lookup("k", "glm-5.2", now.Add(time.Second)); !ok {
		t.Fatal("seeded entry should hit")
	}
	first.cache.Lookup("miss", "kimi-k3", now.Add(time.Second))
	first.saveCacheState()

	// The state file carries the counters without the entry bodies.
	raw, err := os.ReadFile(CacheStatePath(qpath))
	if err != nil {
		t.Fatalf("state file not written: %v", err)
	}
	var onDisk struct {
		Version int    `json:"version"`
		Hits    uint64 `json:"hits"`
		Misses  uint64 `json:"misses"`
	}
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("state file malformed: %v", err)
	}
	if onDisk.Version != 1 || onDisk.Hits != 1 || onDisk.Misses != 1 {
		t.Errorf("state file = %s, want version 1 with hits=1 misses=1", raw)
	}

	// A "restarted" proxy seeds from the file: totals and per-model survive,
	// the entry does not.
	second := NewProxyWithStatePath(cfg, qpath)
	stats := second.cache.Stats()
	if stats.Hits != 1 || stats.Misses != 1 {
		t.Errorf("restarted counters = hits %d misses %d, want 1/1", stats.Hits, stats.Misses)
	}
	if stats.Entries != 0 {
		t.Errorf("restarted entries = %d, want 0 (bodies are not persisted)", stats.Entries)
	}
	var glm, kimi *struct {
		hits, misses uint64
	}
	for _, m := range stats.Models {
		switch m.Name {
		case "glm-5.2":
			glm = &struct{ hits, misses uint64 }{m.Hits, m.Misses}
		case "kimi-k3":
			kimi = &struct{ hits, misses uint64 }{m.Hits, m.Misses}
		}
	}
	if glm == nil || glm.hits != 1 || glm.misses != 0 {
		t.Errorf("glm-5.2 breakdown = %+v, want 1 hit 0 misses", stats.Models)
	}
	if kimi == nil || kimi.hits != 0 || kimi.misses != 1 {
		t.Errorf("kimi-k3 breakdown = %+v, want 0 hits 1 miss", stats.Models)
	}

	// Post-restart traffic accumulates on top of the seeded counters.
	second.cache.Lookup("miss2", "kimi-k3", now)
	if got := second.cache.Stats().Misses; got != 2 {
		t.Errorf("misses after post-restart traffic = %d, want seeded 1 + 1", got)
	}
}

// TestCacheStateResetPersistsZero: reset-stats clears the in-memory counters
// AND immediately persists the zero state, so the next minute tick (or a
// crash) cannot resurrect a history the user just reset.
func TestCacheStateResetPersistsZero(t *testing.T) {
	home := t.TempDir()
	qpath := filepath.Join(home, ".model-proxy", "quota_state.json")
	t.Setenv("HOME", home)

	cfg, err := LoadConfigFromBytes("test", []byte(`listen: 127.0.0.1:0
providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://x}
cache: {enabled: true, ttl: 1h}
`))
	if err != nil {
		t.Fatal(err)
	}
	p := NewProxyWithStatePath(cfg, qpath)
	now := time.Now()
	p.cache.Put("k", "glm-5.2", http.StatusOK, nil, []byte("x"), now)
	p.cache.Lookup("k", "glm-5.2", now)
	if err := p.resetStats(); err != nil {
		t.Fatalf("resetStats: %v", err)
	}
	if got := p.cache.Stats(); got.Hits != 0 || got.Entries != 0 {
		t.Fatalf("stats after reset = %+v, want zero", got)
	}

	// A restarted proxy must see the reset, not the pre-reset history.
	fresh := NewProxyWithStatePath(cfg, qpath)
	if got := fresh.cache.Stats(); got.Hits != 0 || got.Misses != 0 || len(got.Models) != 0 {
		t.Errorf("stats after reset+restart = %+v, want zero counters and empty breakdown", got)
	}
}

// TestLoadCacheStateRejectsGarbage: a corrupt file or a future version must
// yield a zero state (fresh start), never a partial parse or an error path
// that could block startup.
func TestLoadCacheStateRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	corrupt := filepath.Join(dir, "cache_state.json")
	if err := os.WriteFile(corrupt, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := loadCacheState(corrupt); got.Hits != 0 || got.Misses != 0 || len(got.Models) != 0 {
		t.Errorf("corrupt file parsed as %+v, want zero state", got)
	}
	if err := os.WriteFile(corrupt, []byte(`{"version":99,"hits":100}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := loadCacheState(corrupt); got.Hits != 0 {
		t.Errorf("future version parsed as %+v, want zero state", got)
	}
	if got := loadCacheState(filepath.Join(dir, "missing.json")); got.Hits != 0 || got.Misses != 0 {
		t.Errorf("missing file parsed as %+v, want zero state", got)
	}
}
