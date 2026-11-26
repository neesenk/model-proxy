package runtime

import (
	"context"
	"reflect"
	"testing"
	"time"

	"model-proxy/internal/testsyn"
)

// takeSlot takes one in-flight slot that must be dispatchable.
func takeSlot(t *testing.T, m *Manager, provider string) InflightSlot {
	t.Helper()
	slot, ok := m.TakeInflightSlot(provider)
	if !ok {
		t.Fatalf("take on %q refused, want dispatchable", provider)
	}
	return slot
}

// TestTakeInflightSlotGatesOnLearnedCap: a request occupies its account from
// dispatch to completion; once the account's concurrency limit is known, a
// request that would exceed it is refused while the slots are full — yet still
// counted, because a caller that dispatches regardless (a pin) really is
// running against the account.
func TestTakeInflightSlotGatesOnLearnedCap(t *testing.T) {
	m := newTestManager(4)
	for want := 0; want < 3; want++ {
		slot, ok := m.TakeInflightSlot("zcode")
		if !ok || slot != (InflightSlot{Provider: "zcode", Level: want}) {
			t.Fatalf("take %d on an unlimited account = %+v, %v; want level %d, dispatchable", want, slot, ok, want)
		}
	}
	if got := m.Concurrency("zcode"); got != (ConcurrencyStatus{Inflight: 3}) {
		t.Fatalf("concurrency = %+v, want 3 in flight and no cap", got)
	}

	m = newTestManager(4)
	m.NoteConcurrencyLimit("zcode", 0, 2, 4)
	var held []InflightSlot
	for i, wantOK := range []bool{true, true, false, false} {
		slot, ok := m.TakeInflightSlot("zcode")
		if ok != wantOK {
			t.Fatalf("take %d dispatchable = %v, want %v", i, ok, wantOK)
		}
		held = append(held, slot)
	}
	if got := m.Concurrency("zcode").Inflight; got != 4 {
		t.Fatalf("in flight = %d, want 4 (a refused take still occupies the account)", got)
	}
	for _, slot := range held[:3] {
		m.ReleaseInflightSlot(slot, false)
	}
	extra := takeSlot(t, m, "zcode") // one slot free again
	for _, slot := range held[3:] {
		m.ReleaseInflightSlot(slot, false)
	}
	m.ReleaseInflightSlot(extra, false)
	if got := m.Concurrency("zcode"); got != (ConcurrencyStatus{Cap: 2}) {
		t.Fatalf("concurrency after every release = %+v, want no in-flight and the cap kept", got)
	}
}

// TestNoteConcurrencyLimitStatedLimitIsAuthoritative: the upstream's own
// "limit" is a fact — set once, corrected only by a different statement, never
// trimmed by inference; an inferred cap only ever comes down; and a 429 with
// no other request in flight says nothing about concurrency.
func TestNoteConcurrencyLimitStatedLimitIsAuthoritative(t *testing.T) {
	m := newTestManager(4)
	if cap, changed := m.NoteConcurrencyLimit("zcode", 0, 5, 4); cap != 5 || !changed {
		t.Fatalf("inferred cap = %d changed=%v, want 5 changed", cap, changed)
	}
	if cap, changed := m.NoteConcurrencyLimit("zcode", 0, 6, 4); cap != 5 || changed {
		t.Fatalf("higher observation: cap = %d changed=%v, want 5 unchanged", cap, changed)
	}
	if cap, changed := m.NoteConcurrencyLimit("zcode", 8, 3, 4); cap != 8 || !changed {
		t.Fatalf("stated cap = %d changed=%v, want 8 changed", cap, changed)
	}
	if got := m.Concurrency("zcode"); !got.Stated || got.Cap != 8 {
		t.Fatalf("concurrency = %+v, want a stated cap of 8", got)
	}
	if cap, changed := m.NoteConcurrencyLimit("zcode", 0, 3, 4); cap != 8 || changed {
		t.Fatalf("inference on a stated cap: cap = %d changed=%v, want 8 unchanged", cap, changed)
	}
	if cap, changed := m.NoteConcurrencyLimit("zcode", 8, 7, 4); cap != 8 || changed {
		t.Fatalf("restated identically: cap = %d changed=%v, want 8 unchanged", cap, changed)
	}
	if cap, changed := m.NoteConcurrencyLimit("zcode", 12, 7, 4); cap != 12 || !changed {
		t.Fatalf("restated differently: cap = %d changed=%v, want 12 changed", cap, changed)
	}
	if cap, changed := m.NoteConcurrencyLimit("aqp", 0, 0, 4); cap != 0 || changed {
		t.Fatalf("429 with nothing else in flight: cap = %d changed=%v, want no cap", cap, changed)
	}
	if cap, changed := m.NoteConcurrencyLimit("aqp", 0, 4, 3); cap != 0 || changed {
		t.Fatalf("stale generation taught cap = %d changed=%v, want ignored", cap, changed)
	}
}

