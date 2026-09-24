// Package runtime owns the mutable, generation-scoped routing state used by
// the proxy. It deliberately depends only on provider value types: config,
// HTTP, persistence, and Web presentation remain composition-root concerns.
package runtime

import (
	"sync"
	"sync/atomic"

	"model-proxy/internal/provider"
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
	// disabledModels is the operator disable override (Web Status→Models
	// toggle): a disabled (provider, model) is excluded from scheduling and
	// from the exposed model list. Like pins it deliberately survives hot
	// reloads (ReplaceGeneration keeps it) and is memory-only (restart clears).
	disabledModels map[ModelKey]bool
	spread         map[string]uint64
	quotas         map[string]*provider.QuotaSnapshot
	// quality is copy-on-write: record paths (under m.mu) publish a NEW
	// immutable map of providerQuality VALUES; readers load the pointer without
	// the mutex so the scheduling critical section skips the decayed-status
	// projection (map alloc + EWMA math per provider). Mutations are rare
	// relative to DecideOrder calls, and each copies a map of provider-count
	// size. Readers revalidate the loaded pointer after taking m.mu so a
	// concurrent publication cannot cross a generation boundary unnoticed.
	quality atomic.Pointer[map[string]providerQuality]
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
	if m.disabledModels == nil {
		m.disabledModels = make(map[ModelKey]bool)
	}
	if m.spread == nil {
		m.spread = make(map[string]uint64)
	}
	if m.quotas == nil {
		m.quotas = make(map[string]*provider.QuotaSnapshot)
	}
	if m.quality.Load() == nil {
		empty := map[string]providerQuality{}
		m.quality.Store(&empty)
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
// preserving operator pins and the operator disabled-model override, which
// intentionally survive hot reloads.
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
	empty := map[string]providerQuality{}
	m.quality.Store(&empty)
}

// GenerationArg keeps compatibility with callers that omit a generation while
// letting request-scoped paths bind runtime mutations to their captured reload
// generation.
func GenerationArg(generations []uint64) uint64 {
	if len(generations) == 0 {
		return 0
	}
	return generations[0]
}
