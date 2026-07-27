package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"sync"
	"time"
)

// cache.go implements an EXACT-MATCH response cache (prompt-hash + TTL): when a
// request's (method, path, body) was served recently, the cached upstream
// response is replayed verbatim with no upstream call. Targets Claude Code-style
// workloads where identical requests recur (retries, idempotent calls, repeated
// heavy system prompts). Off by default.
//
// The cache stores the raw response bytes the upstream returned (status code +
// headers + body, SSE included), keyed by SHA-256 of the request line + body. A
// hit replays those exact bytes — byte-identical to a fresh response, including
// SSE event streams. Bounded by max_entries (size cap, random eviction) and
// max_body_bytes (responses larger than this stream without being cached).

// CacheConfig configures the exact-match response cache.
type CacheConfig struct {
	Enabled      bool   `yaml:"enabled"`
	TTL          string `yaml:"ttl"`            // entry expiry (default 10m)
	MaxEntries   int    `yaml:"max_entries"`    // size cap (default 1000)
	MaxBodyBytes int    `yaml:"max_body_bytes"` // cache only responses ≤ this (default 256KiB)
}

func (c CacheConfig) enabled() bool { return c.Enabled }

func (c CacheConfig) ttl() time.Duration {
	if c.TTL == "" {
		return 10 * time.Minute
	}
	if d, err := time.ParseDuration(c.TTL); err == nil {
		return d
	}
	return 10 * time.Minute
}

func (c CacheConfig) maxEntries() int {
	if c.MaxEntries > 0 {
		return c.MaxEntries
	}
	return 1000
}

func (c CacheConfig) maxBody() int {
	if c.MaxBodyBytes > 0 {
		return c.MaxBodyBytes
	}
	return 256 * 1024
}

// cacheEntry is one cached response. header is the upstream's full header set
// (content-type etc.); body is the raw bytes to replay.
type cacheEntry struct {
	status    int
	header    http.Header
	body      []byte
	expiresAt time.Time
}

// responseCache is the in-memory exact-match store. Nil-safe: every method
// handles a nil receiver (a no-op), so the forward path calls unconditionally.
type responseCache struct {
	mu         sync.Mutex
	m          map[string]*cacheEntry
	ttl        time.Duration
	maxEntries int
	maxBody    int
	hits       uint64
	misses     uint64
}

func newResponseCache(cfg CacheConfig) *responseCache {
	if !cfg.enabled() {
		return nil
	}
	return &responseCache{
		m:          map[string]*cacheEntry{},
		ttl:        cfg.ttl(),
		maxEntries: cfg.maxEntries(),
		maxBody:    cfg.maxBody(),
	}
}

// get returns a live entry (not expired). Expired entries are lazily evicted.
// Nil-safe: a nil cache always misses.
func (c *responseCache) get(key string, now time.Time) (*cacheEntry, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok {
		c.misses++
		return nil, false
	}
	if now.After(e.expiresAt) {
		delete(c.m, key)
		c.misses++
		return nil, false
	}
	c.hits++
	return e, true
}

// put stores an entry, evicting randomly when at capacity. Nil-safe.
func (c *responseCache) put(key string, e *cacheEntry, now time.Time) {
	if c == nil || key == "" || e == nil {
		return
	}
	e.expiresAt = now.Add(c.ttl)
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) >= c.maxEntries {
		// Size cap: drop one arbitrary entry (map iteration order is randomized,
		// so this is a cheap ~random eviction without an LRU structure).
		for k := range c.m {
			delete(c.m, k)
			break
		}
	}
	c.m[key] = e
}

func (c *responseCache) reset() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m = map[string]*cacheEntry{}
	c.hits, c.misses = 0, 0
}