// TestConcurrencyCapProbesUpAfterFullOccupancySuccesses: an inferred cap is a
// guess the account keeps testing. Twenty consecutive successes of requests
// that took their slot at full occupancy (cap−1 already running) raise it by
// one; successes at lower occupancy prove nothing and leave the streak alone;
// a concurrency 429 resets the streak; a stated cap is never probed.
func TestConcurrencyCapProbesUpAfterFullOccupancySuccesses(t *testing.T) {
	m := newTestManager(4)
	m.NoteConcurrencyLimit("zcode", 0, 3, 4)
	// fullOccupancySuccess completes one request that exercised the current
	// cap: cap−1 holders, then the probe, then the holders.
	fullOccupancySuccess := func() bool {
		var held []InflightSlot
		for i := 1; i < m.Concurrency("zcode").Cap; i++ {
			held = append(held, takeSlot(t, m, "zcode"))
		}
		probe, _ := m.TakeInflightSlot("zcode")
		_, raised := m.ReleaseInflightSlot(probe, true)
		for _, slot := range held {
			m.ReleaseInflightSlot(slot, true)
		}
		return raised
	}
	for i := 1; i < 20; i++ {
		if fullOccupancySuccess() {
			t.Fatalf("cap raised on full-occupancy success %d, want only on the 20th", i)
		}
	}
	if got := m.Concurrency("zcode").Cap; got != 3 {
		t.Fatalf("cap after 19 full successes = %d, want 3", got)
	}
	if !fullOccupancySuccess() {
		t.Fatal("20th full-occupancy success did not raise the cap")
	}
	if got := m.Concurrency("zcode").Cap; got != 4 {
		t.Fatalf("cap after 20 full successes = %d, want 4", got)
	}

	// A failure breaks the streak of consecutive successes.
	for i := 0; i < 19; i++ {
		fullOccupancySuccess()
	}
	slot, _ := m.TakeInflightSlot("zcode")
	m.ReleaseInflightSlot(slot, false)
	// Low-occupancy successes neither help nor break it: 19 full ones, 30 idle
	// ones, then the 20th full one raises.
	for i := 0; i < 19; i++ {
		if fullOccupancySuccess() {
			t.Fatal("cap raised within 20 full-occupancy successes after a failure")
		}
	}
	for i := 0; i < 30; i++ {
		slot, _ := m.TakeInflightSlot("zcode")
		if _, raised := m.ReleaseInflightSlot(slot, true); raised {
			t.Fatal("an idle-occupancy success raised the cap")
		}
	}
	if got := m.Concurrency("zcode").Cap; got != 4 {
		t.Fatalf("cap after idle successes = %d, want 4", got)
	}
	if !fullOccupancySuccess() {
		t.Fatal("20th full-occupancy success (across idle ones) did not raise the cap")
	}

	// A concurrency 429 lowers the cap and resets the streak.
	for i := 0; i < 19; i++ {
		fullOccupancySuccess()
	}
	if cap, changed := m.NoteConcurrencyLimit("zcode", 0, 2, 4); cap != 2 || !changed {
		t.Fatalf("lowered cap = %d changed=%v, want 2 changed", cap, changed)
	}
	for i := 1; i < 20; i++ {
		if fullOccupancySuccess() {
			t.Fatalf("cap raised on full-occupancy success %d after a 429, want the streak reset", i)
		}
	}
	if !fullOccupancySuccess() {
		t.Fatal("cap did not climb back after 20 post-429 full successes")
	}

	m.NoteConcurrencyLimit("step", 2, 0, 4)
	for i := 0; i < 40; i++ {
		held := takeSlot(t, m, "step")
		probe, _ := m.TakeInflightSlot("step")
		if _, raised := m.ReleaseInflightSlot(probe, true); raised {
			t.Fatal("a stated cap was probed upward")
		}
		m.ReleaseInflightSlot(held, true)
	}
	if got := m.Concurrency("step"); got != (ConcurrencyStatus{Cap: 2, Stated: true}) {
		t.Fatalf("stated concurrency = %+v, want a stated cap of 2", got)
	}
}

