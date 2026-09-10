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
	// frozen is the operator-set freeze (CLI freeze / POST /api/health/freeze):
	// the provider is excluded from scheduling until ResetHealth (unfreeze)
	// clears the entry. Success/failure/rate-limit recording never clears it.
	frozen bool
}

func (h *providerHealth) available(now time.Time) bool {
	if h.frozen {
		return false
	}
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
	if cur := m.qualitySnapshot(); len(cur) > 0 {
		next := make(map[string]providerQuality, len(cur))
		pruned := false
		for providerName, q := range cur {
			if match(providerName) {
				pruned = true
				continue
			}
			next[providerName] = q
		}
		if pruned {
			m.storeQualityLocked(next)
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

// FreezeHealth marks the matched providers as operator-frozen: available()
// reports false (scheduling skips them) until ResetHealth (unfreeze) clears the
// entry. Unlike ResetHealth, freeze ALWAYS requires an explicit target — an
// empty name matches nothing (freeze-all is deliberately not offered; unfreeze
// keeps the no-arg = all escape hatch). A pooled parent name matches all its
// virtual accounts. The match iterates the KNOWN universe of runtime provider
// keys (config names plus pooled virtual account keys) instead of only
// existing health entries, so a never-failed provider can be frozen too;
// entries are created lazily for that. Quality EWMA, model locks, quotas,
// sticky, and pins are NOT touched. Returns the sorted matched names; an
// unknown (or empty) name matches nothing (empty result).
func (m *Manager) FreezeHealth(name string, parentOf map[string]string, known []string) (frozen []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLocked()
	if name == "" {
		return nil
	}
	match := func(providerName string) bool {
		return providerName == name || parentOf[providerName] == name
	}
	for _, providerName := range known {
		if !match(providerName) {
			continue
		}
		state := m.health[providerName]
		if state == nil {
			state = &providerHealth{}
			m.health[providerName] = state
		}
		state.frozen = true
		frozen = append(frozen, providerName)
	}
	sort.Strings(frozen)
	return frozen
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

// CooldownState inspects a route's target providers for the wait-retry
// decision: allDown = EVERY target is currently unavailable (rate-limited,
// circuit-open / half-open probe in flight, OR skipped as freshly
// quota-exhausted); allRateLimited = none of the down reasons is a circuit
// (pure rate-limit/quota class — the honest terminal status is then 429, not
// 502); earliest = soonest recovery across targets (clamped to now; quota
// exhaustion recovers at the earlier of the window reset and the snapshot's
// staleness horizon). Model-level locks are not consulted — they make schedule
// drop the target, which leads here via the ordinary all-failed path.
//
// Quota exhaustion participates because the scheduling skip means an
// exhausted target never receives the upstream 429 that would otherwise
// teach health — without folding it in here, an all-exhausted route reads as
// "everything available" and terminates as a bare 502 with no Retry-After.
func (m *Manager) CooldownState(targets []Target, now time.Time, quotaMaxAge time.Duration) (allDown, allRateLimited bool, earliest time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(targets) == 0 {
		return false, false, time.Time{}
	}
	for _, target := range targets {
		state := m.health[target.Provider]
		if quotaExhaustedUntil(m.quotas[target.Provider], now, quotaMaxAge).IsZero() &&
			(state == nil || state.available(now)) {
			return false, false, time.Time{}
		}
	}

	allDown, allRateLimited = true, true
	for _, target := range targets {
		state := m.health[target.Provider]
		// Quota exhaustion is a rate-limit-class down reason: the account IS
		// limited, just measured proactively instead of via an upstream 429.
		// A target is down while EITHER reason holds, so its recovery time is
		// the later of the two.
		var rateLimitUntil time.Time
		if state != nil && now.Before(state.rateLimitedUntil) {
			rateLimitUntil = state.rateLimitedUntil
		}
		if quotaUntil := quotaExhaustedUntil(m.quotas[target.Provider], now, quotaMaxAge); quotaUntil.After(rateLimitUntil) {
			rateLimitUntil = quotaUntil
		}
		rateLimited := !rateLimitUntil.IsZero()
		circuitOpen := state != nil && !state.circuitOpenUntil.IsZero() && now.Before(state.circuitOpenUntil)
		var until time.Time
		switch {
		case rateLimited && circuitOpen:
			allRateLimited = false
			until = rateLimitUntil
			if state.circuitOpenUntil.After(until) {
				until = state.circuitOpenUntil
			}
		case rateLimited:
			until = rateLimitUntil
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

// HasRecoveredUntried reports the TOCTOU case: a target is available now but
// was NOT tried in the failed pass. A freshly quota-exhausted target never
// counts as recovered — the skip means it was deliberately not called, and
// treating it as "recovered" would spin idle rescheduling rounds on an
// all-exhausted route.
func (m *Manager) HasRecoveredUntried(targets []Target, tried map[string]bool, now time.Time, quotaMaxAge time.Duration) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, target := range targets {
		state := m.health[target.Provider]
		if !quotaExhaustedUntil(m.quotas[target.Provider], now, quotaMaxAge).IsZero() {
			continue
		}
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
