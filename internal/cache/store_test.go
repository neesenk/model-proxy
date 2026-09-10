package cache

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func testStore() *Store {
	return New(Options{TTL: time.Hour, MaxEntries: 10, MaxBodyBytes: 1024})
}

func TestStoreLookupExpiryAndCounters(t *testing.T) {
	store := testStore()
	now := time.Unix(1000, 0)
	header := http.Header{"Content-Type": {"application/json"}}
	body := []byte("ok")
	store.Put("key", "m", http.StatusOK, header, body, now)
	header.Set("Content-Type", "mutated")
	body[0] = 'X'

	if got := store.Stats(); got.Hits != 0 || got.Misses != 0 || got.Entries != 1 {
		t.Errorf("post-Put Stats = %+v, want one entry and zero counters", got)
	}
	entry, ok := store.Lookup("key", "m", now.Add(time.Second))
	if !ok || entry.Status() != http.StatusOK {
		t.Fatalf("Lookup = (%v, %v), want status 200 hit", entry, ok)
	}
	recorder := newRecordingWriter()
	if err := Replay(recorder, entry); err != nil {
		t.Fatal(err)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("stored header = %q, want detached application/json", got)
	}
	if got := recorder.body.String(); got != "ok" {
		t.Errorf("stored body = %q, want detached ok", got)
	}
	if got := store.Stats(); got.Hits != 1 || got.Misses != 0 || got.Entries != 1 {
		t.Errorf("post-hit Stats = %+v", got)
	}

	if entry, ok := store.Lookup("key", "m", now.Add(2*time.Hour)); ok || entry != nil {
		t.Errorf("expired Lookup = (%v, %v), want miss", entry, ok)
	}
	if got := store.Stats(); got.Hits != 1 || got.Misses != 1 || got.Entries != 0 {
		t.Errorf("post-expiry Stats = %+v, want hit=1 miss=1 entries=0", got)
	}
}

func TestStoreNilSafeAndReset(t *testing.T) {
	var store *Store
	if entry, ok := store.Lookup("key", "m", time.Now()); ok || entry != nil {
		t.Errorf("nil Lookup = (%v, %v), want miss", entry, ok)
	}
	store.Put("key", "m", http.StatusOK, nil, []byte("body"), time.Now())
	store.Reset()
	if got := store.Stats(); got.Hits != 0 || got.Misses != 0 || got.Entries != 0 {
		t.Errorf("nil Stats = %+v, want zero", got)
	}
	if got := store.MaxBodyBytes(); got != 0 {
		t.Errorf("nil MaxBodyBytes = %d, want 0", got)
	}
	store = testStore()
	store.Put("", "m", http.StatusOK, nil, []byte("body"), time.Now())
	if got := store.Stats(); got.Entries != 0 {
		t.Errorf("empty-key Put retained %d entries, want 0", got.Entries)
	}
	store.Put("key", "m", http.StatusOK, nil, []byte("body"), time.Now())
	store.Lookup("missing", "m", time.Now())
	store.Reset()
	if got := store.Stats(); got.Hits != 0 || got.Misses != 0 || got.Entries != 0 || len(got.Models) != 0 {
		t.Errorf("Stats after Reset = %+v, want zero (counters, entries and per-model breakdown)", got)
	}
}

func TestStoreEvictsAtCapacity(t *testing.T) {
	store := New(Options{TTL: time.Hour, MaxEntries: 2, MaxBodyBytes: 10})
	now := time.Unix(1000, 0)
	store.Put("a", "m", 200, nil, []byte("a"), now)
	store.Put("b", "m", 200, nil, []byte("b"), now)
	store.Put("c", "m", 200, nil, []byte("c"), now)
	if got := store.Stats().Entries; got != 2 {
		t.Errorf("entries after over-cap Put = %d, want 2", got)
	}
	if got := store.MaxBodyBytes(); got != 10 {
		t.Errorf("MaxBodyBytes = %d, want 10", got)
	}
}