// TestSaturatedAccountLeavesSchedulingButStaysUsableByMCP: saturation is
// scheduling unavailability for LLM traffic — the scheduler drops the account
// and the pre-dispatch health check refuses it — while the MCP gateway's
// account picks (no model, a different upstream budget) never see it. A pin
// is a hard choice: its account stays schedulable while saturated.
func TestSaturatedAccountLeavesSchedulingButStaysUsableByMCP(t *testing.T) {
	m := newTestManager(4)
	now := time.Now()
	m.NoteConcurrencyLimit("pool#a", 0, 1, 4)
	held := takeSlot(t, m, "pool#a")
	targets := []Target{{Provider: "pool#a", Parent: "pool"}, {Provider: "pool#b", Parent: "pool"}}
	decide := func() ScheduleResult {
		return m.DecideOrder(ScheduleInput{Exposed: "route", Now: now, QuotaMaxAge: time.Hour, Targets: targets})
	}

	if m.TargetHealthy("pool#a", "m", now) {
		t.Fatal("saturated account reported healthy for scheduling")
	}
	if !m.AccountHealthy("pool#a") {
		t.Fatal("MCP account pick saw LLM concurrency as unavailability")
	}
	if got := decide().Order; !reflect.DeepEqual(got, []int{1}) {
		t.Fatalf("order with a saturated account = %v, want only the free sibling", got)
	}
	m.SetPin("route", Pin{Provider: "pool"})
	if got := decide().Order; len(got) != 2 {
		t.Fatalf("pinned order with a saturated account = %v, want the pinned pool's members kept", got)
	}
	m.ClearPin("route")

	m.ReleaseInflightSlot(held, true)
	if !m.TargetHealthy("pool#a", "m", now) {
		t.Fatal("freed account still reported unhealthy")
	}
	if !m.HasRecoveredUntried(targets, map[string]bool{"pool#b": true}, now, time.Hour) {
		t.Fatal("a slot that freed on an untried account is the recovered-untried case")
	}
}

