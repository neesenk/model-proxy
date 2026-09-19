package mcp

import (
	"container/list"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// Session binds one local (client-facing) MCP session to its upstream facts:
// the gateway server name, the provider account the session is pinned to, and
// the upstream Mcp-Session-Id. Sessions exist only for stateful upstreams —
// stateless servers (no upstream session id) never create entries.
type Session struct {
	// ID is the local session id (the table key), set at Put/PutRoute time.
	// Eviction callbacks key on it.
	ID         string
	Server     string // mcp server name (config key under mcp:)
	Account    string // virtual provider id pinned for the session ("" for auth: none)
	UpstreamID string // upstream Mcp-Session-Id (legacy sse: the full upstream POST URL)
	// Client is the initialize-time clientInfo.name bound to this session
	// ("" when the client declared none): the request-log attribution for
	// follow-up exchanges of stateful sessions — later requests carry only
	// the session id, not their identity.
	Client   string
	Created  time.Time
	LastSeen time.Time
	// Route is non-nil for route (mcp_routes:) sessions: the proxy owns the
	// client session and lazily builds per-backend sub-sessions. Pinned
	// passthrough sessions leave it nil and use Account/UpstreamID above.
	// Get never exposes it (route state is only reachable through the
	// RouteXxx table methods, which serialize under the table lock).
	Route *RouteState
}

// SubSession is one backend's session state inside a route session.
type SubSession struct {
	Server      string // mcp: server name
	Account     string // virtual provider id bound at sub initialize
	UpstreamID  string // upstream Mcp-Session-Id ("" when the backend is stateless or stdio)
	Protocol    string // protocolVersion negotiated with this backend ("stdio" for stdio backends)
	Initialized bool   // the backend handshake completed
}

// RouteState holds the mutable per-route-session facts. All access goes
// through SessionTable's RouteXxx methods (copy semantics under the table
// lock) — concurrent tools/calls of one client session never touch these maps
// directly.
type RouteState struct {
	Subs       map[string]SubSession // server name → backend session (lazy)
	Sticky     map[string]string     // canonical tool → server name (session stickiness)
	Tools      []ToolSpec            // aggregated tools/list cache for this session
	ToolsReady bool                  // Tools was aggregated at least once
}

// SessionTable is a bounded LRU of live MCP sessions with an idle TTL. It is
// process-lifetime state that survives config reloads (sessions fail closed
// later if their account disappears from a new generation). Expiry and
// eviction are LAZY — evaluated on access, so the table owns no goroutine and
// needs no lifecycle admission (precedent: internal/guard/session).
//
// Every removal (expiry, explicit delete, LRU eviction) is reported through
// the optional OnEvict callback, fired OUTSIDE the table lock — the app uses
// it to kill stdio child processes bound to dying sessions.
type SessionTable struct {
	mu       sync.Mutex
	max      int
	ttl      time.Duration
	entries  map[string]*list.Element
	lru      *list.List // front = most recently used; Value = local session id
	sessions map[string]Session
	onEvict  func(Session)
	now      func() time.Time // test seam
}

// NewSessionTable creates a table holding at most max sessions, expiring
// entries idle for longer than ttl.
func NewSessionTable(maxSessions int, ttl time.Duration) *SessionTable {
	if maxSessions <= 0 {
		maxSessions = 512
	}
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	return &SessionTable{
		max:      maxSessions,
		ttl:      ttl,
		entries:  map[string]*list.Element{},
		lru:      list.New(),
		sessions: map[string]Session{},
		now:      time.Now,
	}
}

// SetOnEvict registers the removal callback. Must be set before first use
// (the app wires it at construction); not safe to swap concurrently.
func (t *SessionTable) SetOnEvict(fn func(Session)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.onEvict = fn
}

// fireEvicted invokes the callback outside the lock for one removal batch.
func (t *SessionTable) fireEvicted(evicted []Session) {
	if t.onEvict == nil {
		return
	}
	for _, s := range evicted {
		t.onEvict(s)
	}
}

// Get returns the session for a local id, refreshing its LRU position and
// LastSeen. Expired or unknown ids report ok=false (expired ones are dropped
// and reported to OnEvict). Route state is never exposed here.
func (t *SessionTable) Get(id string) (Session, bool) {
	t.mu.Lock()
	var evicted []Session
	el, ok := t.entries[id]
	if !ok {
		t.mu.Unlock()
		return Session{}, false
	}
	s := t.sessions[id]
	if t.expired(s) {
		evicted = t.removeLocked(id, el)
		t.mu.Unlock()
		t.fireEvicted(evicted)
		return Session{}, false
	}
	s.LastSeen = t.now()
	t.sessions[id] = s
	t.lru.MoveToFront(el)
	t.mu.Unlock()
	s.Route = nil // route state is only reachable through the RouteXxx methods
	return s, true
}

// Put mints a fresh local session id and records the binding. Evicts the
// least recently used entry when the table is full.
func (t *SessionTable) Put(server, account, upstreamID string) string {
	id := newSessionID()
	t.mu.Lock()
	evicted := t.evictLocked()
	now := t.now()
	t.sessions[id] = Session{ID: id, Server: server, Account: account, UpstreamID: upstreamID, Created: now, LastSeen: now}
	t.entries[id] = t.lru.PushFront(id)
	t.mu.Unlock()
	t.fireEvicted(evicted)
	return id
}

// PutRoute mints a fresh local session id for a route session (proxy-owned,
// per-backend sub-sessions built lazily).
func (t *SessionTable) PutRoute(route string) string {
	id := newSessionID()
	t.mu.Lock()
	evicted := t.evictLocked()
	now := t.now()
	t.sessions[id] = Session{
		ID:       id,
		Server:   route,
		Created:  now,
		LastSeen: now,
		Route: &RouteState{
			Subs:   map[string]SubSession{},
			Sticky: map[string]string{},
		},
	}
	t.entries[id] = t.lru.PushFront(id)
	t.mu.Unlock()
	t.fireEvicted(evicted)
	return id
}

// SetClient binds the initialize-time clientInfo.name onto a live session
// (client attribution for later session-carried requests). Unknown or
// expired ids are a no-op; the LRU position/LastSeen are untouched.
func (t *SessionTable) SetClient(id, client string) {
	if client == "" {
		return
	}
	t.mu.Lock()
	if _, ok := t.entries[id]; ok && !t.expired(t.sessions[id]) {
		s := t.sessions[id]
		s.Client = client
		t.sessions[id] = s
	}
	t.mu.Unlock()
}

// RefreshUpstream rebinds a live session's upstream session id: a stateful
// upstream that re-issues its Mcp-Session-Id on re-initialize (id rotation)
// must not leave later requests addressing the stale id (404s). Unknown or
// expired ids are a no-op; the LRU position/LastSeen are untouched.
func (t *SessionTable) RefreshUpstream(id, upstreamID string) {
	t.mu.Lock()
	if _, ok := t.entries[id]; ok && !t.expired(t.sessions[id]) {
		s := t.sessions[id]
		s.UpstreamID = upstreamID
		t.sessions[id] = s
	}
	t.mu.Unlock()
}

// routeSessionLockedE resolves a live route session, refreshing LRU/LastSeen
// like Get. Pinned-passthrough sessions (Route == nil) and unknown ids report
// ok=false; expired sessions are removed and returned in evicted. Caller must
// hold t.mu and fire evicted after unlocking.
func (t *SessionTable) routeSessionLockedE(id string) (s Session, evicted []Session, ok bool) {
	el, exists := t.entries[id]
	if !exists {
		return Session{}, nil, false
	}
	s = t.sessions[id]
	if t.expired(s) {
		return Session{}, t.removeLocked(id, el), false
	}
	if s.Route == nil {
		return Session{}, nil, false
	}
	s.LastSeen = t.now()
	t.sessions[id] = s
	t.lru.MoveToFront(el)
	return s, nil, true
}

// RouteSessionValid reports whether id is a live route session (also
// refreshing its LRU position/LastSeen like Get).
func (t *SessionTable) RouteSessionValid(id string) bool {
	t.mu.Lock()
	_, evicted, ok := t.routeSessionLockedE(id)
	t.mu.Unlock()
	t.fireEvicted(evicted)
	return ok
}

// RouteSubGet returns a copy of one backend sub-session.
func (t *SessionTable) RouteSubGet(id, server string) (SubSession, bool) {
	t.mu.Lock()
	s, evicted, ok := t.routeSessionLockedE(id)
	var sub SubSession
	if ok {
		// The Route maps are shared by every goroutine serving this session —
		// all reads/writes stay UNDER the table lock (lock-held copy
		// semantics); only the eviction callback fires after Unlock.
		sub, ok = s.Route.Subs[server]
	}
	t.mu.Unlock()
	t.fireEvicted(evicted)
	return sub, ok
}

// RouteSubPut upserts one backend sub-session. Reports false when the route
// session no longer exists.
func (t *SessionTable) RouteSubPut(id string, sub SubSession) bool {
	t.mu.Lock()
	s, evicted, ok := t.routeSessionLockedE(id)
	if ok {
		s.Route.Subs[sub.Server] = sub
	}
	t.mu.Unlock()
	t.fireEvicted(evicted)
	return ok
}

// RouteSubDrop removes one backend sub-session (session-invalid failover).
func (t *SessionTable) RouteSubDrop(id, server string) {
	t.mu.Lock()
	s, evicted, ok := t.routeSessionLockedE(id)
	if ok {
		delete(s.Route.Subs, server)
	}
	t.mu.Unlock()
	t.fireEvicted(evicted)
}

// RouteSubs returns copies of all backend sub-sessions (DELETE fan-out).
func (t *SessionTable) RouteSubs(id string) []SubSession {
	t.mu.Lock()
	s, evicted, ok := t.routeSessionLockedE(id)
	var out []SubSession
	if ok {
		out = make([]SubSession, 0, len(s.Route.Subs))
		for _, sub := range s.Route.Subs {
			out = append(out, sub)
		}
	}
	t.mu.Unlock()
	t.fireEvicted(evicted)
	return out
}

// RouteStickyGet returns the sticky backend for one canonical tool.
func (t *SessionTable) RouteStickyGet(id, canonical string) (string, bool) {
	t.mu.Lock()
	s, evicted, ok := t.routeSessionLockedE(id)
	var server string
	if ok {
		server, ok = s.Route.Sticky[canonical]
	}
	t.mu.Unlock()
	t.fireEvicted(evicted)
	return server, ok
}

// RouteStickyPut pins one canonical tool to a backend for this session.
func (t *SessionTable) RouteStickyPut(id, canonical, server string) {
	t.mu.Lock()
	s, evicted, ok := t.routeSessionLockedE(id)
	if ok {
		s.Route.Sticky[canonical] = server
	}
	t.mu.Unlock()
	t.fireEvicted(evicted)
}

// RouteToolsGet returns the session's aggregated tools cache.
func (t *SessionTable) RouteToolsGet(id string) ([]ToolSpec, bool) {
	t.mu.Lock()
	s, evicted, ok := t.routeSessionLockedE(id)
	var out []ToolSpec
	ready := ok && s.Route.ToolsReady
	if ready {
		out = make([]ToolSpec, len(s.Route.Tools))
		copy(out, s.Route.Tools)
	}
	t.mu.Unlock()
	t.fireEvicted(evicted)
	return out, ready
}

// RouteToolsPut stores the session's aggregated tools cache.
func (t *SessionTable) RouteToolsPut(id string, tools []ToolSpec) bool {
	t.mu.Lock()
	s, evicted, ok := t.routeSessionLockedE(id)
	if ok {
		s.Route.Tools = append([]ToolSpec(nil), tools...)
		s.Route.ToolsReady = true
	}
	t.mu.Unlock()
	t.fireEvicted(evicted)
	return ok
}

// Delete drops a session (client DELETE or fail-closed invalidation).
// Unknown ids are a no-op.
func (t *SessionTable) Delete(id string) {
	t.mu.Lock()
	var evicted []Session
	if el, ok := t.entries[id]; ok {
		evicted = t.removeLocked(id, el)
	}
	t.mu.Unlock()
	t.fireEvicted(evicted)
}

// Len reports the number of live entries (test/observability aid).
func (t *SessionTable) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lru.Len()
}

