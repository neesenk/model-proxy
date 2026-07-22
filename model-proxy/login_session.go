package main

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// loginSessionTTL is how long an in-flight login session is kept before GC
// drops it. Async aqp/codex logins should complete well within this window;
// the value matches the brief's spec (15 min).
const loginSessionTTL = 15 * time.Minute

// loginSession holds the in-flight state of one async login (aqp SSO poll or
// codex device-flow poll). Tasks 14/15 populate aqpClient / codex; until then
// they stay nil. Each session has its own mutex so setState (the polling
// goroutine writing the result) doesn't take the store lock.
type loginSession struct {
	id        string
	provider  string
	state     string // "pending" | "done" | "error"
	detail    string // verify_url+user_code (codex) / login_url (aqp) / error msg
	result    string // email or account id on done
	warning   string // non-fatal warning surfaced via poll (e.g. reload failed post-login)
	created   time.Time
	aqpClient *AqpClient       // aqp
	codex     *codexLoginState // codex
	mu        sync.Mutex
}

// codexLoginState carries the per-session codex device-flow state between the
// POST that started it and the polling goroutine that completes it.
type codexLoginState struct {
	deviceAuthID string
	userCode     string
	interval     int
	opts         *codexLoginServerOptions
}

// setState updates the session's state (and result when non-empty) under the
// session's own mutex — callers may pass "" to leave result untouched.
func (s *loginSession) setState(state, result string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = state
	if result != "" {
		s.result = result
	}
}

// setWarning records a non-fatal warning surfaced via the poll response — e.g.
// the in-process reload failed after a successful async login, so credentials
// are persisted but the runtime keeps the old set until config.yaml is fixed +
// reloaded. The login itself still reports "done"; this lets the UI warn the
// user rather than show a false success. Under the session mutex.
func (s *loginSession) setWarning(warning string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.warning = warning
}

// loginSessionStore is the in-memory map of id → session. It is safe for
// concurrent use; the store mutex guards the map only (session fields are
// guarded by each session's own mutex).
type loginSessionStore struct {
	mu       sync.Mutex
	sessions map[string]*loginSession
}

func newLoginSessionStore() *loginSessionStore {
	return &loginSessionStore{sessions: map[string]*loginSession{}}
}

// newSessionID returns a 32-char hex string from 16 bytes of crypto/rand.
func newSessionID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand.Read failing is catastrophic — panic so we notice.
		panic("crypto/rand.Read: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// create allocates a new pending session, stores it, and returns it.
func (s *loginSessionStore) create(provider string) *loginSession {
	sess := &loginSession{
		id:       newSessionID(),
		provider: provider,
		state:    "pending",
		created:  time.Now(),
	}
	s.mu.Lock()
	s.sessions[sess.id] = sess
	s.mu.Unlock()
	return sess
}

// get returns the session for id (and ok=false if missing).
func (s *loginSessionStore) get(id string) (*loginSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	return sess, ok
}

// gc drops sessions older than loginSessionTTL. Run periodically (webGC).
func (s *loginSessionStore) gc() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, sess := range s.sessions {
		if time.Since(sess.created) > loginSessionTTL {
			delete(s.sessions, id)
		}
	}
}
