package main

import (
	"testing"
	"time"
)

func TestLoginSessionStore(t *testing.T) {
	s := newLoginSessionStore()
	a := s.create("codex")
	if a.id == "" || a.provider != "codex" {
		t.Fatalf("bad session: %+v", a)
	}
	if got, ok := s.get(a.id); !ok || got != a {
		t.Errorf("get returned %v,%v", got, ok)
	}
	if _, ok := s.get("nope"); ok {
		t.Errorf("get unknown should miss")
	}
	a.setState("done", "user@example.com")
	if a.state != "done" || a.result != "user@example.com" {
		t.Errorf("setState didn't apply: %+v", a)
	}
	// GC drops nothing fresh.
	s.gc()
	if _, ok := s.get(a.id); !ok {
		t.Errorf("fresh session GC'd prematurely")
	}
}

func TestLoginSessionGC(t *testing.T) {
	s := newLoginSessionStore()
	old := s.create("aqp")
	old.created = time.Now().Add(-20 * time.Minute) // older than TTL
	s.gc()
	if _, ok := s.get(old.id); ok {
		t.Errorf("stale session not GC'd")
	}
}