// TestCooldownStateTreatsSaturationAsRateLimitClass: every target saturated
// reads as all-down in the rate-limit class (the honest terminal is 429) with
// no time horizon — and as waitable, because a completing request frees a
// slot at any moment. Another down reason on the same target (a freeze) makes
// it not waitable; an open circuit elsewhere drops the pure-rate-limit class.
func TestCooldownStateTreatsSaturationAsRateLimitClass(t *testing.T) {
	m := newTestManager(4)
	now := time.Now()
	saturate := func(name string) InflightSlot {
		m.NoteConcurrencyLimit(name, 0, 1, 4)
		return takeSlot(t, m, name)
	}
	b := saturate("b")
	targets := []Target{{Provider: "a"}, {Provider: "b"}}
	if allDown, _, _ := m.CooldownState(targets, now, time.Hour); allDown {
		t.Fatal("one free target read as all-down")
	}
	a := saturate("a")
	allDown, allRateLimited, earliest := m.CooldownState(targets, now, time.Hour)
	if !allDown || !allRateLimited || !earliest.Equal(now) {
		t.Fatalf("all saturated: allDown=%v allRateLimited=%v earliest=%v, want true/true/now", allDown, allRateLimited, earliest)
	}
	if !m.ConcurrencyWaitable(targets, now, time.Hour) {
		t.Fatal("saturated-only targets are not waitable")
	}
	m.ReleaseInflightSlot(b, false)
	b = saturate("b")
	m.RecordFailure("b", 1, time.Hour, 0, 4)
	allDown, allRateLimited, _ = m.CooldownState(targets, now, time.Hour)
	if !allDown || allRateLimited {
		t.Fatalf("saturated + circuit-open: allDown=%v allRateLimited=%v, want true/false", allDown, allRateLimited)
	}
	m.FreezeHealth("a", nil, []string{"a"})
	if m.ConcurrencyWaitable(targets, now, time.Hour) {
		t.Fatal("a frozen account is not waitable even while saturated")
	}
	m.ReleaseInflightSlot(a, false)
	m.ReleaseInflightSlot(b, false)
}

// TestResetHealthClearsLearnedConcurrencyCapKeepsInflight: unfreeze is the
// operator's escape hatch from a wrongly learned cap, but it cannot pretend
// running requests stopped — the in-flight count survives, and releases bring
// it to zero, never below. An account whose cap was cleared is named in the
// cleared list even with no cooldown entry, so the operator's unfreeze does
// not report "nothing frozen" over a limit it did clear.
func TestResetHealthClearsLearnedConcurrencyCapKeepsInflight(t *testing.T) {
	m := newTestManager(4)
	m.NoteConcurrencyLimit("zcode", 0, 2, 4)
	held := takeSlot(t, m, "zcode")
	m.TakeInflightSlot("busy-only")
	cleared, _ := m.ResetHealth("", nil)
	if !reflect.DeepEqual(cleared, []string{"zcode"}) {
		t.Fatalf("cleared = %v, want the account whose cap was cleared (an account with requests but no cap is not frozen state)", cleared)
	}
	if got := m.Concurrency("zcode"); got != (ConcurrencyStatus{Inflight: 1}) {
		t.Fatalf("concurrency after unfreeze = %+v, want the in-flight request kept and the cap cleared", got)
	}
	for i := 0; i < 3; i++ {
		m.ReleaseInflightSlot(held, true)
	}
	if got := m.Concurrency("zcode"); got != (ConcurrencyStatus{}) {
		t.Fatalf("concurrency after repeated releases = %+v, want zero", got)
	}
}

// TestReplaceGenerationPrunesConcurrencyStateToLiveKeys: a reload keeps what
// is still true (a surviving account's cap and its running requests) and
// drops what is gone; a request still running on a removed account releases
// without resurrecting it.
func TestReplaceGenerationPrunesConcurrencyStateToLiveKeys(t *testing.T) {
	m := newTestManager(4)
	m.NoteConcurrencyLimit("a", 0, 2, 4)
	m.NoteConcurrencyLimit("b", 0, 3, 4)
	running := takeSlot(t, m, "b")
	m.ReplaceGeneration(5, map[string]bool{"a": true})
	if got := m.Concurrency("a"); got != (ConcurrencyStatus{Cap: 2}) {
		t.Fatalf("surviving account concurrency = %+v, want its cap kept", got)
	}
	if got := m.Concurrency("b"); got != (ConcurrencyStatus{}) {
		t.Fatalf("removed account concurrency = %+v, want pruned", got)
	}
	m.ReleaseInflightSlot(running, true)
	if got := m.Concurrency("b"); got != (ConcurrencyStatus{}) {
		t.Fatalf("removed account concurrency after its release = %+v, want still absent", got)
	}
}