// ServerCounts returns live session counts keyed by server/route name — the
// /api/mcp read surface's session gauge. Expired-but-not-yet-swept entries
// are included (lazy expiry keeps the table goroutine-free; the gauge is
// advisory, not a correctness input).
func (t *SessionTable) ServerCounts() map[string]int {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := map[string]int{}
	for _, s := range t.sessions {
		out[s.Server]++
	}
	return out
}

func (t *SessionTable) expired(s Session) bool {
	return t.now().Sub(s.LastSeen) > t.ttl
}

func (t *SessionTable) evictLocked() (evicted []Session) {
	for t.lru.Len() >= t.max {
		back := t.lru.Back()
		if back == nil {
			break
		}
		evicted = append(evicted, t.removeLocked(back.Value.(string), back)...)
	}
	return evicted
}

func (t *SessionTable) removeLocked(id string, el *list.Element) []Session {
	t.lru.Remove(el)
	delete(t.entries, id)
	s, ok := t.sessions[id]
	delete(t.sessions, id)
	if !ok {
		return nil
	}
	return []Session{s}
}

// newSessionID mints a random 128-bit hex session id. crypto/rand failures
// only occur on broken platforms; there is no meaningful fallback, so panic
// is the honest behavior (same posture as other id minting in the repo).
func newSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
