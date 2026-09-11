// Package cache owns the in-memory exact-response cache.
package cache

import (
	"net/http"
	"sort"
	"sync"
	"time"
)

// Options contains already-resolved cache policy values.
type Options struct {
	// Counters may be shared across generations; response entries never are.
	Counters     *Counters
	TTL          time.Duration
	MaxEntries   int
	MaxBodyBytes int
}

// Stats is one atomic snapshot of cache counters and entry count. Hits/Misses
// are cumulative since process start (or last Reset); Entries is a live gauge
// of currently-held entries — it can legitimately be lower than Misses
// (failed/streaming requests miss without ever storing, TTL lazily expires
// entries, capacity eviction caps the map while counters keep growing).
type Stats struct {
	Hits    uint64
	Misses  uint64
	Entries uint64
	// Models breaks the counters down by called (exposed) model name, sorted
	// by name; Entries here is the model's share of the live entries.
	Models []ModelStat
}

// ModelStat is one model's share of the cache counters.
type ModelStat struct {
	Name    string
	Hits    uint64
	Misses  uint64
	Entries uint64
}

// Entry is an immutable cached client-facing response.
type Entry struct {
	model     string
	status    int
	header    http.Header
	body      []byte
	expiresAt time.Time
}

// Status returns the cached HTTP status.
func (e *Entry) Status() int {
	if e == nil {
		return 0
	}
	return e.status
}

// Store is a bounded, concurrency-safe exact-response cache.
type Store struct {
	counters     *Counters
	entries      map[string]*Entry
	ttl          time.Duration
	maxEntries   int
	maxBodyBytes int
}

// Counters owns cumulative statistics across Store generations. Its mutex also
// protects the entries of attached stores, so Stats remains an atomic view.
// The zero value is ready to use, including while caching is disabled.
type Counters struct {
	mu     sync.Mutex
	hits   uint64
	misses uint64
	// perModel breaks hits/misses down by called model name; the empty name
	// (an unattributed lookup) counts only in the global totals.
	perModel map[string]*modelCounters
}

type modelCounters struct {
	hits   uint64
	misses uint64
}

// New returns a Store for already-resolved options. Validation and defaults
// belong to the application adapter.
func New(options Options) *Store {
	counters := options.Counters
	if counters == nil {
		counters = &Counters{}
	}
	return &Store{
		counters:     counters,
		entries:      map[string]*Entry{},
		ttl:          options.TTL,
		maxEntries:   options.MaxEntries,
		maxBodyBytes: options.MaxBodyBytes,
	}
}

// Lookup returns a live entry. Expired entries are lazily evicted. A nil Store
// always misses. model is the called (exposed) model name the lookup is
// attributed to in the per-model breakdown; the empty name counts only in the
// global totals.
func (s *Store) Lookup(key, model string, now time.Time) (*Entry, bool) {
	if s == nil {
		return nil, false
	}
	s.counters.mu.Lock()
	defer s.counters.mu.Unlock()
	entry, ok := s.entries[key]
	if !ok {
		s.counters.misses++
		s.counters.model(model).misses++
		return nil, false
	}
	if now.After(entry.expiresAt) {
		delete(s.entries, key)
		s.counters.misses++
		s.counters.model(model).misses++
		return nil, false
	}
	s.counters.hits++
	s.counters.model(model).hits++
	return entry, true
}

// model returns (creating on demand) the per-model counters under the store
// lock; the caller must hold s.mu.
func (s *Counters) model(name string) *modelCounters {
	if s.perModel == nil {
		s.perModel = map[string]*modelCounters{}
	}
	c, ok := s.perModel[name]
	if !ok {
		c = &modelCounters{}
		s.perModel[name] = c
	}
	return c
}

