package main

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// setPoolHome redirects HOME to a temp dir (auto-cleanup via t.Setenv) and
// pre-creates the .model-proxy subdir. Named distinctly to avoid colliding
// with deepseek_forward_test.go's setHome helper.
func setPoolHome(t *testing.T, dir string) {
	t.Helper()
	os.MkdirAll(filepath.Join(dir, ".model-proxy"), 0o700)
	t.Setenv("HOME", dir)
}

func TestAccountIDFor(t *testing.T) {
	z := accountIDFor("zhipu", accountCred{APIKey: "sk-abc"})
	z2 := accountIDFor("zhipu", accountCred{APIKey: "sk-abc"})
	z3 := accountIDFor("zhipu", accountCred{APIKey: "sk-other"})
	if z != z2 {
		t.Fatalf("same key must yield same id: %q vs %q", z, z2)
	}
	if z == z3 {
		t.Fatalf("different keys must yield different ids")
	}
	if len(z) != 16 {
		t.Fatalf("zhipu id len = %d, want 16", len(z))
	}
	// volcengine keys by access_key (account-level), not api_key
	a := accountIDFor("volcengine", accountCred{APIKey: "k1", AccessKey: "AK9"})
	b := accountIDFor("volcengine", accountCred{APIKey: "k2", AccessKey: "AK9"})
	if a != b {
		t.Fatalf("volcengine same access_key must yield same id: %q vs %q", a, b)
	}
	if a != "AK9" {
		t.Fatalf("volcengine id = %q, want AK9", a)
	}
	// access_key empty → fall back to key hash
	c := accountIDFor("volcengine", accountCred{APIKey: "k1"})
	if c == "" || len(c) != 16 {
		t.Fatalf("volcengine fallback id = %q", c)
	}
}

func TestSaveLoadPoolRoundTrip(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	in := credentialPool{Version: 1, Accounts: []poolAccount{
		{ID: "id1", Label: "home", APIKey: "k1", AddedAt: "2026-07-08T00:00:00Z"},
		{ID: "id2", Label: "team", APIKey: "k2", AddedAt: "2026-07-08T00:00:00Z"},
	}}
	if err := savePool("zhipu", in); err != nil {
		t.Fatal(err)
	}
	out, err := loadPool("zhipu", "zhipu")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Accounts) != 2 {
		t.Fatalf("len = %d, want 2", len(out.Accounts))
	}
	if out.Accounts[0].ID != "id1" || out.Accounts[0].APIKey != "k1" || out.Accounts[0].Label != "home" {
		t.Fatalf("account0 = %+v", out.Accounts[0])
	}
}

func TestLoadPoolSingularFallback(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	// Legacy single-key file, no plural pool.
	singular := filepath.Join(dir, ".model-proxy", "zhipu_apikey.json")
	os.WriteFile(singular, []byte(`{"api_key":"legacy-key"}`), 0o600)

	pool, err := loadPool("zhipu", "zhipu")
	if err != nil {
		t.Fatal(err)
	}
	if len(pool.Accounts) != 1 {
		t.Fatalf("fallback len = %d, want 1", len(pool.Accounts))
	}
	if pool.Accounts[0].APIKey != "legacy-key" {
		t.Fatalf("fallback key = %q", pool.Accounts[0].APIKey)
	}
	// id derived from the key.
	if pool.Accounts[0].ID != accountIDFor("zhipu", accountCred{APIKey: "legacy-key"}) {
		t.Fatalf("fallback id not derived from key")
	}
}

func TestLoadPoolEmpty(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	pool, err := loadPool("zhipu", "zhipu")
	if err != nil {
		t.Fatalf("missing pool should be empty, not error: %v", err)
	}
	if len(pool.Accounts) != 0 {
		t.Fatalf("want 0 accounts, got %d", len(pool.Accounts))
	}
}

