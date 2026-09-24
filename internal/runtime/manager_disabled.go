package runtime

import "sort"

// SetModelDisabled installs or clears the operator disable override for one
// (provider, model). `provider` is the config-level provider name (a pooled
// parent disables the model across its virtual accounts via the Target.Parent
// check); exact virtual-id keys also work. Disabling is idempotent; enabling a
// never-disabled pair is a no-op. Like pins the state is memory-only: it
// survives hot reloads and is cleared on restart.
func (m *Manager) SetModelDisabled(provider, model string, disabled bool) {
	if provider == "" || model == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLocked()
	key := ModelKey{Provider: provider, Model: model}
	if disabled {
		m.disabledModels[key] = true
		return
	}
	delete(m.disabledModels, key)
}

// ModelDisabled reports the exact-key disable state for (provider, model).
// Parent expansion is the caller's concern (use TargetDisabled for scheduling
// targets, which carry their pool parent).
func (m *Manager) ModelDisabled(provider, model string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.disabledModels[ModelKey{Provider: provider, Model: model}]
}

// TargetDisabled reports whether a scheduling target is operator-disabled:
// either its own provider key or (for pooled virtual accounts) its parent's
// config-level key matches a disabled (provider, model).
func (m *Manager) TargetDisabled(target Target) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.targetDisabledLocked(target)
}

// targetDisabledLocked is TargetDisabled with m.mu held.
func (m *Manager) targetDisabledLocked(target Target) bool {
	if m.disabledModels[ModelKey{Provider: target.Provider, Model: target.Model}] {
		return true
	}
	return target.Parent != "" &&
		m.disabledModels[ModelKey{Provider: target.Parent, Model: target.Model}]
}

// DisabledModels projects the disable override as a detached, sorted
// provider→models map for the admin/API read surface. The map is a deep copy;
// callers may retain or mutate it.
func (m *Manager) DisabledModels() map[string][]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string][]string, len(m.disabledModels))
	for key := range m.disabledModels {
		out[key.Provider] = append(out[key.Provider], key.Model)
	}
	for provider := range out {
		sort.Strings(out[provider])
	}
	return out
}

// RouteFullyDisabled reports whether a non-empty target list is ENTIRELY
// operator-disabled per this snapshot's detached copy of the override. The
// schedule-status projection skips such routes (they are hidden from
// /v1/models and cannot be served, so listing them would render empty chain
// blocks); an empty list is never "fully disabled".
func (snapshot DashboardSnapshot) RouteFullyDisabled(targets []Target) bool {
	if len(targets) == 0 {
		return false
	}
	for _, target := range targets {
		if !snapshot.disabled.targetDisabled(target) {
			return false
		}
	}
	return true
}

// disabledModelSet is the detached snapshot form consumed by PreviewOrder
// (DashboardSnapshot.carried). Lookup mirrors targetDisabledLocked: the exact
// provider key or the pool parent's config-level key.
type disabledModelSet map[string]map[string]bool

func (s disabledModelSet) targetDisabled(target Target) bool {
	if s[target.Provider][target.Model] {
		return true
	}
	return target.Parent != "" && s[target.Parent][target.Model]
}

func cloneDisabledModels(disabled map[ModelKey]bool) disabledModelSet {
	out := make(disabledModelSet, len(disabled))
	for key := range disabled {
		models := out[key.Provider]
		if models == nil {
			models = make(map[string]bool, 1)
			out[key.Provider] = models
		}
		models[key.Model] = true
	}
	return out
}
