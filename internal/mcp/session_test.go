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

// TestRouteSubPutAfterSessionRemoval: once the route session is gone (LRU
// eviction or DELETE), a late initialize's put MUST report false — the app
// layer relies on exactly this verdict to kill the stdio child it just
// registered (an eviction fired earlier against a registry without the key,
// so a lost-put child would otherwise never be killed).
func TestRouteSubPutAfterSessionRemoval(t *testing.T) {
	tbl := NewSessionTable(8, time.Hour)
	id := tbl.Put("srv", "acct", "")
	tbl.Delete(id)
	if tbl.RouteSubPut(id, SubSession{Server: "srv", Initialized: true}) {
		t.Fatal("RouteSubPut succeeded for a removed session — late stdio initialize would leak its child")
	}
}

// TestSessionTable_OnEvictOutsideLock: every removal path (explicit delete,
// LRU eviction, lazy expiry) must fire OnEvict with the table lock released.
// The production callback reaches into the stdio registry, where Close blocks
// on child reap — holding t.mu across it would stall every session operation
// behind process teardown. The callback here re-enters the same table through
// lock-taking methods (Len/ServerCounts/Get/Delete), the strictest probe of
// "fired after Unlock": a regression that fires under the lock wedges the
// trigger goroutine and the bounded select fails the test instead of hanging
// the suite.
func TestSessionTable_OnEvictOutsideLock(t *testing.T) {
	const deadline = 5 * time.Second

	type fire struct {
		session  Session
		stillSet bool // Get(session.ID) found it while the callback ran
	}

	setup := func(maxSessions int, ttl time.Duration) (*SessionTable, <-chan fire) {
		tbl := NewSessionTable(maxSessions, ttl)
		fired := make(chan fire, 16)
		tbl.SetOnEvict(func(s Session) {
			// Lock-taking re-entry: deadlocks here if the fire happens under t.mu.
			_ = tbl.Len()
			_ = tbl.ServerCounts()
			_, stillSet := tbl.Get(s.ID)
			tbl.Delete(s.ID) // already removed by notification time — no-op
			fired <- fire{session: s, stillSet: stillSet}
		})
		return tbl, fired
	}

	// waitFired runs trigger and collects want fires; a fire that never
	// completes within the deadline means the callback wedged on the lock.
	waitFired := func(t *testing.T, fired <-chan fire, want int, trigger func()) []fire {
		t.Helper()
		go trigger()
		got := make([]fire, 0, want)
		for len(got) < want {
			select {
			case f := <-fired:
				got = append(got, f)
			case <-time.After(deadline):
				t.Fatalf("OnEvict %d/%d fires after %v — callback wedged (fired under the table lock?)", len(got), want, deadline)
			}
		}
		return got
	}

	t.Run("explicit delete", func(t *testing.T) {
		tbl, fired := setup(8, time.Hour)
		victim := tbl.Put("srvA", "acct", "up")
		got := waitFired(t, fired, 1, func() { tbl.Delete(victim) })
		if got[0].session.ID != victim || got[0].session.Server != "srvA" {
			t.Fatalf("evicted session = %+v", got[0].session)
		}
		if got[0].stillSet {
			t.Fatal("evicted session still reachable from the callback")
		}
	})

	t.Run("lru eviction", func(t *testing.T) {
		tbl, fired := setup(1, time.Hour)
		victim := tbl.Put("srvOld", "acct", "u1")
		got := waitFired(t, fired, 1, func() { tbl.Put("srvNew", "acct", "u2") })
		if got[0].session.ID != victim {
			t.Fatalf("lru victim = %+v, want %s", got[0].session, victim)
		}
		if got[0].stillSet {
			t.Fatal("lru-evicted session still reachable from the callback")
		}
	})

	t.Run("lazy expiry", func(t *testing.T) {
		tbl, fired := setup(8, time.Minute)
		now := time.Now()
		tbl.now = func() time.Time { return now }
		victim := tbl.Put("srvExp", "acct", "up")
		now = now.Add(2 * time.Minute)
		got := waitFired(t, fired, 1, func() { tbl.Get(victim) })
		if got[0].session.ID != victim {
			t.Fatalf("expired session = %+v, want %s", got[0].session, victim)
		}
		if got[0].stillSet {
			t.Fatal("expired session still reachable from the callback")
		}
	})
}

// TestSessionTable_ServerCounts: the /api/mcp session gauge groups entries by
// server name (route sessions under their route name) and includes
// expired-but-unswept entries — lazy expiry keeps the table goroutine-free, so
// the gauge is advisory until an access sweeps the entry.
func TestSessionTable_ServerCounts(t *testing.T) {
	tbl := NewSessionTable(8, time.Minute)
	now := time.Now()
	tbl.now = func() time.Time { return now }

	if got := tbl.ServerCounts(); len(got) != 0 {
		t.Fatalf("empty table counts = %v", got)
	}

	a := tbl.Put("srvA", "acct", "u1")
	b := tbl.Put("srvA", "acct", "u2")
	c := tbl.PutRoute("routeB")
	counts := tbl.ServerCounts()
	if len(counts) != 2 || counts["srvA"] != 2 || counts["routeB"] != 1 {
		t.Fatalf("counts = %v", counts)
	}

	// TTL elapsed but nothing accessed the entries: they are still counted.
	now = now.Add(2 * time.Minute)
	counts = tbl.ServerCounts()
	if counts["srvA"] != 2 || counts["routeB"] != 1 {
		t.Fatalf("expired-but-unswept counts = %v, want both servers still counted", counts)
	}

	// Get sweeps the expired entry; the gauge then drops it.
	if _, ok := tbl.Get(a); ok {
		t.Fatal("expired session served")
	}
	if counts = tbl.ServerCounts(); counts["srvA"] != 1 || counts["routeB"] != 1 {
		t.Fatalf("counts after sweep = %v", counts)
	}

	tbl.Delete(b)
	tbl.Delete(c)
	if counts = tbl.ServerCounts(); len(counts) != 0 {
		t.Fatalf("counts after deletes = %v", counts)
	}
}