// Peek reports whether a live entry exists WITHOUT mutating the store: no
// hit/miss counters, no lazy eviction. For read-only diagnostics (the
// /debug/route preview) that must not skew the operational hit-rate metrics
// a poller would otherwise grind down. A nil Store or an expired entry
// reports a miss (the expired entry is left for a real Lookup to evict).
func (s *Store) Peek(key string, now time.Time) bool {
	if s == nil {
		return false
	}
	s.counters.mu.Lock()
	defer s.counters.mu.Unlock()
	entry, ok := s.entries[key]
	return ok && !now.After(entry.expiresAt)
}

// Put stores a detached response, evicting one arbitrary entry at capacity.
// A nil Store or empty key is a no-op. Response eligibility, including whether
// an empty body is cacheable, belongs to the caller. model attributes the
// entry (its live count) to a called model name in the per-model breakdown.
func (s *Store) Put(key, model string, status int, header http.Header, body []byte, now time.Time) {
	if s == nil || key == "" {
		return
	}
	entry := &Entry{
		model:     model,
		status:    status,
		header:    header.Clone(),
		body:      append([]byte(nil), body...),
		expiresAt: now.Add(s.ttl),
	}
	s.counters.mu.Lock()
	defer s.counters.mu.Unlock()
	if len(s.entries) >= s.maxEntries {
		for existing := range s.entries {
			delete(s.entries, existing)
			break
		}
	}
	s.entries[key] = entry
}

// Reset drops all entries and counters.
func (s *Store) Reset() {
	if s == nil {
		return
	}
	s.counters.mu.Lock()
	defer s.counters.mu.Unlock()
	s.entries = map[string]*Entry{}
	s.counters.resetLocked()
}

func (s *Counters) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resetLocked()
}

func (s *Counters) resetLocked() {
	s.perModel = map[string]*modelCounters{}
	s.hits = 0
	s.misses = 0
}

// Seed accumulates persisted counters. Call only at startup, never once per
// generation when counters are shared. Entries are deliberately not seedable.
// The empty model name is ignored (unattributed lookups count only globally).
func (s *Store) Seed(hits, misses uint64, models []ModelStat) {
	if s == nil {
		return
	}
	s.counters.Seed(hits, misses, models)
}

// Seed is used once at startup, before stores begin accepting requests.
func (s *Counters) Seed(hits, misses uint64, models []ModelStat) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hits += hits
	s.misses += misses
	for _, m := range models {
		if m.Name == "" {
			continue
		}
		c := s.model(m.Name)
		c.hits += m.Hits
		c.misses += m.Misses
	}
}

// Stats returns counters, the live entry count and the per-model breakdown
// (sorted by model name) under one lock.
func (s *Store) Stats() Stats {
	if s == nil {
		return Stats{}
	}
	s.counters.mu.Lock()
	defer s.counters.mu.Unlock()
	liveEntries := make(map[string]uint64)
	for _, entry := range s.entries {
		liveEntries[entry.model]++
	}
	out := s.counters.statsLocked(liveEntries)
	out.Entries = uint64(len(s.entries))
	return out
}

// Stats returns cumulative counters without any generation's entry gauge.
func (s *Counters) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statsLocked(nil)
}

func (s *Counters) statsLocked(liveEntries map[string]uint64) Stats {
	models := make([]ModelStat, 0, len(s.perModel))
	for name, counters := range s.perModel {
		if name == "" {
			continue // unattributed lookups count only in the global totals
		}
		models = append(models, ModelStat{
			Name:    name,
			Hits:    counters.hits,
			Misses:  counters.misses,
			Entries: liveEntries[name],
		})
		delete(liveEntries, name)
	}
	// Entries stored under a name that never missed (defensive; every Put is
	// preceded by a Lookup) still deserve a row.
	for name, entries := range liveEntries {
		models = append(models, ModelStat{Name: name, Entries: entries})
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Name < models[j].Name })
	return Stats{
		Hits:   s.hits,
		Misses: s.misses,
		Models: models,
	}
}

// MaxBodyBytes returns the capture limit configured for this Store.
func (s *Store) MaxBodyBytes() int {
	if s == nil {
		return 0
	}
	return s.maxBodyBytes
}
