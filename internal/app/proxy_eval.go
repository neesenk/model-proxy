package app

import (
	"container/list"
	"strings"
	"sync"

	configdomain "model-proxy/internal/config"
)

const (
	// evalBodyCacheMaxEntries bounds the number of sampled primary bodies held
	// in memory between CaptureResponse and eval dispatch. A small cap is safe
	// because dispatch removes the entry immediately after retrieval.
	evalBodyCacheMaxEntries = 128
	// evalBodyCacheMaxBytes bounds the total memory of the cache. Individual
	// bodies are additionally capped at the request log max_body_bytes value.
	evalBodyCacheMaxBytes = 64 << 20
)

// evalBodyCache holds a bounded set of primary response bodies sampled for
// route_policy eval shadow dispatch. Entries are keyed by request id and
// removed on retrieval; an LRU evicts stale entries when the cache is full.
// This keeps response text in memory only for the short window between commit
// and eval dispatch, and only for sampled requests.
type evalBodyCache struct {
	mu         sync.Mutex
	entries    map[string]*list.Element
	lru        *list.List
	maxEntries int
	maxBytes   int
	bytes      int
}

type evalBodyEntry struct {
	key  string
	body []byte
}

func newEvalBodyCache(maxEntries, maxBytes int) *evalBodyCache {
	return &evalBodyCache{
		entries:    make(map[string]*list.Element),
		lru:        list.New(),
		maxEntries: maxEntries,
		maxBytes:   maxBytes,
	}
}

// Store saves a sampled primary response body. It refuses shadow/judge request
// ids and rejects bodies larger than the cache byte cap. Returns false if the
// value was not stored.
func (c *evalBodyCache) Store(requestID string, body []byte) bool {
	if c == nil || isEvalRecursiveID(requestID) || len(body) == 0 || len(body) > c.maxBytes {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[requestID]; ok {
		old := el.Value.(*evalBodyEntry)
		c.bytes -= len(old.body)
		old.body = body
		c.bytes += len(body)
		c.lru.MoveToFront(el)
		return true
	}
	for c.bytes+len(body) > c.maxBytes && c.lru.Len() > 0 {
		c.evictBack()
	}
	for c.lru.Len() >= c.maxEntries {
		c.evictBack()
	}
	ent := &evalBodyEntry{key: requestID, body: body}
	c.entries[requestID] = c.lru.PushFront(ent)
	c.bytes += len(body)
	return true
}

// Retrieve removes and returns a previously stored body. Returns nil if absent.
func (c *evalBodyCache) Retrieve(requestID string) []byte {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[requestID]
	if !ok {
		return nil
	}
	ent := el.Value.(*evalBodyEntry)
	delete(c.entries, requestID)
	c.lru.Remove(el)
	c.bytes -= len(ent.body)
	return ent.body
}

func (c *evalBodyCache) evictBack() {
	back := c.lru.Back()
	if back == nil {
		return
	}
	ent := back.Value.(*evalBodyEntry)
	delete(c.entries, ent.key)
	c.lru.Remove(back)
	c.bytes -= len(ent.body)
}

// isEvalRecursiveID reports ids that must never trigger eval sampling: shadow
// records and eval-judge legs themselves.
func isEvalRecursiveID(requestID string) bool {
	return strings.HasPrefix(requestID, "shadow-") || strings.HasPrefix(requestID, "eval-judge-")
}

// evalConfiguredForRoute reports whether the route has route_policy eval
// configured in cfg — where cfg must be the REQUEST's runtime snapshot
// config, so the body-storage decision and the post-commit sampling decision
// (dispatchEvalShadow, same snapshot) can never diverge across a reload.
func evalConfiguredForRoute(cfg *configdomain.Config, exposed string) bool {
	if cfg == nil {
		return false
	}
	policy, ok := cfg.RoutePolicies[exposed]
	return ok && policy.Eval != nil
}

// maybeStoreEvalPrimaryBody stores a captured primary response body when the
// request's own snapshot configures eval for the route (evalConfigured is
// computed on the request path via evalConfiguredForRoute — this post-commit
// bodycapture callback must never re-read reload-owned config: a reload that
// drops eval between commit and callback would otherwise let the snapshot
// side sample while the body was never stored, silently losing the sample).
// Sampling itself happens later in dispatchShadowAfterCommit using the same
// request snapshot; here we just make the body available. This keeps the
// sampling decision on the snapshot while avoiding an extra copy of the
// response bytes.
func (p *Proxy) maybeStoreEvalPrimaryBody(requestID string, evalConfigured bool, body []byte) {
	if p == nil || p.evalPrimaryBodies == nil || isEvalRecursiveID(requestID) || len(body) == 0 || !evalConfigured {
		return
	}
	p.evalPrimaryBodies.Store(requestID, body)
}
