// Package wirecap owns the endpoint wire-capability domain: the concurrency-safe,
// persisted verdict stores (provider-level Store and model-level ModelStore),
// the protocol-selection policy over those verdicts, and the model_caps.json
// file format. Executing probes, 404-correction triggers and async persistence
// scheduling remain application concerns (internal/app/wirecap.go,
// internal/app/modelcaps.go); probe execution itself lives in internal/probe.
package wirecap

import (
	"encoding/json"
	"maps"
	"strings"
	"sync"
	"time"
)

// Verdict is a three-valued endpoint capability conclusion.
type Verdict uint8

const (
	Unknown Verdict = iota
	Yes
	No
)

func (verdict Verdict) String() string {
	switch verdict {
	case Yes:
		return "yes"
	case No:
		return "no"
	default:
		return "unknown"
	}
}

// ParseVerdict parses the stable persisted representation.
func ParseVerdict(value string) Verdict {
	switch value {
	case "yes":
		return Yes
	case "no":
		return No
	default:
		return Unknown
	}
}

func (verdict Verdict) MarshalJSON() ([]byte, error) {
	return json.Marshal(verdict.String())
}

func (verdict *Verdict) UnmarshalJSON(data []byte) error {
	*verdict = ParseVerdict(strings.Trim(string(data), `"`))
	return nil
}

// ProbeVersion is the provider-level probe semantics version persisted in
// every Capabilities entry. Bump history:
//
//	2 — legs are probed with a function tool attached (agent-grade
//	callability, mirroring model_caps.json version 2); v1 verdicts measured
//	bare pings and are discarded on restore so everything re-probes.
const ProbeVersion = 2

// Capabilities is one endpoint's persisted wire-capability verdict. Anthropic
// support is NOT probed: a provider declares it by configuring
// anthropic_base_url (the decision matrix short-circuits on that), so only the
// two openai-base legs are stored.
type Capabilities struct {
	BaseURL      string    `json:"base_url"`
	Chat         Verdict   `json:"chat"`
	Responses    Verdict   `json:"responses"`
	ProbedAt     time.Time `json:"probed_at"`
	ProbeVersion int       `json:"probe_version"`
}

// Store owns the leaf lock and provider-parent keyed capability map.
// Methods never call application code while holding the lock. The zero value
// is ready for use; the map is allocated lazily by Put.
type Store struct {
	mu       sync.RWMutex
	view     ProviderView
	baseURLs map[string]string
}

// ProviderView is an immutable provider verdict map. Writers publish a new map.
type ProviderView struct{ caps map[string]Capabilities }

func (v ProviderView) Get(parent string) (Capabilities, bool) {
	c, ok := v.caps[parent]
	return c, ok
}

func (store *Store) View() ProviderView {
	if store == nil {
		return ProviderView{}
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	return store.view
}

func (store *Store) Get(parent string) (Capabilities, bool) {
	return store.View().Get(parent)
}

func (store *Store) Put(parent string, capabilities Capabilities) {
	if store == nil {
		return
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	store.putLocked(parent, capabilities)
}

func (store *Store) putLocked(parent string, c Capabilities) {
	if store.baseURLs != nil {
		if base, ok := store.baseURLs[parent]; !ok || base != c.BaseURL {
			return
		}
	}
	next := maps.Clone(store.view.caps)
	if next == nil {
		next = make(map[string]Capabilities)
	}
	next[parent] = c
	store.view = ProviderView{caps: next}
}

// MarkResponsesUnsupported attributes a route-level 404 to the observed endpoint.
func (store *Store) MarkResponsesUnsupported(parent, baseURL string, now time.Time) {
	if store == nil {
		return
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	c, ok := store.view.Get(parent)
	if ok && c.BaseURL != baseURL {
		return
	}
	c.BaseURL, c.Responses, c.ProbedAt = baseURL, No, now
	store.putLocked(parent, c)
}

// Snapshot returns a detached map suitable for persistence.
func (store *Store) Snapshot() map[string]Capabilities {
	out := map[string]Capabilities{}
	maps.Copy(out, store.View().caps)
	return out
}

// Reconfigure retains verdicts for unchanged endpoints and rejects late writes from removed endpoints.
func (store *Store) Reconfigure(baseURLs map[string]string) {
	store.mu.Lock()
	defer store.mu.Unlock()
	next := maps.Clone(store.view.caps)
	for parent, c := range next {
		if base, ok := baseURLs[parent]; !ok || base != c.BaseURL {
			delete(next, parent)
		}
	}
	store.view = ProviderView{caps: next}
	store.baseURLs = maps.Clone(baseURLs)
}

// RestoreMatching replaces the store with persisted entries whose base URL
// still matches the current provider configuration AND whose probe semantics
// are current (ProbeVersion). Stale-semantics verdicts — e.g. v1 bare-ping
// "yes" entries that never measured tools — are dropped so the next probe
// pass re-measures them agent-grade; a wrong v1 yes is trusted indefinitely
// otherwise (yes never expires, only the runtime 404 correction rewrites it).
func (store *Store) RestoreMatching(
	loaded map[string]Capabilities,
	baseURLs map[string]string,
) {
	if store == nil {
		return
	}
	next := make(map[string]Capabilities, len(loaded))
	for parent, capabilities := range loaded {
		baseURL, configured := baseURLs[parent]
		if configured && baseURL == capabilities.BaseURL && capabilities.ProbeVersion == ProbeVersion {
			next[parent] = capabilities
		}
	}
	store.mu.Lock()
	store.view = ProviderView{caps: next}
	store.baseURLs = maps.Clone(baseURLs)
	store.mu.Unlock()
}
