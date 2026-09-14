// cache_state.go — persistence for the exact-response cache counters. The
// cache leaf (internal/cache) stays pure in-memory; this file owns the state
// file: ~/.model-proxy/cache_state.json, loaded once at startup to seed the
// process-lifetime counters shared by all stores, saved on a per-minute
// lifecycle loop, once more on shutdown, and cleared by reset-stats.
package app

import (
	"encoding/json"
	"model-proxy/internal/observe/logx"
	"os"
	"path/filepath"
	"time"
)

const cacheStateFile = "cache_state.json"

// CacheStatePath derives the cache state file from the quota state path (the
// same <home>/.model-proxy directory convention as responses_state.json and
// model_caps.json).
func CacheStatePath(quotaStatePath string) string {
	return filepath.Join(filepath.Dir(quotaStatePath), cacheStateFile)
}

// persistedCacheModel is one model's persisted counters. Entries are NOT
// persisted: cached bodies live only in process memory and do not survive a
// restart, so the seeded store starts with an empty map.
type persistedCacheModel struct {
	Model  string `json:"model"`
	Hits   uint64 `json:"hits"`
	Misses uint64 `json:"misses"`
}

// persistedCacheState is the on-disk shape. version guards against parsing a
// future-incompatible file as zeros.
type persistedCacheState struct {
	Version int                   `json:"version"`
	Hits    uint64                `json:"hits"`
	Misses  uint64                `json:"misses"`
	Models  []persistedCacheModel `json:"models,omitempty"`
}

const cacheStateVersion = 1

// loadCacheState reads the state file best-effort: any error (missing file,
// corrupt JSON, future version) yields a zero state and the store simply
// starts fresh — observability counters must never block startup.
func loadCacheState(path string) persistedCacheState {
	var state persistedCacheState
	data, err := os.ReadFile(path)
	if err != nil {
		return state
	}
	if err := json.Unmarshal(data, &state); err != nil {
		logx.Warnf("[cache_state] parse %s: %v — starting with zero counters", path, err)
		return persistedCacheState{}
	}
	if state.Version != cacheStateVersion {
		logx.Warnf("[cache_state] %s has version %d, want %d — starting with zero counters", path, state.Version, cacheStateVersion)
		return persistedCacheState{}
	}
	return state
}

// saveCacheState persists the process's current counters atomically
// (per-write temp file + fsync + rename, same pattern as the quota state).
// Best-effort: a failed save only logs — the next tick retries.
func (p *Proxy) saveCacheState() {
	p.cachePersistMu.Lock()
	defer p.cachePersistMu.Unlock()
	if p.cacheCounters == nil {
		return
	}
	stats := p.cacheCounters.Stats()
	state := persistedCacheState{
		Version: cacheStateVersion,
		Hits:    stats.Hits,
		Misses:  stats.Misses,
	}
	for _, m := range stats.Models {
		state.Models = append(state.Models, persistedCacheModel{Model: m.Name, Hits: m.Hits, Misses: m.Misses})
	}
	if err := writeCacheState(p.cacheStatePath, state); err != nil {
		logx.Warnf("[cache_state] persist: %v", err)
	}
}

// writeCacheState is called only under cachePersistMu (snapshot through rename).
func writeCacheState(path string, state persistedCacheState) error {
	if path == "" {
		return nil
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// Unique same-directory temp files avoid conflicts with other processes;
	// cachePersistMu separately prevents stale writes within this process.
	f, err := os.CreateTemp(dir, ".cache_state-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	remove := func() { os.Remove(tmp) }
	if _, err := f.Write(data); err != nil {
		f.Close()
		remove()
		return err
	}
	// fsync before rename: a crash+reboot must not leave the rename durable
	// while the data isn't.
	if err := f.Sync(); err != nil {
		f.Close()
		remove()
		return err
	}
	if err := f.Close(); err != nil {
		remove()
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		remove()
		return err
	}
	return nil
}

// cacheSaveLoop persists the counters once per minute until stop closes. Owned
// by the lifecycle (same registration pattern as statsFlushLoop); the final
// save happens in closeRuntimeServices after the wait.
func (p *Proxy) cacheSaveLoop(stop <-chan struct{}) {
	for {
		timer := time.NewTimer(time.Minute)
		select {
		case <-timer.C:
			p.saveCacheState()
		case <-stop:
			timer.Stop()
			return
		}
	}
}
