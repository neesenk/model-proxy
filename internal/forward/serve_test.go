package forward

import (
	"net/http/httptest"
	"strings"
	"testing"

	"model-proxy/internal/catalog"
	"model-proxy/internal/protocol"
	"model-proxy/internal/routing"
)

// TestServeOnce_EffectiveTargetsReflectsScheduledSet (bug 4, claim 1; moved
// from app): serveOnce must return the EFFECTIVE target set it actually used
// (after scheduling drops cooling targets), so the cooldown/TOCTOU decisions
// key on what was really considered — not the original route targets. Here the
// schedule port narrows to ["b"], so effectiveTargets must be ["b"].
func TestServeOnce_EffectiveTargetsReflectsScheduledSet(t *testing.T) {
	h := newHarness()
	h.svc.Schedule = func(cfg *Config, parentOf map[string]string, exposed, sessionKey string, targets []RouteTarget, routeKeys map[string]bool, generation uint64) []RouteTarget {
		return targets[1:] // "a" is cooling; only "b" is servable
	}
	cfg := &Config{
		Providers: map[string]Provider{"a": {}, "b": {}},
		Routes:    map[string][]RouteTarget{"m": {{Provider: "a"}, {Provider: "b"}}},
	}
	snap := h.snapshot(cfg)
	p := pipeline{svc: h.svc, state: h.state}
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	res := p.serveOnce(serveRequest{
		runtime: snap,
		proto:   "openai", upPath: "/chat/completions", exposed: "m", calledModel: "m",
		targets: cfg.Routes["m"], routeKeys: map[string]bool{"m": true},
		writer: httptest.NewRecorder(), request: r, requestID: "req-1",
	}, &serveState{})

	if len(res.effectiveTargets) != 1 || res.effectiveTargets[0].Provider != "b" {
		t.Errorf("effectiveTargets=%+v, want [{b}] (schedule dropped cooling a)", res.effectiveTargets)
	}
}

// TestRequestProfileComputedOncePerRequest: with a catalog the body scan runs
// once and is shared by later calls; without a catalog the profile is empty.
func TestRequestProfileComputedOncePerRequest(t *testing.T) {
	h := newHarness()
	p := pipeline{svc: h.svc, state: h.state}
	var st serveState
	if profile := p.requestProfile(&st, nil, []byte(`{"model":"m"}`)); profile.EstimatedTokens != 0 || st.profiled {
		t.Errorf("nil catalog profile = %+v profiled = %v, want zero/uncomputed", profile, st.profiled)
	}
	cat := catalog.New(nil)
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hello world"}]}`)
	first := p.requestProfile(&st, cat, body)
	if !st.profiled || first.EstimatedTokens == 0 {
		t.Fatalf("profile = %+v profiled = %v, want a computed profile", first, st.profiled)
	}
	second := p.requestProfile(&st, cat, []byte(`{"completely":"different body"}`))
	if second != first {
		t.Errorf("second call rescanned: %+v, want the cached %+v", second, first)
	}
}

// TestFusionSynthesizerSupportsTools: with no catalog and no capabilities
// override the graceful default fits.
func TestFusionSynthesizerSupportsTools(t *testing.T) {
	h := newHarness()
	p := pipeline{svc: h.svc, state: h.state}
	fc := fusionCtx{runtime: h.snapshot(&Config{
		Providers: map[string]Provider{"s": {Provider: "test-static"}},
		Routes:    map[string][]RouteTarget{},
	})}
	if !p.fusionSynthesizerSupportsTools(fc, RouteTarget{Provider: "s", Model: "mm"}) {
		t.Error("nil catalog must degrade to fits=true")
	}
	adapter := fusionAdapter{pipe: p, context: fc}
	if !adapter.SupportsTools(RouteTarget{Provider: "s", Model: "mm"}) {
		t.Error("SupportsTools port must mirror fusionSynthesizerSupportsTools")
	}
}

// TestPlanTargetExportedWrapper: PlanTarget runs the same snapshot-frozen
// planning the pipeline uses (Shadow's entry point).
func TestPlanTargetExportedWrapper(t *testing.T) {
	up := newFakeUpstream(t, openaiOKResponder("ok"))
	h := newHarness()
	cfg := &Config{
		Providers: map[string]Provider{"up": {OpenAIBaseURL: up.srv.URL, Provider: "test-static"}},
		Routes:    map[string][]RouteTarget{"m": {{Provider: "up", Model: "real-model"}}},
	}
	plan, err := PlanTarget(h.svc, PlanInput{
		Runtime:     h.snapshot(cfg),
		Target:      RouteTarget{Provider: "up", Model: "real-model"},
		ClientProto: "openai",
		ClientPath:  "/chat/completions",
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.BaseURL() != up.srv.URL || plan.BackendProtocol() != protocol.OpenAI {
		t.Errorf("plan = %s %s, want the openai backend on the fake upstream", plan.BaseURL(), plan.BackendProtocol())
	}
	if _, err := PlanTarget(h.svc, PlanInput{Runtime: h.snapshot(cfg), Target: RouteTarget{Provider: "ghost"}}); err == nil {
		t.Error("unknown provider must fail the plan")
	}
}

// TestRequestRoutingSchedulerDelegates: the scheduler port passes the captured
// config/identity/generation through to the injected schedule func.
func TestRequestRoutingSchedulerDelegates(t *testing.T) {
	h := newHarness()
	var gotGeneration uint64
	var gotRoute string
	h.svc.Schedule = func(cfg *Config, parentOf map[string]string, exposed, sessionKey string, targets []RouteTarget, routeKeys map[string]bool, generation uint64) []RouteTarget {
		gotGeneration = generation
		gotRoute = exposed
		return targets
	}
	p := pipeline{svc: h.svc, state: h.state}
	snap := h.snapshot(&Config{Routes: map[string][]RouteTarget{"m": {{Provider: "a"}}}})
	snap.Generation = 41
	scheduler := requestRoutingScheduler{
		pipe: p, config: snap.Cfg, parentOf: snap.ParentOf,
		routeKeys: snap.RouteKeys, generation: snap.Generation,
	}
	out := scheduler.Schedule("m", "sess", []RouteTarget{{Provider: "a"}})
	if gotGeneration != 41 || gotRoute != "m" || len(out) != 1 {
		t.Errorf("schedule delegation = gen %d route %q out %+v", gotGeneration, gotRoute, out)
	}

	// The planner constructor projects the same snapshot.
	planner := requestRoutingPlanner(p, snap, snap.RouteKeys)
	if got := planner.ApplyWithProfile("m", "sess", []RouteTarget{{Provider: "a"}}, routing.Profile{}); len(got) != 1 {
		t.Errorf("planner apply = %+v, want passthrough for a fitting target", got)
	}
}

// TestExpandFusionResponsesWithStore: a responses client on a stateless
// backend expands the body and returns history; a previous_response_id miss
// repairs orphaned continuation items.
func TestExpandFusionResponsesWithStore(t *testing.T) {
	h := newHarness()
	h.svc.ResponsesState = protocol.NewResponsesStateStore(t.TempDir() + "/state.json")
	p := pipeline{svc: h.svc, state: h.state}
	fc := fusionCtx{proto: "responses", sessionKey: "sess-x", flc: LogCtx{Exposed: "m"}}
	body := []byte(`{"model":"m","input":"hello","previous_response_id":"resp_gone"}`)
	expanded, history := p.expandFusionResponses(fc, "openai", body)
	if len(expanded) == 0 {
		t.Fatal("expansion returned an empty body")
	}
	if history == nil {
		t.Error("stateless backend with a session must return merged history")
	}
}
