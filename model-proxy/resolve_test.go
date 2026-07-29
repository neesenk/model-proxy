package main

import (
	"testing"
	"time"

	"model-proxy/provider"
)

type fakeResolverState struct {
	spreadStart      int
	spreadCalls      int
	spreadGeneration uint64
	healthy          map[string]bool
}

func (s *fakeResolverState) resolverSpreadStart(_ string, n int, generation uint64) int {
	s.spreadCalls++
	s.spreadGeneration = generation
	if n == 0 {
		return 0
	}
	return s.spreadStart % n
}

func (s *fakeResolverState) resolverTargetHealthy(virtual, _ string, _ time.Time) bool {
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
	r := newResolver(
		state,
		map[string]provider.Provider{"pool#a": nil, "pool#b": nil},
		map[string][]string{"pool": {"pool#a", "pool#b"}},
		9,
	)

	picked, ok := r.Pick(RouteTarget{Provider: "pool", Model: "m"}, "")
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
	if _, ok := r.Pick(RouteTarget{Provider: "pool", Model: "m"}, "session"); !ok {
		t.Fatal("sticky Pick returned !ok; want healthy sibling")
	}
	if state.spreadCalls != 1 {
		t.Fatalf("sticky Pick spread calls = %d, want unchanged 1", state.spreadCalls)
	}
}

// TestResolver_ExpandAndPick: the unified provider resolver — pool expansion
// (Expand), session-sticky + health-aware single-virtual pick (Pick). Non-pooled
// providers pass through; unknown / not-built providers yield no runnable virtual.
func TestResolver_ExpandAndPick(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	writePoolFile(t, "zhipu", "zhipu", "KEY-A", "KEY-B")
	cfg := &Config{
		Listen: "127.0.0.1:1",
		Providers: map[string]Provider{
			"zhipu": {OpenAIBaseURL: "https://z", Provider: "zhipu"},
		},
	}
	p := newTestProxy(t, cfg)
	r := newResolver(p, p.providers, p.poolIndex)

	// Expand: pooled parent → both virtuals, Model/Priority/Protocol preserved.
	got := r.Expand(RouteTarget{Provider: "zhipu", Model: "glm", Priority: 2, Protocol: "openai"})
	if len(got) != 2 {
		t.Fatalf("Expand pooled = %d targets, want 2", len(got))
	}
	for i, rt := range got {
		if rt.Provider == "zhipu" {
			t.Errorf("Expand[%d] returned the parent name, not a virtual", i)
		}
		if rt.Model != "glm" || rt.Priority != 2 || rt.Protocol != "openai" {
			t.Errorf("Expand[%d] = %+v, want Model/Priority/Protocol preserved", i, rt)
		}
		if p.parentOf[rt.Provider] != "zhipu" {
			t.Errorf("Expand[%d] %q is not a zhipu virtual", i, rt.Provider)
		}
	}

	// Pick with a session key is STICKY: the same key always lands on the same
	// virtual (cache-warm for a conversation).
	sessA, ok := r.Pick(RouteTarget{Provider: "zhipu", Model: "glm"}, "session-A")
	if !ok {
		t.Fatal("Pick pooled !ok")
	}
	if p.parentOf[sessA.Provider] != "zhipu" {
		t.Error("Pick did not return a zhipu virtual")
	}
	for i := 0; i < 5; i++ {
		pick, ok := r.Pick(RouteTarget{Provider: "zhipu", Model: "glm"}, "session-A")
		if !ok || pick.Provider != sessA.Provider {
			t.Errorf("Pick session-A iter %d = %q, want stable %q (session-sticky)", i, pick.Provider, sessA.Provider)
		}
	}
	// A different session key lands on a (likely) different virtual — distinct
	// sessions spread across the pool. With 2 accounts and a good hash, both
	// appear across a handful of distinct keys.
	seen := map[string]bool{sessA.Provider: true}
	for _, key := range []string{"s1", "s2", "s3", "s4", "s5", "s6"} {
		pick, ok := r.Pick(RouteTarget{Provider: "zhipu", Model: "glm"}, key)
		if !ok {
			t.Fatalf("Pick %s !ok", key)
		}
		seen[pick.Provider] = true
	}
	if len(seen) != 2 {
		t.Errorf("distinct session keys covered %d virtuals, want 2 (%v)", len(seen), seen)
	}

	// Non-pooled provider: Expand passes through; Pick on a built non-pooled name
	// returns it, on a not-built name returns !ok.
	p2 := newTestProxy(t, &Config{
		Listen:    "127.0.0.1:1",
		Providers: map[string]Provider{"single": {OpenAIBaseURL: "https://x", Provider: testProviderID}},
	})
	r2 := newResolver(p2, p2.providers, p2.poolIndex)
	if e := r2.Expand(RouteTarget{Provider: "single", Model: "m"}); len(e) != 1 || e[0].Provider != "single" {
		t.Errorf("Expand non-pooled = %+v, want [{single}]", e)
	}
	// "single" is a file-backed static provider (built even when not logged in) →
	// Pick returns it. A name with NO built impl (not in cfg.Providers at all) → !ok.
	if pick, ok := r2.Pick(RouteTarget{Provider: "single"}, ""); !ok || pick.Provider != "single" {
		t.Errorf("Pick built non-pooled = %+v ok=%v, want {single}/ok", pick, ok)
	}
	if _, ok := r2.Pick(RouteTarget{Provider: "does-not-exist"}, ""); ok {
		t.Error("Pick on a name with no built impl should be !ok")
	}

	// Health-aware failover: circuit-open the sticky account → Pick fails over to
	// the healthy sibling instead of returning the dead one.
	seedRuntimeCircuit(t, p, sessA.Provider, time.Now().Add(time.Hour))
	fallback, ok := r.Pick(RouteTarget{Provider: "zhipu", Model: "glm"}, "session-A")
	if !ok {
		t.Fatal("Pick session-A with sticky account circuit-open should fail over, got !ok")
	}
	if fallback.Provider == sessA.Provider {
		t.Errorf("Pick did not fail over from the circuit-open sticky account %q (still picked it)", sessA.Provider)
	}
	if p.parentOf[fallback.Provider] != "zhipu" {
		t.Error("failover pick is not a zhipu virtual")
	}

	// All accounts circuit-open → Pick !ok (no healthy virtual).
	other := fallback.Provider
	seedRuntimeCircuit(t, p, other, time.Now().Add(time.Hour))
	if _, ok := r.Pick(RouteTarget{Provider: "zhipu", Model: "glm"}, "session-A"); ok {
		t.Error("Pick with ALL accounts circuit-open should be !ok")
	}
}

