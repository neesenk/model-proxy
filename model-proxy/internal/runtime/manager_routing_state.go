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
