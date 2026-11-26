package runtime

import (
	"context"
	"time"
)

// concurrencyGrowAfter is how many consecutive successful requests that
// exercised the current cap — each took its slot while cap−1 others were
// already running — an INFERRED cap needs before it is probed one higher.
// Twenty keeps the probe rare where the 429 rate at the cap is meaningful
// (at 10% per request, one probe in eight tries survives twenty) while still
// climbing back after a misread within a couple of minutes of full load.
const concurrencyGrowAfter = 20

// concurrencyCapMax bounds an inferred cap, so a run of luck on an account
// nobody else is using cannot grow it without limit.
const concurrencyCapMax = 64

// InflightSlot identifies one request's occupancy of an account, from the
// moment TakeInflightSlot hands it out until ReleaseInflightSlot returns it.
type InflightSlot struct {
	Provider string
	// Level is how many OTHER requests the account was already running when
	// this one took its slot — the request's vantage point on the cap.
	Level int
}

// ConcurrencyStatus is the read model of one account's concurrency.
type ConcurrencyStatus struct {
	// Inflight is the number of requests the proxy is running against the
	// account right now.
	Inflight int
	// Cap is the learned concurrency limit; 0 = none learned (unlimited).
	Cap int
	// Stated says the upstream itself named Cap; such a cap is a fact and is
	// neither trimmed by inference nor probed upward.
	Stated bool
}

// concurrencyState is an account's concurrency ledger. It is process state,
// not config-coupled scheduling state: ReplaceGeneration only prunes it to
// the live accounts (a surviving account keeps its cap and its running
// requests) and ResetHealth clears the cap but never the in-flight count —
// unfreeze cannot make running requests stop.
type concurrencyState struct {
	inflight int
	cap      int
	stated   bool
	// full counts consecutive successes that exercised the cap (the probe-up
	// streak of an inferred cap). Any unsuccessful request or any concurrency
	// 429 resets it.
	full int
	// levels is the multiset of take-time Levels of the in-flight requests,
	// so a release knows whether ITS request exercised the cap even if the cap
	// moved while it ran.
	levels map[int]int
}

// saturatedLocked reports whether every learned slot of the account is taken.
func (m *Manager) saturatedLocked(provider string) bool {
	state := m.concurrency[provider]
	return state != nil && state.cap > 0 && state.inflight >= state.cap
}

// TakeInflightSlot registers one request against the account and reports
// whether it may be dispatched: false once the account's learned concurrency
// limit is reached. The slot is taken either way — a caller that dispatches
// regardless (a pin is a hard choice) really is running against the account —
// and must be released exactly once. Not generation-gated: an in-flight
// request is a fact about the process whatever generation captured it.
func (m *Manager) TakeInflightSlot(provider string) (InflightSlot, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLocked()
	state := m.concurrency[provider]
	if state == nil {
		state = &concurrencyState{levels: map[int]int{}}
		m.concurrency[provider] = state
	}
	slot := InflightSlot{Provider: provider, Level: state.inflight}
	state.inflight++
	state.levels[slot.Level]++
	return slot, state.cap == 0 || slot.Level < state.cap
}

// ReleaseInflightSlot returns a slot taken by TakeInflightSlot. succeeded says
// the upstream served the request (a 2xx): such a request counts toward the
// probe-up streak of an inferred cap when it exercised the cap, and raised
// reports that the streak just lifted the cap to raisedTo. A release that
// leaves the account with a free slot wakes the requests parked in
// WaitInflightSlot.
func (m *Manager) ReleaseInflightSlot(slot InflightSlot, succeeded bool) (raisedTo int, raised bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.concurrency[slot.Provider]
	if state == nil {
		return 0, false // the account left the config while the request ran
	}
	if state.inflight > 0 {
		state.inflight--
	}
	if state.levels[slot.Level] <= 1 {
		delete(state.levels, slot.Level)
	} else {
		state.levels[slot.Level]--
	}
	if succeeded {
		raised = state.growLocked(slot.Level)
	} else {
		state.full = 0 // the streak counts CONSECUTIVE successes
	}
	// Wake the waiters only when THIS release freed a slot: an account still
	// at its cap (a pinned dispatch overshooting it) or that never had one is
	// nobody's reason to wait. The wake is still a broadcast — every parked
	// request looks at its own targets — but a wake meant for another account
	// costs a waiter one locked re-check and a re-park, never a wait round.
	if state.cap > 0 && state.inflight < state.cap {
		m.wakeSlotWaitersLocked()
	}
	if state.inflight == 0 && state.cap == 0 {
		delete(m.concurrency, slot.Provider)
	}
	return state.cap, raised
}