// TestResolver_PickSkipsModelLockedVirtual (bug 6): the resolver is the health
// gate for Fusion panel/judge legs + Shadow. It used to check PROVIDER health
// only (circuit/rate-limit), so a model the main routing path had already
// locked (recordModelFailure) was still picked here — Fusion/Shadow kept
// requesting a known-bad (provider, model) until the lockout expired. Pick must
// treat a model-locked (virtual, model) as unavailable, like tryTarget does.
func TestResolver_PickSkipsModelLockedVirtual(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	writePoolFile(t, "zhipu", "zhipu", "KEY-A", "KEY-B")
	cfg := &Config{Listen: "127.0.0.1:1", Providers: map[string]Provider{
		"zhipu": {OpenAIBaseURL: "https://z", Provider: "zhipu"},
	}}
	p := newTestProxy(t, cfg)
	r := newResolver(p, p.providers, p.poolIndex)

	// Pre-lock: the model resolves to a healthy virtual.
	if _, ok := r.Pick(RouteTarget{Provider: "zhipu", Model: "glm"}, "session-1"); !ok {
		t.Fatal("pre-lock Pick returned !ok; want a healthy virtual")
	}

	// Lock the model on EVERY virtual (the main routing path would skip them all).
	for _, vid := range p.poolIndex["zhipu"] {
		p.recordModelFailure(vid, "glm", Scheduling{ModelLockout: "1h"})
	}
	if _, ok := r.Pick(RouteTarget{Provider: "zhipu", Model: "glm"}, "session-1"); ok {
		t.Errorf("post-lock Pick returned ok for a fully model-locked pool; resolver must skip locked (provider,model)")
	}

	// A DIFFERENT model on the same pool is unaffected — the lock is model-specific.
	if _, ok := r.Pick(RouteTarget{Provider: "zhipu", Model: "glm-other"}, "session-1"); !ok {
		t.Errorf("unlocked model on same pool should still resolve")
	}
}
