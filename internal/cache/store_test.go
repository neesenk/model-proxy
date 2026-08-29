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
	store.Put("key", http.StatusOK, header, body, now)
	header.Set("Content-Type", "mutated")
	body[0] = 'X'

	if got := store.Stats(); got != (Stats{Entries: 1}) {
		t.Errorf("post-Put Stats = %+v, want one entry and zero counters", got)
	}
	entry, ok := store.Lookup("key", now.Add(time.Second))
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
	if got := store.Stats(); got != (Stats{Hits: 1, Entries: 1}) {
		t.Errorf("post-hit Stats = %+v", got)
	}

	if entry, ok := store.Lookup("key", now.Add(2*time.Hour)); ok || entry != nil {
		t.Errorf("expired Lookup = (%v, %v), want miss", entry, ok)
	}
	if got := store.Stats(); got != (Stats{Hits: 1, Misses: 1}) {
		t.Errorf("post-expiry Stats = %+v, want hit=1 miss=1 entries=0", got)
	}
}

func TestStoreNilSafeAndReset(t *testing.T) {
	var store *Store
	if entry, ok := store.Lookup("key", time.Now()); ok || entry != nil {
		t.Errorf("nil Lookup = (%v, %v), want miss", entry, ok)
	}
	store.Put("key", http.StatusOK, nil, []byte("body"), time.Now())
	store.Reset()
	if got := store.Stats(); got != (Stats{}) {
		t.Errorf("nil Stats = %+v, want zero", got)
	}
	if got := store.MaxBodyBytes(); got != 0 {
		t.Errorf("nil MaxBodyBytes = %d, want 0", got)
	}
	store = testStore()
	store.Put("", http.StatusOK, nil, []byte("body"), time.Now())
	if got := store.Stats(); got.Entries != 0 {
		t.Errorf("empty-key Put retained %d entries, want 0", got.Entries)
	}
	store.Put("key", http.StatusOK, nil, []byte("body"), time.Now())
	store.Lookup("missing", time.Now())
	store.Reset()
	if got := store.Stats(); got != (Stats{}) {
		t.Errorf("Stats after Reset = %+v, want zero", got)
	}
}

func TestStoreEvictsAtCapacity(t *testing.T) {
	store := New(Options{TTL: time.Hour, MaxEntries: 2, MaxBodyBytes: 10})
	now := time.Unix(1000, 0)
	store.Put("a", 200, nil, []byte("a"), now)
	store.Put("b", 200, nil, []byte("b"), now)
	store.Put("c", 200, nil, []byte("c"), now)
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
	store.Put("k", 200, nil, big, now)
	if _, ok := store.Lookup("k", now); !ok {
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
	e, ok := store.Lookup(key, now)
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
	store.Put("live", http.StatusOK, http.Header{}, []byte("ok"), now)
	store.Put("stale", http.StatusOK, http.Header{}, []byte("old"), now)

	if !store.Peek("live", now.Add(time.Second)) {
		t.Error("Peek(live entry) = false, want true")
	}
	if store.Peek("absent", now.Add(time.Second)) {
		t.Error("Peek(absent key) = true, want false")
	}
	if store.Peek("stale", now.Add(2*time.Hour)) {
		t.Error("Peek(expired entry) = true, want false")
	}
	if got := store.Stats(); got != (Stats{Entries: 2}) {
		t.Errorf("post-Peek Stats = %+v, want zero counters and the expired entry still present", got)
	}
	// The expired entry is left for a real Lookup to evict.
	if _, ok := store.Lookup("stale", now.Add(2*time.Hour)); ok {
		t.Error("expired Lookup must miss")
	}
	if got := store.Stats(); got != (Stats{Misses: 1, Entries: 1}) {
		t.Errorf("post-Lookup Stats = %+v, want miss=1 entries=1", got)
	}
}
