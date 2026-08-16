package runtime

import (
	"sort"
	"time"
)

type providerHealth struct {
	consecutiveFailures int
	circuitOpenUntil    time.Time
	rateLimitedUntil    time.Time
	rateLimitKind       RateLimitKind
	halfOpenInFlight    bool
}

func (h *providerHealth) available(now time.Time) bool {
	if now.Before(h.rateLimitedUntil) {
		return false
	}
	if !h.circuitOpenUntil.IsZero() && now.Before(h.circuitOpenUntil) {
		return false
	}
	if !h.circuitOpenUntil.IsZero() &&
		!now.Before(h.circuitOpenUntil) &&
		h.halfOpenInFlight {
		return false
	}
	return true
}

type modelLock struct {
	failures    int
	lockedUntil time.Time
}

// ResetHealth clears circuit/rate-limit state and model locks, preserving
// sticky, pins, learned parameters, spread counters, and quotas.
func (m *Manager) ResetHealth(name string, parentOf map[string]string) (cleared []string, locks int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	match := func(providerName string) bool {
		return name == "" || providerName == name || parentOf[providerName] == name
	}
	for providerName := range m.health {
		if match(providerName) {
			delete(m.health, providerName)
			cleared = append(cleared, providerName)
		}
	}
	// unfreeze is the operator saying "retry now": the quality EWMA must not
	// keep demoting a provider the operator just cleared.
	for providerName := range m.quality {
		if match(providerName) {
			delete(m.quality, providerName)
		}
	}
	for key := range m.modelLocks {
		if match(key.Provider) {
			delete(m.modelLocks, key)
			locks++
		}
	}
	sort.Strings(cleared)
	return cleared, locks
}

func (m *Manager) TargetHealthy(providerName, model string, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.health[providerName]
	return (state == nil || state.available(now)) && !m.modelLockedLocked(providerName, model, now)
}

// TakeHalfOpenSlot uses the current clock to preserve the existing request-path
// API. A stale request is allowed to finish but cannot mutate the new state.
func (m *Manager) TakeHalfOpenSlot(name string, generation uint64) bool {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.generationMatchesLocked(generation) {
		return true
	}
	state := m.health[name]
	if state == nil {
		return true
	}
	if now.Before(state.rateLimitedUntil) {
		return false
	}
	if state.circuitOpenUntil.IsZero() {
		return true
	}
	if now.Before(state.circuitOpenUntil) {
		return false
	}
	if state.halfOpenInFlight {
		return false
	}
	state.halfOpenInFlight = true
	return true
}

func (m *Manager) ReleaseHalfOpenSlot(name string, generation uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.generationMatchesLocked(generation) {
		return
	}
	if state := m.health[name]; state != nil {
		state.halfOpenInFlight = false
	}
}

func (m *Manager) RecordSuccess(name, model string, generation uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.generationMatchesLocked(generation) {
		return
	}
	if state := m.health[name]; state != nil {
		state.consecutiveFailures = 0
		state.circuitOpenUntil = time.Time{}
		state.halfOpenInFlight = false
	}
	delete(m.modelLocks, ModelKey{Provider: name, Model: model})
	m.recordQualityLocked(name, 0, time.Now())
}

func (m *Manager) RecordFailure(name string, threshold int, cooldown time.Duration, generation uint64) {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLocked()
	if !m.generationMatchesLocked(generation) {
		return
	}
	state := m.health[name]
	if state == nil {
		state = &providerHealth{}
		m.health[name] = state
	}
	state.consecutiveFailures++
	state.halfOpenInFlight = false
	if state.consecutiveFailures >= threshold {
		state.circuitOpenUntil = now.Add(cooldown)
	}
	m.recordQualityLocked(name, 1, now)
}

func (m *Manager) modelLockedLocked(providerName, model string, now time.Time) bool {
	entry := m.modelLocks[ModelKey{Provider: providerName, Model: model}]
	return entry != nil && now.Before(entry.lockedUntil)
}

func (m *Manager) ModelLocked(providerName, model string, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.modelLockedLocked(providerName, model, now)
}

func (m *Manager) RecordModelFailure(providerName, model string, lockout time.Duration, generation uint64) {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLocked()
	if !m.generationMatchesLocked(generation) {
		return
	}
	key := ModelKey{Provider: providerName, Model: model}
	entry := m.modelLocks[key]
	if entry == nil {
		entry = &modelLock{}
		m.modelLocks[key] = entry
	}
	entry.failures++
	entry.lockedUntil = now.Add(lockout)
}

func (m *Manager) CooldownState(targets []Target, now time.Time) (allDown, allRateLimited bool, earliest time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(targets) == 0 {
		return false, false, time.Time{}
	}
	for _, target := range targets {
		state := m.health[target.Provider]
		if state == nil || state.available(now) {
			return false, false, time.Time{}
		}
	}

	allDown, allRateLimited = true, true
	for _, target := range targets {
		state := m.health[target.Provider]
		rateLimited := now.Before(state.rateLimitedUntil)
		circuitOpen := !state.circuitOpenUntil.IsZero() && now.Before(state.circuitOpenUntil)
		var until time.Time
		switch {
		case rateLimited && circuitOpen:
			allRateLimited = false
			until = state.rateLimitedUntil
			if state.circuitOpenUntil.After(until) {
				until = state.circuitOpenUntil
			}
		case rateLimited:
			until = state.rateLimitedUntil
		case circuitOpen:
			allRateLimited = false
			until = state.circuitOpenUntil
		default:
			allRateLimited = false
			until = now
		}
		if until.Before(now) {
			until = now
		}
		if earliest.IsZero() || until.Before(earliest) {
			earliest = until
		}
	}
	return allDown, allRateLimited, earliest
}

func (m *Manager) HasRecoveredUntried(targets []Target, tried map[string]bool, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, target := range targets {
		state := m.health[target.Provider]
		if (state == nil || state.available(now)) && !tried[target.Provider] {
			return true
		}
	}
	return false
}

func (m *Manager) RecordRateLimit(name string, until time.Time, kind RateLimitKind, generation uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLocked()
	if !m.generationMatchesLocked(generation) {
		return false
	}
	state := m.health[name]
	if state == nil {
		state = &providerHealth{}
		m.health[name] = state
	}
	state.halfOpenInFlight = false
	if until.After(state.rateLimitedUntil) {
		state.rateLimitedUntil = until
		state.rateLimitKind = kind
	}
	return true
}
