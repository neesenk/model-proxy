package runtime

import (
	"sync"
	"testing"
	"time"
)

// seedLatch installs a latch through the production write path
// (RecordLatchOutcome): an escalation (if Target is set) followed by streak
// increments (if BadRuns > 0), all anchored at latch.Since.
func seedLatch(t *testing.T, m *Manager, session, route string, latch Latch, dwell time.Duration, generation uint64) {
	t.Helper()
	if latch.Target != "" {
		if !m.RecordLatchOutcome(LatchOutcome{
			SessionKey: session, Route: route, Now: latch.Since,
			Dwell: dwell, Consecutive: 1, Target: latch.Target, BadSignals: 1,
		}, generation) {
			t.Fatal("seed escalation rejected")
		}
	}
	if latch.BadRuns > 0 {
		if !m.RecordLatchOutcome(LatchOutcome{
			SessionKey: session, Route: route, Now: latch.Since,
			Dwell: dwell, Consecutive: 1 << 30, Target: latch.Target, BadSignals: latch.BadRuns,
		}, generation) {
			t.Fatal("seed streak rejected")
		}
	}
}

func TestLatchRecordAndGet(t *testing.T) {
	m := newTestManager(1)
	now := time.Now()
	seedLatch(t, m, "sess", "ra", Latch{Target: "b/mb", Since: now, BadRuns: 1}, 30*time.Minute, 1)
	got, ok := m.LatchValue("sess", "ra")
	if !ok || got.Target != "b/mb" || got.BadRuns != 1 {
		t.Fatalf("LatchValue = %+v ok=%v, want Target=b/mb BadRuns=1", got, ok)
	}
	if _, ok := m.LatchValue("sess", "rb"); ok {
		t.Fatal("latch must not be visible on another route")
	}
}

func TestLatchGenerationGate(t *testing.T) {
	m := newTestManager(1)
	now := time.Now()
	if m.RecordLatchOutcome(LatchOutcome{
		SessionKey: "sess", Route: "ra", Now: now,
		Dwell: 30 * time.Minute, Consecutive: 1, Target: "b/mb", BadSignals: 1,
	}, 2) {
		t.Fatal("RecordLatchOutcome accepted stale generation")
	}
	seedLatch(t, m, "sess", "ra", Latch{Target: "b/mb", Since: now}, 30*time.Minute, 1)
	// A stale-generation outcome must not mutate the existing latch either.
	if m.RecordLatchOutcome(LatchOutcome{
		SessionKey: "sess", Route: "ra", Now: now,
		Dwell: 30 * time.Minute, Consecutive: 2, Target: "c/mc", BadSignals: 1,
	}, 2) {
		t.Fatal("RecordLatchOutcome accepted stale generation after seed")
	}
	l, ok := m.LatchValue("sess", "ra")
	if !ok || l.Target != "b/mb" || l.BadRuns != 0 {
		t.Fatalf("stale-generation outcome mutated the latch: %+v ok=%v", l, ok)
	}
}