// TestDashboardProjectsConcurrency: the per-provider read model carries each
// account's in-flight count and learned cap — including an account that has
// concurrency state but no health entry, which gets a closed/available row of
// its own. An account with neither stays absent, and `available` keeps
// meaning health: a busy account is not a down one.
func TestDashboardProjectsConcurrency(t *testing.T) {
	m := newTestManager(4)
	m.NoteConcurrencyLimit("zcode", 0, 2, 4)
	takeSlot(t, m, "zcode")
	m.RecordFailure("other", 1, time.Hour, 0, 4)
	snapshot := m.Dashboard(time.Now())
	if got := snapshot.Providers["zcode"]; got != (ProviderStatus{CircuitState: "closed", Available: true, Inflight: 1, ConcurrencyCap: 2}) {
		t.Fatalf("dashboard row for zcode = %+v, want closed/available with 1 in flight and cap 2", got)
	}
	if got := snapshot.Providers["other"]; got.Inflight != 0 || got.ConcurrencyCap != 0 {
		t.Fatalf("dashboard row for an account without concurrency = %+v, want zero counts", got)
	}
	if _, ok := snapshot.Providers["idle"]; ok {
		t.Fatal("dashboard carries a row for an account with neither health nor concurrency state")
	}
}

// TestWaitInflightSlotWakesWhenASlotFrees: a waiter parked on saturated
// targets returns the moment a request completes — no polling — and also on
// its bound or the client leaving.
func TestWaitInflightSlotWakesWhenASlotFrees(t *testing.T) {
	m := newTestManager(4)
	targets := []Target{{Provider: "a"}}
	if freed, canceled := m.WaitInflightSlot(targets, time.Hour, time.Second, context.Background()); !freed || canceled {
		t.Fatalf("wait with a free slot = freed=%v canceled=%v, want immediate true/false", freed, canceled)
	}

	m.NoteConcurrencyLimit("a", 0, 1, 4)
	held := takeSlot(t, m, "a")
	if freed, canceled := m.WaitInflightSlot(targets, time.Hour, 0, context.Background()); freed || canceled {
		t.Fatalf("wait on a saturated account with no budget = freed=%v canceled=%v, want false/false", freed, canceled)
	}
	ctx, cancel := context.WithCancel(context.Background())
	canceledByClient := make(chan bool, 1)
	go func() {
		_, c := m.WaitInflightSlot(targets, time.Hour, time.Minute, ctx)
		canceledByClient <- c
	}()
	awaitWaiters(t, m, 1)
	cancel()
	if <-canceledByClient != true {
		t.Fatal("client cancellation did not end the wait as canceled")
	}

	freedByRelease := make(chan bool, 1)
	go func() {
		f, _ := m.WaitInflightSlot(targets, time.Hour, time.Minute, context.Background())
		freedByRelease <- f
	}()
	awaitWaiters(t, m, 1)
	m.ReleaseInflightSlot(held, false)
	select {
	case freed := <-freedByRelease:
		if !freed {
			t.Fatal("a freed slot did not wake the waiter as freed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter was not woken by the release")
	}
}

// awaitWaiters blocks until the manager has n waiters registered.
func awaitWaiters(t *testing.T, m *Manager, n int) {
	t.Helper()
	testsyn.WaitUntil(t, "in-flight waiters registered", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.slotWaiters == n
	})
}

