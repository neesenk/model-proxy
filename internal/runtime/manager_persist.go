package runtime

import (
	"sort"
	"time"
)

// RestoreSticky merges a detached persisted sticky snapshot into the current
// generation. Restore is a boot-time operation and therefore is not generation
// gated.
func (m *Manager) RestoreSticky(sticky map[string]Sticky) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLocked()
	for key, value := range sticky {
		m.sticky[key] = value
	}
}

// RestoreHealth restores future-dated cooldowns and model locks, plus all
// learned parameter blocks. A restored circuit receives the threshold failure
// count so its next hard failure immediately re-opens it.
func (m *Manager) RestoreHealth(health map[string]PersistedHealth, now time.Time, threshold int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLocked()
	for name, persisted := range health {
		if now.Before(persisted.RateLimitedUntil) || now.Before(persisted.CircuitOpenUntil) {
			state := m.health[name]
			if state == nil {
				state = &providerHealth{}
				m.health[name] = state
			}
			if now.Before(persisted.RateLimitedUntil) {
				state.rateLimitedUntil = persisted.RateLimitedUntil
				state.rateLimitKind = ParseRateLimitKind(persisted.RateLimitKind)
			}
			if now.Before(persisted.CircuitOpenUntil) {
				state.circuitOpenUntil = persisted.CircuitOpenUntil
				state.consecutiveFailures = threshold
			}
		}
		for model, until := range persisted.ModelLocks {
			if now.Before(until) {
				m.modelLocks[ModelKey{Provider: name, Model: model}] = &modelLock{
					failures:    1,
					lockedUntil: until,
				}
			}
		}
		for model, params := range persisted.ParamBlock {
			key := ModelKey{Provider: name, Model: model}
			blocked := m.paramBlock[key]
			if blocked == nil {
				blocked = make(map[string]bool)
				m.paramBlock[key] = blocked
			}
			for _, param := range params {
				blocked[param] = true
			}
		}
	}
}

// SnapshotForPersist copies generation, quota, route-keyed sticky, health,
// model locks, and parameter blocks while holding the single manager mutex.
func (m *Manager) SnapshotForPersist(routeKeys map[string]bool, now time.Time) PersistSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLocked()

	snapshot := PersistSnapshot{
		Generation: m.generation,
		Quotas:     cloneQuotas(m.quotas),
		Sticky:     make(map[string]Sticky),
		Health:     m.snapshotHealthLocked(now),
	}
	for key, value := range m.sticky {
		if routeKeys[key] {
			snapshot.Sticky[key] = value
		}
	}
	return snapshot
}

func (m *Manager) snapshotHealthLocked(now time.Time) map[string]PersistedHealth {
	out := make(map[string]PersistedHealth)
	names := make(map[string]bool, len(m.health)+len(m.modelLocks)+len(m.paramBlock))
	for name := range m.health {
		names[name] = true
	}
	for key := range m.modelLocks {
		names[key.Provider] = true
	}
	for key := range m.paramBlock {
		names[key.Provider] = true
	}

	for name := range names {
		persisted := PersistedHealth{}
		if state := m.health[name]; state != nil {
			persisted.RateLimitedUntil = state.rateLimitedUntil
			persisted.CircuitOpenUntil = state.circuitOpenUntil
			if now.Before(state.rateLimitedUntil) {
				persisted.RateLimitKind = state.rateLimitKind.String()
			}
		}
		if persisted.RateLimitedUntil.IsZero() && persisted.CircuitOpenUntil.IsZero() {
			if !hasModelState(name, m.modelLocks, m.paramBlock) {
				continue
			}
		}
		out[name] = persisted
	}

	for key, entry := range m.modelLocks {
		persisted := out[key.Provider]
		if persisted.ModelLocks == nil {
			persisted.ModelLocks = make(map[string]time.Time)
		}
		persisted.ModelLocks[key.Model] = entry.lockedUntil
		out[key.Provider] = persisted
	}
	for key, values := range m.paramBlock {
		if len(values) == 0 {
			continue
		}
		persisted := out[key.Provider]
		if persisted.ParamBlock == nil {
			persisted.ParamBlock = make(map[string][]string)
		}
		params := make([]string, 0, len(values))
		for param := range values {
			params = append(params, param)
		}
		sort.Strings(params)
		persisted.ParamBlock[key.Model] = params
		out[key.Provider] = persisted
	}
	return out
}

func hasModelState(
	providerName string,
	locks map[ModelKey]*modelLock,
	params map[ModelKey]map[string]bool,
) bool {
	for key := range locks {
		if key.Provider == providerName {
			return true
		}
	}
	for key, values := range params {
		if key.Provider == providerName && len(values) > 0 {
			return true
		}
	}
	return false
}