func TestLatchClearedOnReplaceGeneration(t *testing.T) {
	m := newTestManager(1)
	seedLatch(t, m, "sess", "ra", Latch{Target: "b/mb", Since: time.Now()}, 30*time.Minute, 1)
	m.ReplaceGeneration(2, nil)
	if _, ok := m.LatchValue("sess", "ra"); ok {
		t.Fatal("latch survived ReplaceGeneration")
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
	seedLatch(t, m, "sess", "ra", Latch{Target: "b/mb", Since: now, BadRuns: 1}, 30*time.Minute, 1)
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
	seedLatch(t, m, "sess", "ra", Latch{Target: "b/mb", Since: stale, BadRuns: 2}, 30*time.Minute, 1)
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

// TestRecordLatchOutcomeDeletesExpiredLatchOnGoodRun: an expired latch is
// deleted outright (not merely ignored) so inactive sessions cannot grow the
// latch map within a generation. A good run — which writes nothing — must
// still drop the expired entry.
func TestRecordLatchOutcomeDeletesExpiredLatchOnGoodRun(t *testing.T) {
	m := newTestManager(1)
	stale := time.Now().Add(-time.Hour)
	seedLatch(t, m, "sess", "ra", Latch{Target: "b/mb", Since: stale}, 30*time.Minute, 1)
	m.RecordLatchOutcome(LatchOutcome{
		SessionKey: "sess", Route: "ra", Now: time.Now(),
		Dwell: 30 * time.Minute, Consecutive: 2, Target: "b/mb",
		Good: true,
	}, 1)
	if _, ok := m.LatchValue("sess", "ra"); ok {
		t.Fatal("expired latch must be deleted, not left in the map")
	}
	m.mu.Lock()
	n := len(m.latch)
	m.mu.Unlock()
	if n != 0 {
		t.Fatalf("latch map size = %d, want 0 after expired latch cleanup", n)
	}
}

// TestRecordLatchOutcomeSweepsExpiredSameRouteKeys: any outcome on a route
// prunes OTHER sessions' expired latches on that same route, bounding the map
// by sessions active within one dwell window. Keys on other routes are
// untouched — their dwell may differ, so this outcome cannot judge them.
func TestRecordLatchOutcomeSweepsExpiredSameRouteKeys(t *testing.T) {
	m := newTestManager(1)
	dwell := 30 * time.Minute
	stale := time.Now().Add(-time.Hour)
	seedLatch(t, m, "sess-old-1", "ra", Latch{Target: "b/mb", Since: stale}, dwell, 1)
	seedLatch(t, m, "sess-old-2", "ra", Latch{Target: "b/mb", Since: stale, BadRuns: 1}, dwell, 1)
	// Same age but a DIFFERENT route: must survive (route rb's dwell may be
	// longer than ra's).
	seedLatch(t, m, "sess-old-3", "rb", Latch{Target: "b/mb", Since: stale}, dwell, 1)

	m.RecordLatchOutcome(LatchOutcome{
		SessionKey: "sess-new", Route: "ra", Now: time.Now(),
		Dwell: dwell, Consecutive: 2, Target: "b/mb",
		Good: true,
	}, 1)

	if _, ok := m.LatchValue("sess-old-1", "ra"); ok {
		t.Fatal("expired same-route latch (sess-old-1) must be swept")
	}
	if _, ok := m.LatchValue("sess-old-2", "ra"); ok {
		t.Fatal("expired same-route latch (sess-old-2) must be swept")
	}
	if _, ok := m.LatchValue("sess-old-3", "rb"); !ok {
		t.Fatal("other-route latch must not be swept by a route ra outcome")
	}
	if _, ok := m.LatchValue("sess-new", "ra"); ok {
		t.Fatal("good run without an existing latch must not create one")
	}
	m.mu.Lock()
	n := len(m.latch)
	m.mu.Unlock()
	if n != 1 {
		t.Fatalf("latch map size = %d, want 1 (only the other-route latch)", n)
	}
}

func TestRecordLatchOutcomeGenerationGateRejectsEmptyKeys(t *testing.T) {
	m := newTestManager(1)
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
		t.Fatal("different turn should not duplicate")
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

// TestCheckRepeatTurnSweepsExpiredSameRouteWindows: a window whose entries
// have all expired is dropped from the map entirely (not kept as an empty
// shell), so silent sessions cannot grow the repeat-turn map within a
// generation. Other routes' windows are untouched.
func TestCheckRepeatTurnSweepsExpiredSameRouteWindows(t *testing.T) {
	m := newTestManager(1)
	now := time.Now()
	window := time.Minute
	m.CheckRepeatTurn("sess-old", "route", "turn-a", now, window, 1)
	m.CheckRepeatTurn("sess-old", "other", "turn-a", now, window, 1)

	// A later observation on the same route sweeps sess-old's expired window.
	m.CheckRepeatTurn("sess-new", "route", "turn-b", now.Add(window+time.Second), window, 1)

	m.mu.Lock()
	_, oldGone := m.repeatTurns[repeatTurnKey{SessionKey: "sess-old", Route: "route"}]
	_, otherKept := m.repeatTurns[repeatTurnKey{SessionKey: "sess-old", Route: "other"}]
	_, newKept := m.repeatTurns[repeatTurnKey{SessionKey: "sess-new", Route: "route"}]
	m.mu.Unlock()
	if oldGone {
		t.Fatal("expired same-route window must be swept from the map")
	}
	if !otherKept {
		t.Fatal("other-route window must not be swept")
	}
	if !newKept {
		t.Fatal("the recording session's window must be present")
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
