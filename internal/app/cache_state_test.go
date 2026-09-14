package app

import (
	"encoding/json"
	"fmt"
	configdomain "model-proxy/internal/config"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
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

	cfg, err := configdomain.LoadConfigFromBytes("test", []byte(`listen: 127.0.0.1:0
providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://x}
cache: {enabled: true, ttl: 1h}
`))
	if err != nil {
		t.Fatal(err)
	}
	first := NewProxyWithStatePath(cfg, qpath)
	t.Cleanup(first.Close)
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
	first.Close()
	second := NewProxyWithStatePath(cfg, qpath)
	t.Cleanup(second.Close)
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

	cfg, err := configdomain.LoadConfigFromBytes("test", []byte(`listen: 127.0.0.1:0
providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://x}
cache: {enabled: true, ttl: 1h}
`))
	if err != nil {
		t.Fatal(err)
	}
	p := NewProxyWithStatePath(cfg, qpath)
	t.Cleanup(p.Close)
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
	p.Close()
	fresh := NewProxyWithStatePath(cfg, qpath)
	t.Cleanup(fresh.Close)
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

func cacheReloadFixture(t *testing.T) (*Proxy, func(bool)) {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(`{}`)) }))
	t.Cleanup(up.Close)
	t.Setenv("MP_MODELSDEV_URL", up.URL)
	path := filepath.Join(t.TempDir(), "config.yaml")
	write := func(enabled bool) {
		t.Helper()
		if err := os.WriteFile(path, []byte(fmt.Sprintf("listen: 127.0.0.1:0\nproviders:\n  up: {provider_id: zhipu, openai_base_url: %s}\ncache: {enabled: %t, ttl: 1h}\n", up.URL, enabled)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(true)
	cfg, err := configdomain.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	p := newTestProxy(t, cfg)
	return p, func(enabled bool) {
		t.Helper()
		write(enabled)
		if err := p.Reload(path); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCacheStateReloadPreservesLiveCounters(t *testing.T) {
	p, reload := cacheReloadFixture(t)
	now := time.Now()
	old := p.SnapshotRuntime().Cache
	old.Lookup("first", "m", now)
	p.saveCacheState()
	old.Lookup("second", "m", now)
	old.Put("old-entry", "m", 200, nil, []byte("cached"), now)
	reload(true)
	current := p.SnapshotRuntime().Cache
	if current == old || current.Peek("old-entry", now) {
		t.Fatal("reload reused old response entries")
	}
	if stats := current.Stats(); stats.Misses != 2 || stats.Entries != 0 {
		t.Fatalf("reload stats = %+v", stats)
	}
	// A request which captured the old generation has not reached Lookup yet.
	old.Lookup("late", "m", now)
	old.Put("late-entry", "m", 200, nil, []byte("late"), now)
	stats := current.Stats()
	if stats.Misses != 3 || len(stats.Models) != 1 || stats.Models[0].Misses != 3 || stats.Entries != 0 {
		t.Fatalf("late lookup stats = %+v", stats)
	}
	p.Close()
	fresh := newTestProxyAt(t, p.cfg, p.quota.Path)
	if got := fresh.cache.Stats(); got.Misses != 3 || got.Entries != 0 {
		t.Fatalf("restart stats = %+v", got)
	}
}

func TestCacheStateResetWhileDisabled(t *testing.T) {
	p, reload := cacheReloadFixture(t)
	p.cache.Lookup("miss", "m", time.Now())
	p.saveCacheState()
	reload(false)
	if p.cache != nil {
		t.Fatal("cache not disabled")
	}
	if err := p.resetStats(); err != nil {
		t.Fatal(err)
	}
	state := loadCacheState(p.cacheStatePath)
	if state.Misses != 0 || state.Hits != 0 || len(state.Models) != 0 {
		t.Fatalf("disk reset = %+v", state)
	}
	reload(true)
	if got := p.cache.Stats(); got.Misses != 0 || len(got.Models) != 0 {
		t.Fatalf("re-enabled cache resurrected history: %+v", got)
	}
}

func TestCacheStateResetSerializesConcurrentSaves(t *testing.T) {
	p, _ := cacheReloadFixture(t)
	p.cache.Lookup("miss", "m", time.Now())
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; p.saveCacheState() }()
	}
	close(start)
	if err := p.resetStats(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if state := loadCacheState(p.cacheStatePath); state.Misses != 0 || len(state.Models) != 0 {
		t.Fatalf("save resurrected pre-reset stats: %+v", state)
	}
}

func TestCacheStateResetWriteFailureKeepsCounters(t *testing.T) {
	p := newTestProxy(t, &configdomain.Config{Cache: configdomain.CacheConfig{Enabled: true}})
	p.cache.Lookup("miss", "m", time.Now())
	// A directory at the destination makes the atomic rename fail on all OSes.
	if err := os.Mkdir(p.cacheStatePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := p.resetStats(); err == nil {
		t.Fatal("durable reset failure acknowledged as success")
	}
	if got := p.cache.Stats().Misses; got != 1 {
		t.Fatalf("failed reset lost live misses: %d", got)
	}
	if err := os.Remove(p.cacheStatePath); err != nil {
		t.Fatal(err)
	}
	if err := p.resetStats(); err != nil {
		t.Fatal(err)
	}
	if got := p.cache.Stats().Misses; got != 0 {
		t.Fatalf("retry did not reset misses: %d", got)
	}
}
