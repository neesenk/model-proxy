package cache

import (
	"testing"
	"time"
)

func TestSharedCountersKeepGenerationEntriesIsolated(t *testing.T) {
	counters := &Counters{}
	counters.Seed(3, 4, []ModelStat{{Name: "m", Hits: 3, Misses: 4}})
	options := Options{Counters: counters, TTL: time.Hour, MaxEntries: 10}
	old, current := New(options), New(options)
	now := time.Now()
	old.Put("key", "m", 200, nil, []byte("old response"), now)
	if _, hit := current.Lookup("key", "m", now); hit {
		t.Fatal("new generation saw old entry")
	}
	if _, hit := old.Lookup("key", "m", now); !hit {
		t.Fatal("old generation lost entry")
	}
	if got := current.Stats(); got.Hits != 4 || got.Misses != 5 || got.Entries != 0 || got.Models[0].Entries != 0 {
		t.Fatalf("current stats = %+v", got)
	}
	if got := old.Stats(); got.Entries != 1 || got.Models[0].Entries != 1 {
		t.Fatalf("old stats = %+v", got)
	}
	snapshot := counters.Stats()
	snapshot.Models[0].Misses = 999
	if got := counters.Stats(); got.Misses != 5 || got.Models[0].Misses != 5 || got.Entries != 0 {
		t.Fatalf("counters aliased snapshot: %+v", got)
	}
	counters.Reset()
	if got := current.Stats(); got.Hits != 0 || got.Misses != 0 || len(got.Models) != 0 {
		t.Fatalf("reset = %+v", got)
	}
	if _, hit := old.Lookup("key", "m", now); !hit {
		t.Fatal("counter reset unexpectedly changed old entries")
	}
	if got := counters.Stats(); got.Hits != 1 || got.Models[0].Hits != 1 {
		t.Fatalf("post-reset lookup = %+v", got)
	}
}
