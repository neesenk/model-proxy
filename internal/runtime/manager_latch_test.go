package runtime

import (
	"sync"
	"testing"
	"time"
)

func TestLatchSetAndGet(t *testing.T) {
	m := newTestManager(1)
	v := Latch{Target: "b/mb", Since: time.Now(), BadRuns: 1}
	if !m.SetLatch("sess", "ra", v, 1) {
		t.Fatal("SetLatch rejected same-generation write")
	}
	got, ok := m.LatchValue("sess", "ra")
	if !ok || got.Target != v.Target || got.BadRuns != v.BadRuns {
		t.Fatalf("LatchValue = %+v ok=%v, want %+v", got, ok, v)
	}
	if _, ok := m.LatchValue("sess", "rb"); ok {
		t.Fatal("latch must not be visible on another route")
	}
}

func TestLatchGenerationGate(t *testing.T) {
	m := newTestManager(1)
	if m.SetLatch("sess", "ra", Latch{Target: "b/mb"}, 2) {
		t.Fatal("SetLatch accepted stale generation")
	}
	if m.ClearLatch("sess", "ra", 2) {
		t.Fatal("ClearLatch accepted stale generation")
	}
	m.SetLatch("sess", "ra", Latch{Target: "b/mb"}, 1)
	if m.ClearLatch("sess", "ra", 2) {
		t.Fatal("ClearLatch accepted stale generation after set")
	}
	if _, ok := m.LatchValue("sess", "ra"); !ok {
		t.Fatal("stale-generation clear must not remove latch")
	}
}

func TestLatchClearedOnReplaceGeneration(t *testing.T) {
	m := newTestManager(1)
	m.SetLatch("sess", "ra", Latch{Target: "b/mb"}, 1)
	m.ReplaceGeneration(2, nil)
	if _, ok := m.LatchValue("sess", "ra"); ok {
		t.Fatal("latch survived ReplaceGeneration")
	}
}

func TestLatchClear(t *testing.T) {
	m := newTestManager(1)
	m.SetLatch("sess", "ra", Latch{Target: "b/mb"}, 1)
	if !m.ClearLatch("sess", "ra", 1) {
		t.Fatal("ClearLatch rejected same-generation clear")
	}
	if _, ok := m.LatchValue("sess", "ra"); ok {
		t.Fatal("latch still present after clear")
	}
}

func TestRecordLatchOutcomeEscalatesAfterConsecutive(t *testing.T) {
	m := newTestManager(1)
	now := time.Now()
	bad := LatchOutcome{
		SessionKey: "sess", Route: "ra", Now: now,
		Dwell: 30 * time.Minute, Consecutive: 2, Target: "b/mb",
		BadSignals: 1,
	}
	if !m.RecordLatchOutcome(bad, 1) {
		t.Fatal("RecordLatchOutcome rejected same-generation write")
	}
	l, ok := m.LatchValue("sess", "ra")
	if !ok || l.BadRuns != 1 || l.Target != "" {
		t.Fatalf("after one bad run: %+v ok=%v, want BadRuns=1 Target empty", l, ok)
	}
	if !m.RecordLatchOutcome(bad, 1) {
		t.Fatal("RecordLatchOutcome rejected same-generation write")
	}
	l, ok = m.LatchValue("sess", "ra")
	if !ok || l.Target != "b/mb" || l.BadRuns != 0 {
		t.Fatalf("after two bad runs: %+v ok=%v, want escalated Target=b/mb BadRuns=0", l, ok)
	}
}

func TestRecordLatchOutcomeGoodRunResetsStreakKeepsTarget(t *testing.T) {
	m := newTestManager(1)
	now := time.Now()
	m.SetLatch("sess", "ra", Latch{Target: "b/mb", Since: now, BadRuns: 1}, 1)
	good := LatchOutcome{
		SessionKey: "sess", Route: "ra", Now: now,
		Dwell: 30 * time.Minute, Consecutive: 2, Target: "b/mb",
		Good: true,
	}
	if !m.RecordLatchOutcome(good, 1) {
		t.Fatal("RecordLatchOutcome rejected same-generation write")
	}
	l, ok := m.LatchValue("sess", "ra")
	if !ok || l.BadRuns != 0 || l.Target != "b/mb" || !l.Since.Equal(now) {
		t.Fatalf("good run: %+v ok=%v, want BadRuns=0 Target kept Since kept", l, ok)
	}
}