// growLocked folds one successful release into the probe-up streak and raises
// an inferred cap after concurrencyGrowAfter consecutive full-occupancy
// successes. A success below full occupancy says nothing about the limit and
// leaves the streak alone.
func (state *concurrencyState) growLocked(level int) bool {
	if state.stated || state.cap == 0 || state.cap >= concurrencyCapMax || level < state.cap-1 {
		return false
	}
	state.full++
	if state.full < concurrencyGrowAfter {
		return false
	}
	state.full = 0
	state.cap++
	return true
}

// NoteConcurrencyLimit records what a 429 proved about an account's
// concurrency limit and returns the cap in force afterwards, with changed
// set when this call moved it:
//
//   - limit > 0: the upstream stated its own limit. That is the fact — set,
//     corrected only by a different statement, never trimmed by inference.
//   - limit == 0: the limit is inferred from inflight, the other requests the
//     account was running when the rejected one was dispatched. An inferred
//     cap only comes down, and an inference never trims a stated one.
//   - inflight == 0 with nothing stated: a 429 while nothing else ran is not
//     about concurrency (a rate limit, another consumer, a plan window) and
//     teaches nothing — the caller keeps the ordinary transient cooldown.
//
// Any concurrency 429 resets the probe-up streak. A stale generation cannot
// teach: the account set it describes is gone.
func (m *Manager) NoteConcurrencyLimit(provider string, limit, inflight int, generation uint64) (cap int, changed bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLocked()
	if !m.generationMatchesLocked(generation) {
		return 0, false
	}
	state := m.concurrency[provider]
	if state == nil {
		state = &concurrencyState{levels: map[int]int{}}
		m.concurrency[provider] = state
	}
	state.full = 0
	switch {
	case limit > 0:
		changed = state.cap != limit || !state.stated
		state.cap, state.stated = limit, true
	case state.stated || inflight < 1:
	case state.cap == 0 || inflight < state.cap:
		state.cap, state.stated, changed = inflight, false, true
	}
	return state.cap, changed
}

// Concurrency is the read model of one account's concurrency.
func (m *Manager) Concurrency(provider string) ConcurrencyStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	if state := m.concurrency[provider]; state != nil {
		return ConcurrencyStatus{Inflight: state.inflight, Cap: state.cap, Stated: state.stated}
	}
	return ConcurrencyStatus{}
}

// WaitInflightSlot parks the caller until one of the targets has a free
// concurrency slot — the moment a request completes on it, not a poll — and
// returns freed then. It returns at once when a slot is already free, and
// without waiting when no target could become schedulable by a freed slot
// (each is unavailable for another reason too: frozen, cooling, quota
// exhausted). quotaMaxAge is the scheduler's quota-snapshot freshness window.
// max bounds the wait (freed=false when it elapses) and the context ends it
// as canceled.
//
// The wake is a broadcast over every parked request, so a wake meant for
// another account's slot arrives here too: after each wake the targets are
// looked at again under the lock and the request parks again when none of
// them is free. Only a slot on one of ITS targets (or a cap cleared by
// unfreeze) ends the wait as freed — an unrelated completion never spends
// the caller's wait budget.
func (m *Manager) WaitInflightSlot(targets []Target, quotaMaxAge, max time.Duration, ctx context.Context) (freed, canceled bool) {
	timer := time.NewTimer(max)
	defer timer.Stop()
	for {
		now := time.Now()
		m.mu.Lock()
		if m.anySlotFreeLocked(targets, now, quotaMaxAge) {
			m.mu.Unlock()
			return true, false
		}
		if !m.anyWaitableLocked(targets, now, quotaMaxAge) {
			m.mu.Unlock()
			return false, false
		}
		if m.slotFree == nil {
			m.slotFree = make(chan struct{})
		}
		m.slotWaiters++
		wake := m.slotFree
		m.mu.Unlock()

		select {
		case <-wake:
			// A slot freed on some capped account, or a cap was cleared:
			// look again at these targets.
		case <-ctx.Done():
			m.unwaitSlotLocked(wake)
			return false, true
		case <-timer.C:
			m.unwaitSlotLocked(wake)
			// A release that raced the deadline still gets its look.
			m.mu.Lock()
			defer m.mu.Unlock()
			return m.anySlotFreeLocked(targets, time.Now(), quotaMaxAge), false
		}
	}
}