// TestStorePutDoesNotEnforceMaxBodyBytes pins the eligibility split: the
// Store never truncates or rejects on MaxBodyBytes — body-size eligibility is
// the CALLER's decision (the recorder truncates, the root decides whether to
// Put). If Store.Put ever grew its own cap, this test goes red and the split
// must be re-decided deliberately.
func TestStorePutDoesNotEnforceMaxBodyBytes(t *testing.T) {
	store := New(Options{TTL: time.Hour, MaxEntries: 2, MaxBodyBytes: 4})
	now := time.Unix(1000, 0)
	big := []byte("0123456789abcdef") // 16 bytes > MaxBodyBytes 4
	store.Put("k", "m", 200, nil, big, now)
	if _, ok := store.Lookup("k", "m", now); !ok {
		t.Fatal("oversized-vs-MaxBodyBytes body must still be stored (caller owns eligibility)")
	}
	rec := httptest.NewRecorder()
	if err := Replay(rec, mustLookup(t, store, "k", now)); err != nil {
		t.Fatalf("replay oversized entry: %v", err)
	}
	if got := rec.Body.String(); got != string(big) {
		t.Fatalf("replayed body = %q, want the caller's bytes verbatim", got)
	}
}

func mustLookup(t *testing.T, store *Store, key string, now time.Time) *Entry {
	t.Helper()
	e, ok := store.Lookup(key, "m", now)
	if !ok {
		t.Fatalf("lookup %q: missing", key)
	}
	return e
}

// Peek is the read-only probe for diagnostics: it reports live entries
// without booking hits/misses or evicting expired ones (a polled preview
// must not grind the operational hit-rate metrics down).
func TestStorePeekDoesNotMutate(t *testing.T) {
	store := testStore()
	now := time.Unix(1000, 0)
	store.Put("live", "m", http.StatusOK, http.Header{}, []byte("ok"), now)
	store.Put("stale", "m", http.StatusOK, http.Header{}, []byte("old"), now)

	if !store.Peek("live", now.Add(time.Second)) {
		t.Error("Peek(live entry) = false, want true")
	}
	if store.Peek("absent", now.Add(time.Second)) {
		t.Error("Peek(absent key) = true, want false")
	}
	if store.Peek("stale", now.Add(2*time.Hour)) {
		t.Error("Peek(expired entry) = true, want false")
	}
	if got := store.Stats(); got.Hits != 0 || got.Misses != 0 || got.Entries != 2 {
		t.Errorf("post-Peek Stats = %+v, want zero counters and the expired entry still present", got)
	}
	// The expired entry is left for a real Lookup to evict.
	if _, ok := store.Lookup("stale", "m", now.Add(2*time.Hour)); ok {
		t.Error("expired Lookup must miss")
	}
	if got := store.Stats(); got.Hits != 0 || got.Misses != 1 || got.Entries != 1 {
		t.Errorf("post-Lookup Stats = %+v, want miss=1 entries=1", got)
	}
}

