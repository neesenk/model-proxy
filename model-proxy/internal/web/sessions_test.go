package web

import (
	"model-proxy/internal/appapi"
	"testing"
	"time"
)

func TestLoginSessionStore(t *testing.T) {
	store := newSessionStore()
	id := store.Create("codex", "https://verify.invalid")
	if len(id) != 32 {
		t.Fatalf("session ID length = %d, want 32", len(id))
	}
	update, ok := store.Snapshot(id)
	if !ok || update != (appapi.LoginUpdate{State: "pending", Detail: "https://verify.invalid"}) {
		t.Fatalf("initial snapshot = %#v, %v", update, ok)
	}
	if _, ok := store.Snapshot("nope"); ok {
		t.Fatal("unknown session unexpectedly exists")
	}

	want := appapi.LoginUpdate{State: "done", Detail: "https://verify.invalid", Result: "user@example.com", Warning: "reload later"}
	if !store.Update(id, want) {
		t.Fatal("update rejected known session")
	}
	if got, ok := store.Snapshot(id); !ok || got != want {
		t.Fatalf("updated snapshot = %#v, %v; want %#v", got, ok, want)
	}
	if store.Update("nope", want) {
		t.Fatal("update accepted unknown session")
	}

	store.GC()
	if _, ok := store.Snapshot(id); !ok {
		t.Fatal("fresh session GC'd prematurely")
	}
}

func TestLoginSessionGC(t *testing.T) {
	store := newSessionStore()
	id := store.Create("aqp", "")
	store.mu.RLock()
	session := store.sessions[id]
	store.mu.RUnlock()
	session.created = time.Now().Add(-20 * time.Minute)

	store.GC()
	if _, ok := store.Snapshot(id); !ok {
		t.Fatal("stale pending session was GC'd while its poll may still be running")
	}
	if !store.Update(id, appapi.LoginUpdate{State: "error", Detail: "poll timed out"}) {
		t.Fatal("update rejected stale session")
	}
	store.GC()
	if _, ok := store.Snapshot(id); ok {
		t.Fatal("stale terminal session was not GC'd")
	}
}