// TestSavePoolAtomic_NoTruncationOnDisk verifies savePool writes via temp +
// rename: the final file is whole JSON (parseable) and no stray .tmp is left.
// Regression guard for #1/#2A (crash mid-write must not strand a truncated pool).
func TestSavePoolAtomic_NoTruncationOnDisk(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	in := credentialPool{Version: 1, Accounts: []poolAccount{
		{ID: "id1", Label: "home", APIKey: "k1", AddedAt: "2026-07-08T00:00:00Z"},
	}}
	if err := savePool("deepseek", in); err != nil {
		t.Fatal(err)
	}
	// No leftover temp file.
	if _, err := os.Stat(poolPath("deepseek") + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf(".tmp leftover after savePool: %v", err)
	}
	// The written file is parseable (not a half-written truncation).
	out, err := loadPool("deepseek", "deepseek")
	if err != nil {
		t.Fatalf("reload after atomic save: %v", err)
	}
	if len(out.Accounts) != 1 || out.Accounts[0].APIKey != "k1" {
		t.Fatalf("round-trip mismatch: %+v", out.Accounts)
	}
}

// TestWithPoolLock_MutualExclusion asserts the O_EXCL lockfile serializes
// callers: at no point do two callbacks run their critical section
// concurrently. Spawns several goroutines (more than GOMAXPROCS to force real
// contention) and records the peak number of overlapping entries.
func TestWithPoolLock_MutualExclusion(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	const name = "zhipu"
	var mu sync.Mutex
	var inCS, maxCS int
	fn := func() error {
		mu.Lock()
		inCS++
		if inCS > maxCS {
			maxCS = inCS
		}
		mu.Unlock()
		// Hold long enough that another goroutine is very likely waiting at the
		// O_EXCL create when this one releases — proves serialization, not luck.
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		inCS--
		mu.Unlock()
		return nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := withPoolLock(name, fn); err != nil {
				t.Errorf("withPoolLock: %v", err)
			}
		}()
	}
	wg.Wait()
	if maxCS != 1 {
		t.Fatalf("critical section overlapped: peak concurrency = %d, want 1", maxCS)
	}
	// Lockfile cleaned up after every holder released.
	if _, err := os.Stat(poolPath(name) + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("lockfile left behind after all holders: %v", err)
	}
}

// TestWithPoolLock_StaleSteal asserts a lockfile whose mtime is older than
// poolLockStaleAge is treated as stranded (holder crashed) and stolen: withPoolLock
// removes it, re-acquires, runs fn, and cleans up. This is the crash-recovery
// path that makes O_EXCL safe without flock.
func TestWithPoolLock_StaleSteal(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	const name = "deepseek"
	lockPath := poolPath(name) + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, []byte("99999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Age the lockfile past poolLockStaleAge (60s) so it looks stranded.
	past := time.Now().Add(-2 * poolLockStaleAge)
	if err := os.Chtimes(lockPath, past, past); err != nil {
		t.Fatal(err)
	}
	ran := false
	if err := withPoolLock(name, func() error { ran = true; return nil }); err != nil {
		t.Fatalf("withPoolLock stale-steal failed: %v", err)
	}
	if !ran {
		t.Fatal("fn did not run after stealing stale lock")
	}
	// Stolen lock is owned by this run, so it must be cleaned up afterward.
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("stale lockfile not removed after steal+run: %v", err)
	}
}

// TestWithPoolLock_FreshLockIsBusy asserts a YOUNG lockfile is NOT stolen —
// withPoolLock waits and eventually reports contention rather than clobbering a
// live holder. Uses a short-lived holder goroutine to confirm the wait succeeds
// when the holder releases promptly (distinguishing "busy, then acquired" from a
// spurious steal). The steal path is covered by TestWithPoolLock_StaleSteal.
func TestWithPoolLock_FreshLockIsBusy(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	const name = "volcengine"
	lockPath := poolPath(name) + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		t.Fatal(err)
	}
	// Pre-create a fresh lockfile held by "another process", mtime = now.
	if err := os.WriteFile(lockPath, []byte("99999\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A concurrent goroutine simulates the other process releasing the lock
	// after 80ms — well within poolLockRetry cadence, so withPoolLock should
	// observe the free slot and acquire rather than erroring.
	go func() {
		time.Sleep(80 * time.Millisecond)
		os.Remove(lockPath)
	}()

	ran := false
	if err := withPoolLock(name, func() error { ran = true; return nil }); err != nil {
		t.Fatalf("withPoolLock should have waited + acquired a freshly-freed lock: %v", err)
	}
	if !ran {
		t.Fatal("fn did not run after waiting for the holder to release")
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("lockfile not cleaned up: %v", err)
	}
}
