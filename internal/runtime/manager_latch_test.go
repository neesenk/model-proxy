package runtime

import (
	"testing"
	"time"
)

func TestLatchSetAndGet(t *testing.T) {
	m := newTestManager(1)
	v := Latch{Target: "b/mb", Since: time.Now(), BadRuns: 1}
	if !m.SetLatch("sess", v, 1) {
		t.Fatal("SetLatch rejected same-generation write")
	}
	got, ok := m.LatchValue("sess")
	if !ok || got.Target != v.Target || got.BadRuns != v.BadRuns {
		t.Fatalf("LatchValue = %+v ok=%v, want %+v", got, ok, v)
	}
}

func TestLatchGenerationGate(t *testing.T) {
	m := newTestManager(1)
	if m.SetLatch("sess", Latch{Target: "b/mb"}, 2) {
		t.Fatal("SetLatch accepted stale generation")
	}
	if m.ClearLatch("sess", 2) {
		t.Fatal("ClearLatch accepted stale generation")
	}
	m.SetLatch("sess", Latch{Target: "b/mb"}, 1)
	if m.ClearLatch("sess", 2) {
		t.Fatal("ClearLatch accepted stale generation after set")
	}
	if _, ok := m.LatchValue("sess"); !ok {
		t.Fatal("stale-generation clear must not remove latch")
	}
}

func TestLatchClearedOnReplaceGeneration(t *testing.T) {
	m := newTestManager(1)
	m.SetLatch("sess", Latch{Target: "b/mb"}, 1)
	m.ReplaceGeneration(2)
	if _, ok := m.LatchValue("sess"); ok {
		t.Fatal("latch survived ReplaceGeneration")
	}
}

func TestLatchClear(t *testing.T) {
	m := newTestManager(1)
	m.SetLatch("sess", Latch{Target: "b/mb"}, 1)
	if !m.ClearLatch("sess", 1) {
		t.Fatal("ClearLatch rejected same-generation clear")
	}
	if _, ok := m.LatchValue("sess"); ok {
		t.Fatal("latch still present after clear")
	}
}

func TestCheckRepeatTurnDetectsDuplicate(t *testing.T) {
	m := newTestManager(1)
	now := time.Now()
	window := time.Minute
	if m.CheckRepeatTurn("sess", "route", "turn-a", now, window, 1) {
		t.Fatal("first observation reported as duplicate")
	}
	if !m.CheckRepeatTurn("sess", "route", "turn-a", now.Add(time.Second), window, 1) {
		t.Fatal("same turn within window should be duplicate")
	}
	if m.CheckRepeatTurn("sess", "route", "turn-b", now.Add(time.Second), window, 1) {
		t.Fatal("different turn should not be duplicate")
	}
}

func TestCheckRepeatTurnExpiresOutsideWindow(t *testing.T) {
	m := newTestManager(1)
	now := time.Now()
	window := time.Minute
	m.CheckRepeatTurn("sess", "route", "turn-a", now, window, 1)
	// Just after the window expires, the old observation is gone, so this is a
	// fresh observation (not a duplicate).
	if m.CheckRepeatTurn("sess", "route", "turn-a", now.Add(window+time.Second), window, 1) {
		t.Fatal("repeat after window expiry should not duplicate the evicted entry")
	}
	// The fresh observation at now+window+1s is now in the window; a second call
	// shortly after should duplicate it.
	if !m.CheckRepeatTurn("sess", "route", "turn-a", now.Add(window+2*time.Second), window, 1) {
		t.Fatal("repeat of the fresh observation should be a duplicate")
	}
}

func TestCheckRepeatTurnDifferentSessionsAndRoutes(t *testing.T) {
	m := newTestManager(1)
	now := time.Now()
	window := time.Minute
	m.CheckRepeatTurn("sess1", "route", "turn-a", now, window, 1)
	if m.CheckRepeatTurn("sess2", "route", "turn-a", now.Add(time.Second), window, 1) {
		t.Fatal("different session should not duplicate")
	}
	if m.CheckRepeatTurn("sess1", "other", "turn-a", now.Add(time.Second), window, 1) {
		t.Fatal("different route should not duplicate")
	}
}

func TestCheckRepeatTurnGenerationGate(t *testing.T) {
	m := newTestManager(1)
	now := time.Now()
	window := time.Minute
	m.CheckRepeatTurn("sess", "route", "turn-a", now, window, 1)
	if m.CheckRepeatTurn("sess", "route", "turn-a", now.Add(time.Second), window, 2) {
		t.Fatal("stale generation should not report duplicate")
	}
}

func TestCheckRepeatTurnClearedOnReplaceGeneration(t *testing.T) {
	m := newTestManager(1)
	now := time.Now()
	window := time.Minute
	m.CheckRepeatTurn("sess", "route", "turn-a", now, window, 1)
	m.ReplaceGeneration(2)
	if m.CheckRepeatTurn("sess", "route", "turn-a", now.Add(time.Second), window, 2) {
		t.Fatal("turn window survived ReplaceGeneration")
	}
}

func TestCheckRepeatTurnWindowBound(t *testing.T) {
	m := newTestManager(1)
	now := time.Now()
	window := time.Hour
	for i := 0; i < maxRepeatTurnWindowEntries+10; i++ {
		m.CheckRepeatTurn("sess", "route", "turn-"+string(rune('a'+i%26)), now.Add(time.Duration(i)*time.Second), window, 1)
	}
	key := repeatTurnKey{SessionKey: "sess", Route: "route"}
	m.mu.Lock()
	w := m.repeatTurns[key]
	m.mu.Unlock()
	if w == nil || len(w.Entries) > maxRepeatTurnWindowEntries {
		t.Fatalf("window bound exceeded: len=%d, want <= %d", len(w.Entries), maxRepeatTurnWindowEntries)
	}
}
