package routing

import (
	"testing"
	"time"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/provider"
)

type fakeResolverState struct {
	spreadStart      int
	spreadCalls      int
	spreadGeneration uint64
	healthy          map[string]bool
	disabled         map[string]bool
}

func (s *fakeResolverState) ModelDisabled(provider, model string) bool {
	return s.disabled[provider+"/"+model]
}

func (s *fakeResolverState) ResolverSpreadStart(_ string, n int, generation uint64) int {
	s.spreadCalls++
	s.spreadGeneration = generation
	if n == 0 {
		return 0
	}
	return s.spreadStart % n
}

func (s *fakeResolverState) TargetHealthy(virtual, _ string, _ time.Time) bool {
	return s.healthy[virtual]
}

func TestResolverUsesNarrowStateCapability(t *testing.T) {
	state := &fakeResolverState{
		spreadStart: 1,
		healthy: map[string]bool{
			"pool#a": true,
			"pool#b": false,
		},
	}
	r := NewResolver(
		state,
		map[string]provider.Provider{"pool#a": nil, "pool#b": nil},
		map[string][]string{"pool": {"pool#a", "pool#b"}},
		9,
	)

	picked, ok := r.Pick(configdomain.RouteTarget{Provider: "pool", Model: "m"}, "")
	if !ok {
		t.Fatal("Pick returned !ok; want healthy sibling failover")
	}
	if picked.Provider != "pool#a" {
		t.Fatalf("Pick provider = %q, want pool#a", picked.Provider)
	}
	if state.spreadCalls != 1 {
		t.Fatalf("spread calls = %d, want 1", state.spreadCalls)
	}
	if state.spreadGeneration != 9 {
		t.Fatalf("spread generation = %d, want request snapshot generation 9", state.spreadGeneration)
	}

	// Sticky selection is deterministic and must not consume the shared spread
	// capability; health failover still scans siblings from the sticky slot.
	if _, ok := r.Pick(configdomain.RouteTarget{Provider: "pool", Model: "m"}, "session"); !ok {
		t.Fatal("sticky Pick returned !ok; want healthy sibling")
	}
	if state.spreadCalls != 1 {
		t.Fatalf("sticky Pick spread calls = %d, want unchanged 1", state.spreadCalls)
	}
}

// TestResolverPickSkipsDisabledModel pins the operator disabled-model leg of
// Pick: a config-keyed disabled (provider, model) is refused for both pooled
// parents and plain providers — before any identity or health work — while a
// different model on the same provider keeps picking normally.
func TestResolverPickSkipsDisabledModel(t *testing.T) {
	state := &fakeResolverState{
		healthy: map[string]bool{"pool#a": true, "solo": true},
		disabled: map[string]bool{
			"pool/m": true,
		},
	}
	r := NewResolver(
		state,
		map[string]provider.Provider{"pool#a": nil, "solo": nil},
		map[string][]string{"pool": {"pool#a"}},
		1,
	)
	if _, ok := r.Pick(configdomain.RouteTarget{Provider: "pool", Model: "m"}, "s"); ok {
		t.Fatal("Pick returned ok for a disabled pooled (provider, model)")
	}
	if _, ok := r.Pick(configdomain.RouteTarget{Provider: "pool", Model: "other"}, "s"); !ok {
		t.Fatal("Pick returned !ok for a non-disabled model on the same provider")
	}
	// Non-pooled provider, exact config key.
	state.disabled["solo/m2"] = true
	if _, ok := r.Pick(configdomain.RouteTarget{Provider: "solo", Model: "m2"}, ""); ok {
		t.Fatal("Pick returned ok for a disabled non-pooled (provider, model)")
	}
	if _, ok := r.Pick(configdomain.RouteTarget{Provider: "solo", Model: "m3"}, ""); !ok {
		t.Fatal("Pick returned !ok for a non-disabled model on the solo provider")
	}
}
