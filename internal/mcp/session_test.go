package mcp

import (
	"testing"
	"time"
)

// TestSessionTable_RefreshUpstream: a stateful upstream that rotates its own
// Mcp-Session-Id on re-initialize must have the binding refreshed so later
// requests address the new id.
func TestSessionTable_RefreshUpstream(t *testing.T) {
	tbl := NewSessionTable(4, time.Minute)
	id := tbl.Put("srv", "acct", "up-1")

	tbl.RefreshUpstream(id, "up-2")
	s, ok := tbl.Get(id)
	if !ok || s.UpstreamID != "up-2" {
		t.Fatalf("after refresh: s=%+v ok=%v", s, ok)
	}

	// Refreshing again overwrites.
	tbl.RefreshUpstream(id, "up-3")
	s, ok = tbl.Get(id)
	if !ok || s.UpstreamID != "up-3" {
		t.Fatalf("after second refresh: s=%+v ok=%v", s, ok)
	}
}

// TestSessionTable_RefreshUpstream_Expired: refreshing an expired session is a
// no-op and the entry is dropped.
func TestSessionTable_RefreshUpstream_Expired(t *testing.T) {
	tbl := NewSessionTable(4, time.Minute)
	now := time.Now()
	tbl.now = func() time.Time { return now }
	id := tbl.Put("srv", "acct", "up-1")

	now = now.Add(2 * time.Minute)
	tbl.RefreshUpstream(id, "up-2")
	if _, ok := tbl.Get(id); ok {
		t.Fatal("expired session refreshed and served")
	}
	if tbl.Len() != 0 {
		t.Fatalf("expired entry not dropped, Len = %d", tbl.Len())
	}
}

// TestSessionTable_RefreshUpstream_Unknown: refreshing an unknown id is a
// no-op and must not create a phantom session.
func TestSessionTable_RefreshUpstream_Unknown(t *testing.T) {
	tbl := NewSessionTable(4, time.Minute)
	tbl.RefreshUpstream("ghost-id", "up-2")
	if tbl.Len() != 0 {
		t.Fatalf("unknown id created a session, Len = %d", tbl.Len())
	}
}
