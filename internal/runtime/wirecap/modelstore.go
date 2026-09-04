package wirecap

import (
	"sync"
	"time"
)

// ModelProtocols is one model's three-protocol capability matrix on its
// provider. A leg is No (never Unknown) when its base URL isn't configured —
// unsupported by definition, not by observation.
type ModelProtocols struct {
	Chat      Verdict `json:"chat"`
	Anthropic Verdict `json:"anthropic"`
	Responses Verdict `json:"responses"`
}

// Concluded reports whether every leg reached a final verdict (yes or no).
// Unknown legs are re-probed on the next pass.
func (mp ModelProtocols) Concluded() bool {
	return mp.Chat != Unknown && mp.Anthropic != Unknown && mp.Responses != Unknown
}

// ProviderModelCaps is one provider's persisted model-level capability state.
// Fingerprint invalidates the whole entry when the provider's
// protocol-relevant config changes (base urls / provider_id / headers) — a
// matching fingerprint means the verdicts are reused without re-probing (no
// TTL).
type ProviderModelCaps struct {
	Fingerprint string                    `json:"fingerprint"`
	ProbedAt    time.Time                 `json:"probed_at"`
	Models      map[string]ModelProtocols `json:"models"`
}

// ModelStore owns the leaf lock and the (provider-parent, model) keyed
// capability map. Methods never call application code while holding the lock.
// The zero value is ready for use; the map is allocated lazily by Put.
//
// The expected-fingerprint map (installed by Restore) is the write guard: a
// probe pass from a superseded config generation must not write its verdicts
// back over the current generation's state. Put silently drops writes whose
// fingerprint does not match the expected one, so a slow old-generation pass
// racing a reload cannot resurrect stale verdicts.
type ModelStore struct {
	mu       sync.RWMutex
	caps     map[string]ProviderModelCaps
	expected map[string]string
}

// Get returns one model's protocol matrix.
func (store *ModelStore) Get(parent, model string) (ModelProtocols, bool) {
	if store == nil {
		return ModelProtocols{}, false
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	entry, ok := store.caps[parent]
	if !ok {
		return ModelProtocols{}, false
	}
	mp, ok := entry.Models[model]
	return mp, ok
}

// ProviderFingerprint returns the fingerprint recorded for a provider.
func (store *ModelStore) ProviderFingerprint(parent string) (string, bool) {
	if store == nil {
		return "", false
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	entry, ok := store.caps[parent]
	return entry.Fingerprint, ok
}

// Put records one model's protocol matrix, stamping the provider's
// fingerprint and probe time. The write is dropped when Restore installed an
// expected fingerprint for the provider and this one differs — the writer is
// a probe pass from a superseded config generation (pre-reload capture), and
// its verdicts describe a base URL the current config no longer has.
func (store *ModelStore) Put(parent, fingerprint, model string, mp ModelProtocols, now time.Time) {
	if store == nil {
		return
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if want, ok := store.expected[parent]; ok && want != fingerprint {
		return
	}
	if store.caps == nil {
		store.caps = map[string]ProviderModelCaps{}
	}
	entry := store.caps[parent]
	entry.Fingerprint = fingerprint
	entry.ProbedAt = now
	if entry.Models == nil {
		entry.Models = map[string]ModelProtocols{}
	}
	entry.Models[model] = mp
	store.caps[parent] = entry
}

// MarkResponsesUnsupported corrects a stale positive model-level verdict
// after a verdict-selected /responses request receives a route-level 404.
// No-op when the (parent, model) entry doesn't exist — a verdict-driven choice
// implies one does.
func (store *ModelStore) MarkResponsesUnsupported(parent, model string, now time.Time) {
	if store == nil {
		return
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	entry, ok := store.caps[parent]
	if !ok {
		return
	}
	mp, ok := entry.Models[model]
	if !ok {
		return
	}
	mp.Responses = No
	entry.Models[model] = mp
	entry.ProbedAt = now
	store.caps[parent] = entry
}

// Snapshot returns a detached deep copy suitable for persistence or API
// projection.
func (store *ModelStore) Snapshot() map[string]ProviderModelCaps {
	out := map[string]ProviderModelCaps{}
	if store == nil {
		return out
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	for parent, entry := range store.caps {
		models := make(map[string]ModelProtocols, len(entry.Models))
		for model, mp := range entry.Models {
			models[model] = mp
		}
		out[parent] = ProviderModelCaps{
			Fingerprint: entry.Fingerprint,
			ProbedAt:    entry.ProbedAt,
			Models:      models,
		}
	}
	return out
}

// Restore replaces the store with persisted entries whose fingerprint still
// matches the provider's current protocol-relevant config. Providers absent
// from fingerprints (deleted from config) are dropped. The fingerprint map is
// ALSO installed as the store's expected fingerprints, arming Put's
// stale-generation guard — callers must therefore re-Run Restore on every
// config generation swap (boot AND reload), not just at first load.
func (store *ModelStore) Restore(loaded map[string]ProviderModelCaps, fingerprints map[string]string) {
	if store == nil {
		return
	}
	next := make(map[string]ProviderModelCaps, len(loaded))
	for parent, entry := range loaded {
		fp, configured := fingerprints[parent]
		if configured && fp == entry.Fingerprint {
			next[parent] = entry
		}
	}
	expected := make(map[string]string, len(fingerprints))
	for parent, fp := range fingerprints {
		expected[parent] = fp
	}
	store.mu.Lock()
	store.caps = next
	store.expected = expected
	store.mu.Unlock()
}
