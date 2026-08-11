package web

import (
	"crypto/rand"
	"encoding/hex"
	"model-proxy/internal/appapi"
	"sync"
	"time"
)

const loginSessionTTL = 15 * time.Minute

// sessionStore keeps detached, transport-visible login updates. It does not
// retain provider clients or application credentials.
type sessionStore struct {
	mu       sync.RWMutex
	sessions map[string]*loginSession
}

type loginSession struct {
	mu       sync.RWMutex
	provider string
	update   appapi.LoginUpdate
	created  time.Time
}

func newSessionStore() *sessionStore {
	return &sessionStore{sessions: make(map[string]*loginSession)}
}

func newSessionID() string {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		panic("crypto/rand.Read: " + err.Error())
	}
	return hex.EncodeToString(bytes)
}

// Create stores one pending login session and returns its opaque identifier.
func (store *sessionStore) Create(provider, detail string) string {
	if store == nil {
		return ""
	}
	id := newSessionID()
	store.mu.Lock()
	store.sessions[id] = &loginSession{
		provider: provider,
		update:   appapi.LoginUpdate{State: "pending", Detail: detail},
		created:  time.Now(),
	}
	store.mu.Unlock()
	return id
}

// Update replaces the transport state for an existing session. It reports
// false for unknown or already-collected sessions.
func (store *sessionStore) Update(id string, update appapi.LoginUpdate) bool {
	session, ok := store.session(id)
	if !ok {
		return false
	}
	session.mu.Lock()
	session.update = update
	session.mu.Unlock()
	return true
}

// Snapshot returns a detached point-in-time copy of one session update.
func (store *sessionStore) Snapshot(id string) (appapi.LoginUpdate, bool) {
	session, ok := store.session(id)
	if !ok {
		return appapi.LoginUpdate{}, false
	}
	session.mu.RLock()
	update := session.update
	session.mu.RUnlock()
	return update, true
}

func (store *sessionStore) session(id string) (*loginSession, bool) {
	if store == nil {
		return nil, false
	}
	store.mu.RLock()
	session, ok := store.sessions[id]
	store.mu.RUnlock()
	return session, ok
}

// GC drops only stale terminal sessions. Pending sessions remain visible until
// their owned poll completes, even if it outlives the normal TTL.
func (store *sessionStore) GC() {
	if store == nil {
		return
	}
	now := time.Now()
	store.mu.Lock()
	defer store.mu.Unlock()
	for id, session := range store.sessions {
		if now.Sub(session.created) <= loginSessionTTL {
			continue
		}
		session.mu.RLock()
		terminal := session.update.State == "done" || session.update.State == "error"
		session.mu.RUnlock()
		if terminal {
			delete(store.sessions, id)
		}
	}
}
