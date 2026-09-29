package runtime

import (
	"sort"
	"time"
)

func (m *Manager) SetSticky(key string, sticky Sticky, generation uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLocked()
	if !m.generationMatchesLocked(generation) {
		return false
	}
	m.sticky[key] = sticky
	return true
}

func (m *Manager) Sticky(key string) (Sticky, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	value, ok := m.sticky[key]
	return value, ok
}

func (m *Manager) SetLatch(sessionKey, route string, latch Latch, generation uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLocked()
	if !m.generationMatchesLocked(generation) {
		return false
	}
	m.latch[latchKey{SessionKey: sessionKey, Route: route}] = latch
	return true
}

func (m *Manager) LatchValue(sessionKey, route string) (Latch, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	value, ok := m.latch[latchKey{SessionKey: sessionKey, Route: route}]
	return value, ok
}

func (m *Manager) ClearLatch(sessionKey, route string, generation uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLocked()
	if !m.generationMatchesLocked(generation) {
		return false
	}
	key := latchKey{SessionKey: sessionKey, Route: route}
	_, ok := m.latch[key]
	delete(m.latch, key)
	return ok
}

// RecordLatchOutcome atomically applies one request outcome to the
// (sessionKey, route) latch: the expiry check, the bad-run streak
// increment/reset and the escalation all happen inside this single Manager
// critical section, so concurrent requests of the same session cannot lose
// updates (the previous LatchValue→SetLatch read-modify-write across calls
// could). Pure in-memory: no callbacks, no I/O while holding the lock.
// Returns false on generation mismatch or missing keys.
func (m *Manager) RecordLatchOutcome(in LatchOutcome, generation uint64) bool {
	if in.SessionKey == "" || in.Route == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLocked()
	if !m.generationMatchesLocked(generation) {
		return false
	}
	key := latchKey{SessionKey: in.SessionKey, Route: in.Route}
	latch, has := m.latch[key]
	if has && in.Now.Sub(latch.Since) > in.Dwell {
		has = false
		latch = Latch{}
	}
	if in.BadSignals <= 0 {
		if in.Good && has {
			latch.BadRuns = 0
			m.latch[key] = latch
		}
		return true
	}
	badRuns := latch.BadRuns + in.BadSignals
	since := latch.Since
	target := latch.Target
	if !has {
		since = in.Now
	}
	if badRuns >= in.Consecutive {
		target = in.Target
		since = in.Now
		badRuns = 0
	}
	m.latch[key] = Latch{Target: target, Since: since, BadRuns: badRuns}
	return true
}

// CheckRepeatTurn reports whether the same turn key was observed for the same
// session and exposed route within the supplied time window. It always records
// the current observation (unless the generation has changed), so a subsequent
// identical turn within the window will return true. The window is bounded:
// expired entries are evicted and the per-(session,route) list is capped at
// maxRepeatTurnWindowEntries. A non-positive window is treated as 30 minutes.
// The method does not perform I/O or callbacks while holding the Manager lock.
func (m *Manager) CheckRepeatTurn(sessionKey, route, turnKey string, now time.Time, window time.Duration, generation uint64) bool {
	if sessionKey == "" || route == "" || turnKey == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLocked()
	if !m.generationMatchesLocked(generation) {
		return false
	}
	if window <= 0 {
		window = 30 * time.Minute
	}
	key := repeatTurnKey{SessionKey: sessionKey, Route: route}
	w := m.repeatTurns[key]
	if w == nil {
		w = &repeatTurnWindow{}
		m.repeatTurns[key] = w
	}
	cutoff := now.Add(-window)
	// Evict expired entries.
	kept := w.Entries[:0]
	for _, e := range w.Entries {
		if !e.At.Before(cutoff) {
			kept = append(kept, e)
		}
	}
	w.Entries = kept
	// Check for a duplicate.
	duplicate := false
	for _, e := range w.Entries {
		if e.TurnKey == turnKey {
			duplicate = true
			break
		}
	}
	// Record the current observation.
	w.Entries = append(w.Entries, repeatTurnEntry{TurnKey: turnKey, At: now})
	// Enforce the bound by dropping oldest entries.
	if len(w.Entries) > maxRepeatTurnWindowEntries {
		w.Entries = w.Entries[len(w.Entries)-maxRepeatTurnWindowEntries:]
	}
	return duplicate
}

func (m *Manager) SetPin(route string, pin Pin) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLocked()
	m.pins[route] = pin
}

func (m *Manager) ClearPin(route string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.pins[route]
	delete(m.pins, route)
	return ok
}

// Pins returns active pins without deleting expired entries.
func (m *Manager) Pins(now time.Time) map[string]Pin {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]Pin, len(m.pins))
	for route, pin := range m.pins {
		if pin.Active(now) {
			out[route] = pin
		}
	}
	return out
}

func (m *Manager) PinForces(exposed string, targets []Target, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	pin, ok := m.pins[exposed]
	if !ok || !pin.Active(now) {
		return false
	}
	for _, target := range targets {
		if target.Provider == pin.Provider || target.Parent == pin.Provider {
			return true
		}
	}
	return false
}

func (m *Manager) ResolverSpreadStart(parent string, n int, generation uint64) int {
	if n <= 0 {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLocked()
	if !m.generationMatchesLocked(generation) {
		return 0
	}
	start := int(m.spread[parent] % uint64(n))
	m.spread[parent]++
	return start
}

func (m *Manager) LearnParamBlock(providerName, model, param string, generation uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLocked()
	if !m.generationMatchesLocked(generation) {
		return false
	}
	key := ModelKey{Provider: providerName, Model: model}
	blocked := m.paramBlock[key]
	if blocked == nil {
		blocked = make(map[string]bool)
		m.paramBlock[key] = blocked
	}
	isNew := !blocked[param]
	blocked[param] = true
	return isNew
}

func (m *Manager) ParamBlock(providerName, model string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	values := m.paramBlock[ModelKey{Provider: providerName, Model: model}]
	out := make([]string, 0, len(values))
	for param := range values {
		out = append(out, param)
	}
	sort.Strings(out)
	return out
}

func (m *Manager) ParamBlocked(providerName, model, param string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.paramBlock[ModelKey{Provider: providerName, Model: model}][param]
}