func TestRecordLatchOutcomeScopedPerRoute(t *testing.T) {
	m := newTestManager(1)
	now := time.Now()
	bad := func(route string) LatchOutcome {
		return LatchOutcome{
			SessionKey: "sess", Route: route, Now: now,
			Dwell: 30 * time.Minute, Consecutive: 3, Target: "b/mb",
			BadSignals: 1,
		}
	}
	m.RecordLatchOutcome(bad("ra"), 1)
	m.RecordLatchOutcome(bad("ra"), 1)
	// A good run on route rb must not touch route ra's streak.
	m.RecordLatchOutcome(LatchOutcome{
		SessionKey: "sess", Route: "rb", Now: now,
		Dwell: 30 * time.Minute, Consecutive: 3, Target: "b/mb",
		Good: true,
	}, 1)
	l, ok := m.LatchValue("sess", "ra")
	if !ok || l.BadRuns != 2 {
		t.Fatalf("route ra streak after route rb good run: %+v ok=%v, want BadRuns=2", l, ok)
	}
	if _, ok := m.LatchValue("sess", "rb"); ok {
		t.Fatal("good run without an existing latch must not create one")
	}
	// And a latch escalated on ra must not be visible on rb.
	m.RecordLatchOutcome(LatchOutcome{
		SessionKey: "sess", Route: "ra", Now: now,
		Dwell: 30 * time.Minute, Consecutive: 1, Target: "b/mb",
		BadSignals: 1,
	}, 1)
	if _, ok := m.LatchValue("sess", "rb"); ok {
		t.Fatal("route ra latch must not be visible on route rb")
	}
}

func TestRecordLatchOutcomeExpiredLatchTreatedAsAbsent(t *testing.T) {
	m := newTestManager(1)
	stale := time.Now().Add(-time.Hour)
	m.SetLatch("sess", "ra", Latch{Target: "b/mb", Since: stale, BadRuns: 2}, 1)
	now := time.Now()
	m.RecordLatchOutcome(LatchOutcome{
		SessionKey: "sess", Route: "ra", Now: now,
		Dwell: 30 * time.Minute, Consecutive: 3, Target: "b/mb",
		BadSignals: 1,
	}, 1)
	l, ok := m.LatchValue("sess", "ra")
	if !ok || l.BadRuns != 1 || l.Target != "" || !l.Since.Equal(now) {
		t.Fatalf("expired latch: %+v ok=%v, want fresh streak BadRuns=1 Target empty Since=now", l, ok)
	}
}

func TestRecordLatchOutcomeGenerationGate(t *testing.T) {
	m := newTestManager(1)
	if m.RecordLatchOutcome(LatchOutcome{
		SessionKey: "sess", Route: "ra", Now: time.Now(),
		Dwell: 30 * time.Minute, Consecutive: 2, Target: "b/mb",
		BadSignals: 1,
	}, 2) {
		t.Fatal("RecordLatchOutcome accepted stale generation")
	}
	if _, ok := m.LatchValue("sess", "ra"); ok {
		t.Fatal("stale-generation outcome must not write a latch")
	}
	if m.RecordLatchOutcome(LatchOutcome{SessionKey: "", Route: "ra", Now: time.Now(), BadSignals: 1, Consecutive: 1, Dwell: time.Minute, Target: "b/mb"}, 1) {
		t.Fatal("RecordLatchOutcome accepted empty session key")
	}
	if m.RecordLatchOutcome(LatchOutcome{SessionKey: "sess", Route: "", Now: time.Now(), BadSignals: 1, Consecutive: 1, Dwell: time.Minute, Target: "b/mb"}, 1) {
		t.Fatal("RecordLatchOutcome accepted empty route")
	}
}

// TestRecordLatchOutcomeConcurrentBadRuns: the outcome recording is one atomic
// read-modify-write per call, so N concurrent bad runs on the same
// (session, route) leave a streak of exactly N — no lost updates.
func TestRecordLatchOutcomeConcurrentBadRuns(t *testing.T) {
	m := newTestManager(1)
	now := time.Now()
	const n = 64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.RecordLatchOutcome(LatchOutcome{
				SessionKey: "sess", Route: "ra", Now: now,
				Dwell: 30 * time.Minute, Consecutive: 1 << 30, Target: "b/mb",
				BadSignals: 1,
			}, 1)
		}()
	}
	wg.Wait()
	l, ok := m.LatchValue("sess", "ra")
	if !ok {
		t.Fatal("no latch recorded for concurrent bad runs")
	}
	if l.BadRuns != n {
		t.Fatalf("BadRuns = %d, want %d (lost updates in concurrent outcome recording)", l.BadRuns, n)
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
	m.ReplaceGeneration(2, nil)
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