func TestStorePerModelBreakdown(t *testing.T) {
	store := testStore()
	now := time.Unix(1000, 0)
	store.Put("glm-key", "glm-5.2", http.StatusOK, nil, []byte("a"), now)
	store.Put("kimi-key", "kimi-k3", http.StatusOK, nil, []byte("b"), now)
	// Misses attributed to the calling model, including a model that never
	// stored anything.
	store.Lookup("missing", "glm-5.2", now)
	store.Lookup("missing", "deepseek-v4", now)

	stats := store.Stats()
	if len(stats.Models) != 3 {
		t.Fatalf("models = %+v, want 3 rows (glm-5.2, kimi-k3, deepseek-v4)", stats.Models)
	}
	// Sorted by model name; glm-5.2: 1 miss + 1 live entry, kimi-k3: entry only,
	// deepseek-v4: miss only.
	want := []ModelStat{
		{Name: "deepseek-v4", Misses: 1},
		{Name: "glm-5.2", Misses: 1, Entries: 1},
		{Name: "kimi-k3", Entries: 1},
	}
	for i, m := range stats.Models {
		if m != want[i] {
			t.Errorf("models[%d] = %+v, want %+v", i, m, want[i])
		}
	}

	// Hits attribute to the request that experiences them: each lookup passes
	// its own called model, so a hit on kimi-k3's entry made by a glm request
	// counts for glm-5.2.
	if _, ok := store.Lookup("glm-key", "glm-5.2", now.Add(time.Second)); !ok {
		t.Fatal("glm-key lookup should hit")
	}
	if _, ok := store.Lookup("kimi-key", "glm-5.2", now.Add(time.Second)); !ok {
		t.Fatal("kimi-key lookup should hit regardless of model")
	}
	stats = store.Stats()
	if stats.Hits != 2 || stats.Misses != 2 {
		t.Errorf("global counters = hits %d misses %d, want 2/2", stats.Hits, stats.Misses)
	}
	byName := map[string]ModelStat{}
	for _, m := range stats.Models {
		byName[m.Name] = m
	}
	// Both lookups carried model glm-5.2: both hits count for it; the live
	// kimi entry stays under kimi-k3.
	if got := byName["glm-5.2"]; got.Hits != 2 || got.Misses != 1 {
		t.Errorf("glm-5.2 = %+v, want 2 hits 1 miss", got)
	}
	if got := byName["kimi-k3"]; got.Hits != 0 || got.Entries != 1 {
		t.Errorf("kimi-k3 = %+v, want 0 hits 1 entry", got)
	}

	// Unattributed (empty-model) lookups count only in the global totals.
	store.Lookup("missing", "", now)
	stats = store.Stats()
	if stats.Misses != 3 {
		t.Errorf("global misses = %d, want 3", stats.Misses)
	}
	for _, m := range stats.Models {
		if m.Name == "" {
			t.Error("empty model name must not produce a breakdown row")
		}
	}

	// Expired-entry misses attribute to the looking-up model too.
	if _, ok := store.Lookup("glm-key", "glm-5.2", now.Add(2*time.Hour)); ok {
		t.Fatal("expired lookup should miss")
	}
	if got := store.Stats().Models[1]; got.Misses != 2 {
		t.Errorf("glm-5.2 misses after expiry = %d, want 2", got.Misses)
	}

	// Reset clears the breakdown along with everything else.
	store.Reset()
	if got := store.Stats(); len(got.Models) != 0 || got.Entries != 0 {
		t.Errorf("stats after Reset = %+v, want empty breakdown", got)
	}
}

func TestStoreSeedAccumulatesPersistedCounters(t *testing.T) {
	store := testStore()
	now := time.Unix(1000, 0)
	// Seed a fresh store the way the app's state-file loader does.
	store.Seed(3, 7, []ModelStat{
		{Name: "glm-5.2", Hits: 2, Misses: 4},
		{Name: "kimi-k3", Hits: 1, Misses: 3},
	})
	stats := store.Stats()
	if stats.Hits != 3 || stats.Misses != 7 {
		t.Errorf("seeded counters = hits %d misses %d, want 3/7", stats.Hits, stats.Misses)
	}
	if len(stats.Models) != 2 {
		t.Fatalf("seeded models = %+v, want 2 rows", stats.Models)
	}
	// Seeding must not fabricate live entries.
	if stats.Entries != 0 {
		t.Errorf("seeded entries = %d, want 0 (bodies are not persisted)", stats.Entries)
	}
	// Live traffic accumulates on top of the seeds.
	store.Put("k", "glm-5.2", http.StatusOK, nil, []byte("x"), now)
	store.Lookup("k", "glm-5.2", now)
	stats = store.Stats()
	if stats.Hits != 4 || stats.Entries != 1 {
		t.Errorf("post-seed traffic = hits %d entries %d, want 4/1", stats.Hits, stats.Entries)
	}
	// Empty model names in a seed file never create a breakdown row.
	store.Seed(1, 1, []ModelStat{{Name: "", Hits: 1, Misses: 1}})
	for _, m := range store.Stats().Models {
		if m.Name == "" {
			t.Error("empty model name must not produce a breakdown row")
		}
	}
	// A nil store ignores the seed.
	var nilStore *Store
	nilStore.Seed(1, 1, nil)
}
