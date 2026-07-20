package main

import (
	"testing"
	"time"
)

// TestResolver_ExpandAndPick: the unified provider resolver — pool expansion
// (Expand) and sticky/round-robin single-virtual pick (Pick) with a health hint.
// Non-pooled providers pass through; unknown / not-built providers yield no
// runnable virtual.
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
	p := NewProxy(cfg)
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

	// Pick: one zhipu virtual, ok=true, Model preserved.
	pick, ok := r.Pick(RouteTarget{Provider: "zhipu", Model: "glm"})
	if !ok {
		t.Fatal("Pick pooled: !ok, want ok")
	}
	if pick.target.Provider == "zhipu" {
		t.Error("Pick returned the parent name, not a virtual")
	}
	if p.parentOf[pick.target.Provider] != "zhipu" {
		t.Error("Pick did not return a zhipu virtual")
	}
	if pick.target.Model != "glm" {
		t.Error("Pick dropped Model")
	}

	// Pick round-robins across the pool (spreadCtr): 10 picks cover BOTH virtuals.
	seen := map[string]bool{}
	for i := 0; i < 10; i++ {
		rt, ok := r.Pick(RouteTarget{Provider: "zhipu", Model: "glm"})
		if !ok {
			t.Fatal("Pick !ok during round-robin")
		}
		seen[rt.target.Provider] = true
	}
	if len(seen) != 2 {
		t.Errorf("Pick round-robin covered %d virtuals, want 2 (%v)", len(seen), seen)
	}

	// Non-pooled provider: Expand passes through unchanged.
	e := r.Expand(RouteTarget{Provider: "single", Model: "m"})
	if len(e) != 1 || e[0].Provider != "single" {
		t.Errorf("Expand non-pooled = %+v, want [{single}]", e)
	}
	// Pick on a non-pooled name with no built impl → !ok (not runnable). "single"
	// isn't in p.providers (only the zhipu virtuals are), so it is not runnable.
	if _, ok := r.Pick(RouteTarget{Provider: "single"}); ok {
		t.Error("Pick on a non-pooled name with no built impl should be !ok")
	}

	// Health hint: a nil-health virtual is available; a circuit-open one is not.
	healthyVid := got[0].Provider
	if h := r.hintFor(healthyVid); !h.available || h.circuitOpen || h.rateLimited {
		t.Errorf("hintFor healthy virtual = %+v, want available", h)
	}
	openVid := got[1].Provider
	p.healthMu.Lock()
	p.health[openVid] = &providerHealth{circuitOpenUntil: time.Now().Add(time.Hour)}
	p.healthMu.Unlock()
	if h := r.hintFor(openVid); h.available || !h.circuitOpen {
		t.Errorf("hintFor circuit-open virtual = %+v, want !available+circuitOpen", h)
	}
}