// A request completing on an unrelated account must not end the wait. The
// waiter parks for THESE targets, and only a slot freed on one of them makes
// it schedulable. With one manager-wide wake every unrelated completion
// handed the waiter back as "freed", so the caller spent a wait round on each
// of them and an all-saturated route answered 429 after two request lifetimes
// on OTHER accounts instead of waiting retry_wait for its own slot.
func TestWaitInflightSlotIgnoresUnrelatedRelease(t *testing.T) {
	m := newTestManager(4)
	targets := []Target{{Provider: "a"}}
	m.NoteConcurrencyLimit("a", 0, 1, 4)
	heldA := takeSlot(t, m, "a") // a is saturated: its one slot is taken
	heldB := takeSlot(t, m, "b") // an unrelated account running a request

	freed := make(chan bool, 1)
	go func() {
		f, _ := m.WaitInflightSlot(targets, time.Hour, time.Minute, context.Background())
		freed <- f
	}()
	awaitWaiters(t, m, 1)

	m.ReleaseInflightSlot(heldB, true)
	// Let the waiter act on that release: it must either still be parked on a
	// NEW wake channel (it woke, looked, and parked again) or — the bug —
	// have returned.
	testsyn.WaitUntil(t, "the waiter to settle after the unrelated release", func() bool {
		if len(freed) > 0 {
			return true
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.slotWaiters == 1 && m.slotFree != nil
	})
	select {
	case f := <-freed:
		t.Fatalf("waiter returned freed=%v when a request on the unrelated account b completed; a is still saturated (cap %d, %d in flight)", f, m.Concurrency("a").Cap, m.Concurrency("a").Inflight)
	default:
	}

	m.ReleaseInflightSlot(heldA, false)
	select {
	case f := <-freed:
		if !f {
			t.Fatal("releasing the slot on the waited-for account must wake the waiter as freed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter was not woken by the release on its own account")
	}
}

// Unfreeze clears a learned cap, and an account without a cap is never
// saturated: a request parked on that account's full slot-set becomes
// dispatchable that instant and must be woken, not left to time out.
func TestWaitInflightSlotWakesWhenUnfreezeClearsTheCap(t *testing.T) {
	m := newTestManager(4)
	targets := []Target{{Provider: "a"}}
	m.NoteConcurrencyLimit("a", 0, 1, 4)
	held := takeSlot(t, m, "a")

	freed := make(chan bool, 1)
	go func() {
		f, _ := m.WaitInflightSlot(targets, time.Hour, time.Minute, context.Background())
		freed <- f
	}()
	awaitWaiters(t, m, 1)
	m.ResetHealth("a", nil)
	select {
	case f := <-freed:
		if !f {
			t.Fatal("the cleared cap left the waiter without a free slot")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("clearing the cap did not wake the waiter")
	}
	m.ReleaseInflightSlot(held, false)
}

// A reload that drops the account a request waits on must not leave it parked
// until the deadline: with wakes now limited to freed slots and cleared caps,
// nothing else would ever end that wait.
func TestWaitInflightSlotEndsWhenReloadDropsTheAccount(t *testing.T) {
	m := newTestManager(4)
	targets := []Target{{Provider: "a"}}
	m.NoteConcurrencyLimit("a", 0, 1, 4)
	held := takeSlot(t, m, "a")

	done := make(chan bool, 1)
	go func() {
		f, _ := m.WaitInflightSlot(targets, time.Hour, time.Minute, context.Background())
		done <- f
	}()
	awaitWaiters(t, m, 1)
	m.ReplaceGeneration(5, nil) // the account leaves the config
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("dropping the waited-for account did not end the wait")
	}
	m.ReleaseInflightSlot(held, false)
}

// TestCooldownStateIgnoresModelLocks: a model lock makes the scheduler drop
// the target and the request ends through the ordinary all-failed path; it is
// not an availability fact for the wait-retry decision (a lock about to lapse
// is the recovered-untried case, answered with an immediate re-pass). Pins
// that folding saturation into the same check did not widen it.
func TestCooldownStateIgnoresModelLocks(t *testing.T) {
	m := newTestManager(4)
	now := time.Now()
	m.RecordModelFailure("a", "m", time.Hour, 4)
	allDown, _, _ := m.CooldownState([]Target{{Provider: "a", Model: "m"}}, now, time.Hour)
	if allDown {
		t.Fatal("a model lock alone read as all-down; CooldownState must not consult model locks")
	}
}