// stats returns (hits, misses, entries) under one lock, for /api/status observability.
func (c *responseCache) stats() (hits, misses, entries uint64) {
	if c == nil {
		return 0, 0, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, c.misses, uint64(len(c.m))
}

// cacheKeyHeaders are request headers that can change the response and so must
// be part of the exact-match key — otherwise two requests differing only in one
// of these could collide and return the wrong cached response. Canonical names;
// r.Header.Get canonicalizes, so client casing doesn't matter.
var cacheKeyHeaders = []string{"anthropic-beta", "accept-language"}

// cacheKeyOf returns the exact-match key for a request: SHA-256 of the method,
// path, raw query, the response-affecting headers, and the full request body.
// The body carries the model + messages + system + tools; the query string and
// these headers (anthropic-beta output features, accept-language) can each
// change the response, so two requests cache-share only when ALL of these match.
func cacheKeyOf(r *http.Request, body []byte) string {
	h := sha256.New()
	h.Write([]byte(r.Method))
	h.Write([]byte{0})
	h.Write([]byte(r.URL.Path))
	h.Write([]byte{0})
	h.Write([]byte(r.URL.RawQuery))
	h.Write([]byte{0})
	for _, name := range cacheKeyHeaders {
		if v := r.Header.Get(name); v != "" {
			h.Write([]byte(name))
			h.Write([]byte{':'})
			h.Write([]byte(v))
			h.Write([]byte{0})
		}
	}
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// cacheRecorder is a pass-through io.ReadCloser that tees the bytes read into a
// bounded buffer for the cache. truncated=true if the body exceeded the cap; the
// entry is then NOT cached (replaying a truncated body would corrupt the
// response). sawEOF=true only when the source reached a clean EOF — a client
// disconnect mid-stream leaves it false, so a half-read body is NOT cached as
// complete. The caller reads buf after Close.
type cacheRecorder struct {
	src       io.ReadCloser
	buf       []byte
	max       int
	truncated bool
	sawEOF    bool
}

func newCacheRecorder(src io.ReadCloser, max int) *cacheRecorder {
	return &cacheRecorder{src: src, max: max}
}

func (c *cacheRecorder) Read(p []byte) (int, error) {
	n, err := c.src.Read(p)
	if err == io.EOF {
		c.sawEOF = true // clean end of stream — the body is complete
	}
	if n > 0 && !c.truncated {
		if len(c.buf)+n > c.max {
			nfit := c.max - len(c.buf)
			if nfit > 0 {
				c.buf = append(c.buf, p[:nfit]...)
			}
			c.truncated = true // exceeded cap → this response is uncachable
		} else {
			c.buf = append(c.buf, p[:n]...)
		}
	}
	return n, err
}

func (c *cacheRecorder) Close() error { return c.src.Close() }

// cachedResponseHeader returns the header set to store with a cached entry:
// the upstream headers adjusted to match the CLIENT-facing body the recorder
// captured. A converted body has a different length than the backend's, so the
// backend's Content-Length / Transfer-Encoding must not be cached (Go's server
// re-derives the length on replay). A mode-mismatch rewrite changed the
// content-type the live path sent (e.g. a JSON upstream body replayed to the
// client as SSE), so the cached header must carry THAT content-type — otherwise
// a replay would pair an SSE body with the upstream's application/json.
func cachedResponseHeader(h http.Header, convert, modeMismatch, clientWantsStream bool) http.Header {
	hdr := h.Clone()
	if convert {
		hdr.Del("Content-Length")
		hdr.Del("Transfer-Encoding")
	}
	if modeMismatch {
		if clientWantsStream {
			hdr.Set("Content-Type", "text/event-stream")
		} else {
			hdr.Set("Content-Type", "application/json")
		}
	}
	return hdr
}

// replayCached writes a cached entry to the client verbatim: status + the stored
// headers, then the body streamed (flushCopy so SSE chunks flush). Used on a
// cache hit, before any upstream call.
func replayCached(w http.ResponseWriter, e *cacheEntry) {
	for k, vs := range e.header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(e.status)
	flushCopy(w, io.NopCloser(bytes.NewReader(e.body)))
}
