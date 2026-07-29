// Package runtime owns the mutable, generation-scoped routing state used by
// the proxy. It deliberately depends only on provider value types: config,
// HTTP, persistence, and Web presentation remain composition-root concerns.
package runtime

import (
	"sync"

	"model-proxy/provider"
)

// Manager owns all mutable routing state behind one mutex. The zero value is
// ready for use; maps are allocated lazily by mutating methods.
type Manager struct {
	mu sync.Mutex

	generation uint64
	health     map[string]*providerHealth
	sticky     map[string]Sticky
	pins       map[string]Pin
	modelLocks map[ModelKey]*modelLock
	paramBlock map[ModelKey]map[string]bool
	spread     map[string]uint64
	quotas     map[string]*provider.QuotaSnapshot
}

func NewManager(generation uint64) *Manager {
	m := &Manager{generation: generation}
	m.ensureLocked()
	return m
}

func (m *Manager) ensureLocked() {
	if m.health == nil {
		m.health = make(map[string]*providerHealth)
	}
	if m.sticky == nil {
		m.sticky = make(map[string]Sticky)
	}
	if m.pins == nil {
		m.pins = make(map[string]Pin)
	}
	if m.modelLocks == nil {
		m.modelLocks = make(map[ModelKey]*modelLock)
	}
	if m.paramBlock == nil {
		m.paramBlock = make(map[ModelKey]map[string]bool)
	}
	if m.spread == nil {
		m.spread = make(map[string]uint64)
	}
	if m.quotas == nil {
		m.quotas = make(map[string]*provider.QuotaSnapshot)
	}
}

func (m *Manager) generationMatchesLocked(generation uint64) bool {
	return generation == 0 || generation == m.generation
}

func (m *Manager) Generation() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.generation
}

// ReplaceGeneration atomically clears state tied to the old config while
// preserving operator pins, which intentionally survive hot reloads.
func (m *Manager) ReplaceGeneration(generation uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLocked()
	m.generation = generation
	m.health = make(map[string]*providerHealth)
	m.sticky = make(map[string]Sticky)
	m.modelLocks = make(map[ModelKey]*modelLock)
	m.paramBlock = make(map[ModelKey]map[string]bool)
	m.spread = make(map[string]uint64)
	m.quotas = make(map[string]*provider.QuotaSnapshot)
}
