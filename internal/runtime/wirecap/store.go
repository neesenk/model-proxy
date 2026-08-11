// Package wirecap owns the concurrency-safe, persisted state kernel for
// endpoint wire-capability verdicts. HTTP probing, provider authentication,
// config lookup, and persistence scheduling remain application concerns.
package wirecap

import (
	"encoding/json"
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

// Capabilities is one endpoint's persisted wire-capability verdict.
type Capabilities struct {
	BaseURL   string    `json:"base_url"`
	Responses Verdict   `json:"responses"`
	Anthropic Verdict   `json:"anthropic"`
	ProbedAt  time.Time `json:"probed_at"`
}

// Store owns the leaf lock and provider-parent keyed capability map.
// Methods never call application code while holding the lock.
type Store struct {
	mu   sync.RWMutex
	caps map[string]Capabilities
}

func NewStore() *Store {
	return &Store{caps: map[string]Capabilities{}}
}

func (store *Store) Get(parent string) (Capabilities, bool) {
	if store == nil {
		return Capabilities{}, false
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	capabilities, ok := store.caps[parent]
	return capabilities, ok
}

func (store *Store) Put(parent string, capabilities Capabilities) {
	if store == nil {
		return
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.caps == nil {
		store.caps = map[string]Capabilities{}
	}
	store.caps[parent] = capabilities
}

// MarkResponsesUnsupported corrects a stale positive verdict after a
// verdict-selected /responses request receives a route-level 404.
func (store *Store) MarkResponsesUnsupported(parent string, now time.Time) {
	if store == nil {
		return
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.caps == nil {
		store.caps = map[string]Capabilities{}
	}
	capabilities := store.caps[parent]
	capabilities.Responses = No
	capabilities.ProbedAt = now
	store.caps[parent] = capabilities
}

// Snapshot returns a detached map suitable for persistence.
func (store *Store) Snapshot() map[string]Capabilities {
	if store == nil {
		return map[string]Capabilities{}
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	out := make(map[string]Capabilities, len(store.caps))
	for parent, capabilities := range store.caps {
		out[parent] = capabilities
	}
	return out
}

// RestoreMatching replaces the store with persisted entries whose base URL
// still matches the current provider configuration.
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
		if configured && baseURL == capabilities.BaseURL {
			next[parent] = capabilities
		}
	}
	store.mu.Lock()
	store.caps = next
	store.mu.Unlock()
}
