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
