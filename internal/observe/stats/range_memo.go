package stats

import (
	"math"
	"sync"
)

// Range-query memo for LoadCumulativeRange / LoadCumulativeAgentsRange.
//
// The Web token-usage selector (Status tab tokens cards, Accounts page)
// re-requests the same cumulative range on every 5s tick, and each answer is a
// GROUP BY over every minute bucket in the range — on a months-old stats.db a
// 30-day range costs ~0.3s of CPU per request under the pure-Go SQLite driver.
// The tables change only through this Store's mutation methods (the daemon is
// stats.db's single writer; CLI readers go through the daemon API), so an
// aggregate computed under mutation generation g is exact until the next
// mutation ends.
//
// Ordering contract: a reader loads the generation BEFORE it queries, and
// every mutation method advances the generation only AFTER its transaction
// ends (noteMutation runs deferred). A query racing a mutation is therefore
// tagged with the pre-mutation generation and can never be served once the
// mutation has landed. Callers own the maps they receive: entries are cloned
// on the way out, never shared.

// rangeMemoCapacity bounds the memo per query kind. The selector produces a
// handful of live keys (one per open range preset); stale-generation entries
// are evicted first.
const rangeMemoCapacity = 16

// rangeKey is a normalized [from, to] bound pair: the same normalization the
// queries apply (from < 0 → 0, to <= 0 → unbounded), so equivalent spellings
// share one entry.
type rangeKey struct{ from, to int64 }

func normalizeRange(from, to int64) rangeKey {
	if to <= 0 {
		to = math.MaxInt64
	}
	return rangeKey{from: max(from, 0), to: to}
}

type rangeMemoEntry[V any] struct {
	generation uint64
	value      V
}

// rangeMemo is a leaf-locked, bounded map from range to the aggregate computed
// under one mutation generation. No method calls out while holding mu.
type rangeMemo[V any] struct {
	mu      sync.Mutex
	entries map[rangeKey]rangeMemoEntry[V]
}

// get returns the entry for key only when it was computed under generation.
func (m *rangeMemo[V]) get(key rangeKey, generation uint64) (V, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.entries[key]
	if !ok || entry.generation != generation {
		var zero V
		return zero, false
	}
	return entry.value, true
}

// put records value for key under generation. A slow reader finishing after a
// faster one never replaces a newer-generation entry with its older answer.
func (m *rangeMemo[V]) put(key rangeKey, generation uint64, value V) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.entries[key]; ok && existing.generation > generation {
		return
	}
	if m.entries == nil {
		m.entries = make(map[rangeKey]rangeMemoEntry[V], rangeMemoCapacity)
	}
	if _, ok := m.entries[key]; !ok && len(m.entries) >= rangeMemoCapacity {
		for stale, entry := range m.entries {
			if entry.generation != generation {
				delete(m.entries, stale)
			}
		}
		for victim := range m.entries {
			if len(m.entries) < rangeMemoCapacity {
				break
			}
			delete(m.entries, victim)
		}
	}
	m.entries[key] = rangeMemoEntry[V]{generation: generation, value: value}
}

// noteMutation advances the mutation generation, retiring every memo entry.
// Mutation methods call it deferred so it runs after the transaction ends
// (committed or rolled back — a rollback only costs a recomputation).
func (s *Store) noteMutation() {
	s.mutations.Add(1)
}
