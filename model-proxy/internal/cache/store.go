// Package cache owns the in-memory exact-response cache.
package cache

import (
	"net/http"
	"sync"
	"time"
)

// Options contains already-resolved cache policy values.
type Options struct {
	TTL          time.Duration
	MaxEntries   int
	MaxBodyBytes int
}

// Stats is one atomic snapshot of cache counters and entry count.
type Stats struct {
	Hits    uint64
	Misses  uint64
	Entries uint64
}

// Entry is an immutable cached client-facing response.
type Entry struct {
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
	mu           sync.Mutex
	entries      map[string]*Entry
	ttl          time.Duration
	maxEntries   int
	maxBodyBytes int
	hits         uint64
	misses       uint64
}

// New returns a Store for already-resolved options. Validation and defaults
// belong to the application adapter.
func New(options Options) *Store {
	return &Store{
		entries:      map[string]*Entry{},
		ttl:          options.TTL,
		maxEntries:   options.MaxEntries,
		maxBodyBytes: options.MaxBodyBytes,
	}
}

// Lookup returns a live entry. Expired entries are lazily evicted. A nil Store
// always misses.
func (s *Store) Lookup(key string, now time.Time) (*Entry, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[key]
	if !ok {
		s.misses++
		return nil, false
	}
	if now.After(entry.expiresAt) {
		delete(s.entries, key)
		s.misses++
		return nil, false
	}
	s.hits++
	return entry, true
}

// Put stores a detached response, evicting one arbitrary entry at capacity.
// A nil Store or empty key is a no-op. Response eligibility, including whether
// an empty body is cacheable, belongs to the caller.
func (s *Store) Put(key string, status int, header http.Header, body []byte, now time.Time) {
	if s == nil || key == "" {
		return
	}
	entry := &Entry{
		status:    status,
		header:    header.Clone(),
		body:      append([]byte(nil), body...),
		expiresAt: now.Add(s.ttl),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
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
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = map[string]*Entry{}
	s.hits = 0
	s.misses = 0
}

// Stats returns counters and entry count under one lock.
func (s *Store) Stats() Stats {
	if s == nil {
		return Stats{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return Stats{
		Hits:    s.hits,
		Misses:  s.misses,
		Entries: uint64(len(s.entries)),
	}
}

// MaxBodyBytes returns the capture limit configured for this Store.
func (s *Store) MaxBodyBytes() int {
	if s == nil {
		return 0
	}
	return s.maxBodyBytes
}
