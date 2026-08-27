package accounts

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestStore(t *testing.T, dir string) Store {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".model-proxy"), 0o700); err != nil {
		t.Fatal(err)
	}
	return NewStore(dir)
}

func TestAccountIDFor(t *testing.T) {
	z := AccountID("zhipu", Credentials{APIKey: "sk-abc"})
	z2 := AccountID("zhipu", Credentials{APIKey: "sk-abc"})
	z3 := AccountID("zhipu", Credentials{APIKey: "sk-other"})
	if z != z2 {
		t.Fatalf("same key must yield same id: %q vs %q", z, z2)
	}
	if z == z3 {
		t.Fatalf("different keys must yield different ids")
	}
	if len(z) != 16 {
		t.Fatalf("zhipu id len = %d, want 16", len(z))
	}
	// volcengine keys by access_key (account-level), not api_key. The id is a
	// hash of it — virtual ids reach logs and persisted state, so the raw
	// access key must never appear in the identifier (AGENTS.md red line 3).
	a := AccountID("volcengine", Credentials{APIKey: "k1", AccessKey: "AK9"})
	b := AccountID("volcengine", Credentials{APIKey: "k2", AccessKey: "AK9"})
	if a != b {
		t.Fatalf("volcengine same access_key must yield same id: %q vs %q", a, b)
	}
	if strings.Contains(a, "AK9") {
		t.Fatalf("volcengine id %q must not contain the raw access key", a)
	}
	if len(a) != 16 {
		t.Fatalf("volcengine id len = %d, want 16", len(a))
	}
	d := AccountID("volcengine", Credentials{APIKey: "k1", AccessKey: "AK8"})
	if a == d {
		t.Fatalf("different access keys must yield different ids")
	}
	// access_key empty → fall back to key hash
	c := AccountID("volcengine", Credentials{APIKey: "k1"})
	if c == "" || len(c) != 16 {
		t.Fatalf("volcengine fallback id = %q", c)
	}
}

func TestAccountCredentialsAndTimestamp(t *testing.T) {
	account := Account{
		APIKey: "api-key", AccessKey: "access-key", SecretKey: "secret-key",
	}
	if got := account.Credentials(); got != (Credentials{
		APIKey: "api-key", AccessKey: "access-key", SecretKey: "secret-key",
	}) {
		t.Fatalf("Credentials() = %+v", got)
	}

	when := time.Date(2026, time.July, 28, 9, 10, 11, 0, time.FixedZone("UTC+8", 8*60*60))
	if got, want := Timestamp(when), "2026-07-28T01:10:11Z"; got != want {
		t.Fatalf("Timestamp() = %q, want %q", got, want)
	}
}

func TestSaveLoadPoolRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := newTestStore(t, dir)
	in := Pool{Version: 1, Accounts: []Account{
		{ID: "id1", Label: "home", APIKey: "k1", AddedAt: "2026-07-08T00:00:00Z"},
		{ID: "id2", Label: "team", APIKey: "k2", AddedAt: "2026-07-08T00:00:00Z"},
	}}
	if err := store.Save("zhipu", "zhipu", in); err != nil {
		t.Fatal(err)
	}
	out, err := store.Load("zhipu", "zhipu")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Accounts) != 2 {
		t.Fatalf("len = %d, want 2", len(out.Accounts))
	}
	// IDs are DERIVED from credentials on load, never free-form strings: the
	// hand-assigned "id1" converges to the canonical hash (login always wrote
	// AccountID; hand-written ids were never a supported path, and normalizing
	// them is what migrates pre-hash volcengine pools).
	if want := AccountID("zhipu", in.Accounts[0].Credentials()); out.Accounts[0].ID != want {
		t.Fatalf("account0 id = %q, want derived %q", out.Accounts[0].ID, want)
	}
	if out.Accounts[0].APIKey != "k1" || out.Accounts[0].Label != "home" || out.Accounts[0].AddedAt != "2026-07-08T00:00:00Z" {
		t.Fatalf("account0 = %+v", out.Accounts[0])
	}
}