// anySlotFreeLocked reports whether any target may be dispatched to now.
func (m *Manager) anySlotFreeLocked(targets []Target, now time.Time, quotaMaxAge time.Duration) bool {
	for _, target := range targets {
		if m.slotFreeLocked(target, now, quotaMaxAge) {
			return true
		}
	}
	return false
}

// anyWaitableLocked reports whether any target is unavailable solely because
// its slots are full — one a freed slot could make schedulable.
func (m *Manager) anyWaitableLocked(targets []Target, now time.Time, quotaMaxAge time.Duration) bool {
	for _, target := range targets {
		if m.waitableLocked(target, now, quotaMaxAge) {
			return true
		}
	}
	return false
}

// ConcurrencyWaitable reports whether at least one target is unavailable
// solely because its concurrency slots are full: such a target becomes
// schedulable the moment one of its requests completes, with no time horizon
// to wait for — the wait-retry decision treats it as worth a bounded wait.
func (m *Manager) ConcurrencyWaitable(targets []Target, now time.Time, quotaMaxAge time.Duration) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, target := range targets {
		if m.waitableLocked(target, now, quotaMaxAge) {
			return true
		}
	}
	return false
}

// schedulableLocked reports whether nothing but a full slot-set keeps the
// target out of scheduling: not frozen, cooling or circuit-open, and its
// quota not exhausted.
func (m *Manager) schedulableLocked(target Target, now time.Time, quotaMaxAge time.Duration) bool {
	state := m.health[target.Provider]
	if state != nil && !state.available(now) {
		return false
	}
	return QuotaExhaustedUntil(m.quotas[target.Provider], now, quotaMaxAge).IsZero()
}

// slotFreeLocked reports whether the target may be dispatched to right now.
func (m *Manager) slotFreeLocked(target Target, now time.Time, quotaMaxAge time.Duration) bool {
	return !m.saturatedLocked(target.Provider) && m.schedulableLocked(target, now, quotaMaxAge)
}

// waitableLocked reports whether the target is saturated and otherwise
// schedulable — unavailable solely because its slots are full.
func (m *Manager) waitableLocked(target Target, now time.Time, quotaMaxAge time.Duration) bool {
	return m.saturatedLocked(target.Provider) && m.schedulableLocked(target, now, quotaMaxAge)
}

// wakeSlotWaitersLocked broadcasts a freed slot to every parked waiter. The
// channel is closed and retired in one step: waiters deregister by identity,
// so a waiter that times out after the broadcast cannot touch the next
// generation of waiters.
func (m *Manager) wakeSlotWaitersLocked() {
	if m.slotWaiters > 0 {
		close(m.slotFree)
		m.slotFree = nil
		m.slotWaiters = 0
	}
}

// unwaitSlotLocked deregisters a waiter that stopped waiting on its own.
func (m *Manager) unwaitSlotLocked(wake chan struct{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.slotFree == wake {
		m.slotWaiters--
		if m.slotWaiters == 0 {
			m.slotFree = nil
		}
	}
}