func TestSaveRejectsInvalidPoolWithoutReplacingExistingFile(t *testing.T) {
	dir := t.TempDir()
	store := newTestStore(t, dir)
	valid := Pool{Accounts: []Account{{ID: "existing", APIKey: "existing-key"}}}
	if err := store.Save("volcengine", "volcengine", valid); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(store.PoolPath("volcengine"))
	if err != nil {
		t.Fatal(err)
	}

	invalid := Pool{Accounts: []Account{{
		ID: "replacement", APIKey: "replacement-key", AccessKey: "access-without-secret",
	}}}
	err = store.Save("volcengine", "volcengine", invalid)
	if err == nil || !strings.Contains(err.Error(), "must set both access_key and secret_key") {
		t.Fatalf("Save() error = %v, want semantic validation failure", err)
	}
	after, readErr := os.ReadFile(store.PoolPath("volcengine"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(after) != string(before) {
		t.Fatal("failed Save replaced the previously valid pool")
	}
}

func TestLoadPoolSingularFallback(t *testing.T) {
	dir := t.TempDir()
	store := newTestStore(t, dir)
	// Legacy single-key file, no plural pool.
	singular := filepath.Join(dir, ".model-proxy", "zhipu_apikey.json")
	os.WriteFile(singular, []byte(`{"api_key":"legacy-key"}`), 0o600)

	snapshot, err := store.LoadSnapshot("zhipu", "zhipu")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Source != SourceLegacy {
		t.Fatalf("source = %v, want SourceLegacy", snapshot.Source)
	}
	pool := snapshot.Pool
	if len(pool.Accounts) != 1 {
		t.Fatalf("fallback len = %d, want 1", len(pool.Accounts))
	}
	if pool.Accounts[0].APIKey != "legacy-key" {
		t.Fatalf("fallback key = %q", pool.Accounts[0].APIKey)
	}
	// id derived from the key.
	if pool.Accounts[0].ID != AccountID("zhipu", Credentials{APIKey: "legacy-key"}) {
		t.Fatalf("fallback id not derived from key")
	}
}

func TestLoadPoolPluralPrecedenceAndParseErrors(t *testing.T) {
	t.Run("valid plural wins over valid legacy", func(t *testing.T) {
		dir := t.TempDir()
		store := newTestStore(t, dir)
		if err := os.WriteFile(store.LegacyPath("zhipu"), []byte(`{"api_key":"legacy"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := store.Save("zhipu", "zhipu", Pool{Accounts: []Account{{
			ID: "plural-id", APIKey: "plural",
		}}}); err != nil {
			t.Fatal(err)
		}

		snapshot, err := store.LoadSnapshot("zhipu", "zhipu")
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.Source != SourcePlural {
			t.Fatalf("source = %v, want SourcePlural", snapshot.Source)
		}
		pool := snapshot.Pool
		if len(pool.Accounts) != 1 || pool.Accounts[0].APIKey != "plural" {
			t.Fatalf("Load() = %+v, want only plural credential", pool.Accounts)
		}
	})

	t.Run("corrupt plural never falls back to legacy", func(t *testing.T) {
		dir := t.TempDir()
		store := newTestStore(t, dir)
		if err := os.WriteFile(store.LegacyPath("zhipu"), []byte(`{"api_key":"legacy"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(store.PoolPath("zhipu"), []byte(`{`), 0o600); err != nil {
			t.Fatal(err)
		}

		snapshot, err := store.LoadSnapshot("zhipu", "zhipu")
		if err == nil {
			t.Fatal("corrupt plural pool should fail")
		}
		if snapshot.Source != SourcePlural {
			t.Fatalf("source = %v, want SourcePlural", snapshot.Source)
		}
		if len(snapshot.Pool.Accounts) != 0 {
			t.Fatalf("corrupt plural pool returned legacy credentials: %+v", snapshot.Pool.Accounts)
		}
	})

	t.Run("corrupt legacy reports its own path", func(t *testing.T) {
		dir := t.TempDir()
		store := newTestStore(t, dir)
		if err := os.WriteFile(store.LegacyPath("deepseek"), []byte(`{`), 0o600); err != nil {
			t.Fatal(err)
		}

		snapshot, err := store.LoadSnapshot("deepseek", "deepseek")
		if err == nil {
			t.Fatal("corrupt legacy pool should fail")
		} else if got := err.Error(); got == "" || !strings.Contains(got, store.LegacyPath("deepseek")) {
			t.Fatalf("Load() error = %q, want legacy path %q", got, store.LegacyPath("deepseek"))
		}
		if snapshot.Source != SourceLegacy {
			t.Fatalf("source = %v, want SourceLegacy", snapshot.Source)
		}
	})
}

func TestLoadSnapshotRejectsInvalidAccounts(t *testing.T) {
	tests := []struct {
		name       string
		providerID string
		body       string
		want       string
	}{
		{
			name: "empty id",
			body: `{"version":1,"accounts":[{"id":"","api_key":"key"}]}`,
			want: "id is empty",
		},
		{
			name: "reserved id separator",
			body: `{"version":1,"accounts":[{"id":"one#two","api_key":"key"}]}`,
			want: "reserved '#'",
		},
		{
			// Mixed plaintext/metadata entries are NOT a keychain→file restore
			// candidate (a fully metadata-only pool is) — validatePool still
			// rejects the empty secret.
			name: "empty api key",
			body: `{"version":1,"accounts":[{"id":"one","api_key":"key"},{"id":"two","api_key":""}]}`,
			want: "api_key is empty",
		},
		{
			name: "duplicate id",
			body: `{"version":1,"accounts":[{"id":"one","api_key":"key-1"},{"id":"one","api_key":"key-2"}]}`,
			want: "duplicates an earlier account",
		},
		{
			name:       "partial volcengine signing pair",
			providerID: "volcengine",
			body:       `{"version":1,"accounts":[{"id":"one","api_key":"key","access_key":"ak"}]}`,
			want:       "must set both access_key and secret_key",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			store := newTestStore(t, dir)
			if err := os.WriteFile(store.PoolPath("provider"), []byte(test.body), 0o600); err != nil {
				t.Fatal(err)
			}
			providerID := test.providerID
			if providerID == "" {
				providerID = "zhipu"
			}

			snapshot, err := store.LoadSnapshot("provider", providerID)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("LoadSnapshot() error = %v, want %q", err, test.want)
			}
			if snapshot.Source != SourcePlural || len(snapshot.Pool.Accounts) != 0 {
				t.Fatalf("invalid snapshot = %+v, want empty SourcePlural", snapshot)
			}
		})
	}
}

func TestLoadSnapshotDuplicateErrorDoesNotExposeIdentifier(t *testing.T) {
	dir := t.TempDir()
	store := newTestStore(t, dir)
	const accessKey = "AK-SENSITIVE-DO-NOT-LOG"
	body := `{"version":1,"accounts":[` +
		`{"id":"` + accessKey + `","api_key":"key-1","access_key":"ak-1","secret_key":"sk-1"},` +
		`{"id":"` + accessKey + `","api_key":"key-2","access_key":"ak-2","secret_key":"sk-2"}]}`
	if err := os.WriteFile(store.PoolPath("volcengine"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := store.LoadSnapshot("volcengine", "volcengine")
	if err == nil {
		t.Fatal("duplicate account id should fail")
	}
	if strings.Contains(err.Error(), accessKey) {
		t.Fatalf("validation error exposed credential identifier: %q", err)
	}
	if !strings.Contains(err.Error(), "accounts[1].id duplicates an earlier account") {
		t.Fatalf("validation error = %q, want index-only duplicate diagnostic", err)
	}
}

func TestLoadSnapshotRejectsEmptyLegacyKey(t *testing.T) {
	dir := t.TempDir()
	store := newTestStore(t, dir)
	if err := os.WriteFile(store.LegacyPath("zhipu"), []byte(`{"api_key":""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.LoadSnapshot("zhipu", "zhipu")
	if err == nil || !strings.Contains(err.Error(), "api_key is empty") {
		t.Fatalf("LoadSnapshot() error = %v, want empty api_key", err)
	}
	if snapshot.Source != SourceLegacy || len(snapshot.Pool.Accounts) != 0 {
		t.Fatalf("invalid legacy snapshot = %+v, want empty SourceLegacy", snapshot)
	}
}

func TestLoadPoolEmpty(t *testing.T) {
	dir := t.TempDir()
	store := newTestStore(t, dir)
	snapshot, err := store.LoadSnapshot("zhipu", "zhipu")
	if err != nil {
		t.Fatalf("missing pool should be empty, not error: %v", err)
	}
	if snapshot.Source != SourceMissing {
		t.Fatalf("source = %v, want SourceMissing", snapshot.Source)
	}
	pool := snapshot.Pool
	if len(pool.Accounts) != 0 {
		t.Fatalf("want 0 accounts, got %d", len(pool.Accounts))
	}
}

// TestSavePoolAtomic_NoTruncationOnDisk verifies savePool writes via temp +
// rename: the final file is whole JSON (parseable) and no stray .tmp is left.
// Regression guard for #1/#2A (crash mid-write must not strand a truncated pool).
func TestSavePoolAtomic_NoTruncationOnDisk(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	if err := os.MkdirAll(filepath.Dir(store.PoolPath("deepseek")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(store.PoolPath("deepseek")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.PoolPath("deepseek")+".tmp", []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(store.PoolPath("deepseek")+".tmp", 0o644); err != nil {
		t.Fatal(err)
	}
	in := Pool{Version: 1, Accounts: []Account{
		{ID: "id1", Label: "home", APIKey: "k1", AddedAt: "2026-07-08T00:00:00Z"},
	}}
	if err := store.Save("deepseek", "deepseek", in); err != nil {
		t.Fatal(err)
	}
	// No leftover temp file.
	if _, err := os.Stat(store.PoolPath("deepseek") + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf(".tmp leftover after savePool: %v", err)
	}
	// The written file is parseable (not a half-written truncation).
	out, err := store.Load("deepseek", "deepseek")
	if err != nil {
		t.Fatalf("reload after atomic save: %v", err)
	}
	if len(out.Accounts) != 1 || out.Accounts[0].APIKey != "k1" {
		t.Fatalf("round-trip mismatch: %+v", out.Accounts)
	}
	if info, err := os.Stat(store.PoolPath("deepseek")); err != nil {
		t.Fatal(err)
	} else if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("pool mode = %04o, want 0600", got)
	}
	if info, err := os.Stat(filepath.Dir(store.PoolPath("deepseek"))); err != nil {
		t.Fatal(err)
	} else if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("pool directory mode = %04o, want 0700", got)
	}
}

// TestWithPoolLock_MutualExclusion asserts the O_EXCL lockfile serializes
// callers: at no point do two callbacks run their critical section
// concurrently. Spawns several goroutines (more than GOMAXPROCS to force real
// contention) and records the peak number of overlapping entries.
func TestWithPoolLock_MutualExclusion(t *testing.T) {
	dir := t.TempDir()
	store := newTestStore(t, dir)
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
			if err := store.WithLock(name, fn); err != nil {
				t.Errorf("withPoolLock: %v", err)
			}
		}()
	}
	wg.Wait()
	if maxCS != 1 {
		t.Fatalf("critical section overlapped: peak concurrency = %d, want 1", maxCS)
	}
	// Lockfile cleaned up after every holder released.
	if _, err := os.Stat(store.PoolPath(name) + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("lockfile left behind after all holders: %v", err)
	}
}

func TestWithPoolLockCallbackErrorCleansUp(t *testing.T) {
	store := NewStore(t.TempDir())
	want := errors.New("mutation failed")
	if err := store.WithLock("zhipu", func() error {
		info, err := os.Stat(store.PoolPath("zhipu") + ".lock")
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("lock mode = %04o, want 0600", got)
		}
		return want
	}); !errors.Is(err, want) {
		t.Fatalf("WithLock() error = %v, want %v", err, want)
	}
	if _, err := os.Stat(store.PoolPath("zhipu") + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("lockfile left behind after callback error: %v", err)
	}
}

// TestWithPoolLock_StaleSteal asserts a lockfile whose mtime is older than
// poolLockStaleAge is treated as stranded (holder crashed) and stolen: withPoolLock
// removes it, re-acquires, runs fn, and cleans up. This is the crash-recovery
// path that makes O_EXCL safe without flock.
func TestWithPoolLock_StaleSteal(t *testing.T) {
	dir := t.TempDir()
	store := newTestStore(t, dir)
	const name = "deepseek"
	lockPath := store.PoolPath(name) + ".lock"
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
	if err := store.WithLock(name, func() error { ran = true; return nil }); err != nil {
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

// TestWithPoolLock_FreshLockIsBusy asserts a live holder is not stolen. Both
// contenders use the real O_EXCL lock path; the wait seam only pauses the
// waiter after it has observed contention. The holder's WithLock must return
// (and therefore run its deferred lockfile removal) before the waiter retries.
func TestWithPoolLock_FreshLockIsBusy(t *testing.T) {
	dir := t.TempDir()
	store := newTestStore(t, dir)
	const name = "volcengine"
	lockPath := store.PoolPath(name) + ".lock"

	holderEntered := make(chan struct{})
	releaseHolder := make(chan struct{})
	holderResult := make(chan error, 1)
	var releaseHolderOnce sync.Once
	t.Cleanup(func() { releaseHolderOnce.Do(func() { close(releaseHolder) }) })
	go func() {
		holderResult <- store.WithLock(name, func() error {
			close(holderEntered)
			<-releaseHolder
			return nil
		})
	}()
	select {
	case <-holderEntered:
	case <-time.After(time.Second):
		t.Fatal("holder did not acquire the fresh lock")
	}

	waiterBlocked := make(chan struct{}, 1)
	retryWaiter := make(chan struct{})
	callbackEntered := make(chan struct{})
	waiterResult := make(chan error, 1)
	var retryWaiterOnce sync.Once
	t.Cleanup(func() { retryWaiterOnce.Do(func() { close(retryWaiter) }) })
	go func() {
		waiterResult <- store.withLock(name, func() error {
			close(callbackEntered)
			return nil
		}, func(time.Duration) {
			select {
			case waiterBlocked <- struct{}{}:
			default:
			}
			<-retryWaiter
		})
	}()
	select {
	case <-waiterBlocked:
	case err := <-waiterResult:
		t.Fatalf("waiter returned instead of observing the live holder: %v", err)
	case <-time.After(time.Second):
		t.Fatal("waiter did not reach the contention wait point")
	}
	select {
	case <-callbackEntered:
		t.Fatal("waiter callback entered while the holder still owned the lock")
	default:
	}

	releaseHolderOnce.Do(func() { close(releaseHolder) })
	select {
	case err := <-holderResult:
		if err != nil {
			t.Fatalf("holder WithLock: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("holder did not finish releasing the lock")
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("holder returned before removing its lockfile: %v", err)
	}

	retryWaiterOnce.Do(func() { close(retryWaiter) })
	select {
	case err := <-waiterResult:
		if err != nil {
			t.Fatalf("waiter WithLock after holder release: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter did not acquire after holder release")
	}
	select {
	case <-callbackEntered:
	default:
		t.Fatal("callback did not run after holder release")
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("lockfile not cleaned up: %v", err)
	}
}

// Regression (credential red line): pools written before the volcengine id
// became a hash stored the raw access key as the account id; those ids flow
// into virtual ids, logs, request-log JSONL and persisted runtime state.
// Loading must converge them onto the hashed derivation — and collapse
// old/new duplicates of the same credential — so existing installs stop
// leaking without waiting for a re-login rewrite.
func TestLoadSnapshotNormalizesLegacyVolcengineIDs(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	body := `{"version":1,"accounts":[` +
		`{"id":"AKSECRET123","label":"old","api_key":"k1","access_key":"AKSECRET123","secret_key":"s1","added_at":"x"},` +
		`{"id":"` + AccountID("volcengine", Credentials{APIKey: "k1", AccessKey: "AKSECRET123", SecretKey: "s1"}) + `","label":"dup","api_key":"k1","access_key":"AKSECRET123","secret_key":"s1","added_at":"y"},` +
		`{"id":"AKOTHER456","label":"second","api_key":"k2","access_key":"AKOTHER456","secret_key":"s2","added_at":"z"}]}`
	if err := os.WriteFile(store.PoolPath("volcengine"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	snapshot, err := store.LoadSnapshot("volcengine", "volcengine")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Source != SourcePlural {
		t.Fatalf("source = %v, want SourcePlural", snapshot.Source)
	}
	if got := len(snapshot.Pool.Accounts); got != 2 {
		t.Fatalf("accounts = %d, want 2 (same-credential duplicate collapsed)", got)
	}
	for _, account := range snapshot.Pool.Accounts {
		if strings.Contains(account.ID, "AKSECRET123") || strings.Contains(account.ID, "AKOTHER456") {
			t.Fatalf("id %q still carries raw access key material", account.ID)
		}
		if want := AccountID("volcengine", account.Credentials()); account.ID != want {
			t.Fatalf("id %q not normalized to %q", account.ID, want)
		}
	}
}
