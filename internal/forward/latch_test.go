package forward

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	responsecache "model-proxy/internal/cache"
	configdomain "model-proxy/internal/config"
)

func TestApplyRouteLatchActive(t *testing.T) {
	state := newFakeRouteState(1)
	state.SetLatch("sess", "m", Latch{Target: "b/mb", Since: time.Now(), BadRuns: 0}, 1)
	ordered := []RouteTarget{{Provider: "a", Model: "ma"}, {Provider: "b", Model: "mb"}}
	policy := RoutePolicy{
		Bands:      []configdomain.RouteBand{{When: configdomain.BandWhen{HasImage: boolp(true)}, Target: RouteTarget{Provider: "a", Model: "ma"}}},
		Escalation: &configdomain.EscalationConfig{Target: RouteTarget{Provider: "b", Model: "mb"}},
	}
	got, decision, latched := applyRouteLatch(state, "sess", "m", ordered, policy, nil, time.Now())
	if !latched || got[0].Provider != "b" {
		t.Fatalf("latched order = %+v, want b first", got)
	}
	if decision == nil || decision.Source != "latch" || decision.Latch != "b/mb" || decision.Target != "b/mb" {
		t.Fatalf("latch decision = %+v, want source=latch latch=b/mb", decision)
	}
}

func TestApplyRouteLatchExpired(t *testing.T) {
	state := newFakeRouteState(1)
	state.SetLatch("sess", "m", Latch{Target: "b/mb", Since: time.Now().Add(-time.Hour), BadRuns: 0}, 1)
	ordered := []RouteTarget{{Provider: "a", Model: "ma"}, {Provider: "b", Model: "mb"}}
	policy := RoutePolicy{Escalation: &configdomain.EscalationConfig{Dwell: "30m", Target: RouteTarget{Provider: "b", Model: "mb"}}}
	got, decision, latched := applyRouteLatch(state, "sess", "m", ordered, policy, nil, time.Now())
	if latched {
		t.Fatalf("expired latch should be ignored, got %+v", got)
	}
	if decision != nil {
		t.Fatalf("expired latch decision = %+v, want nil", decision)
	}
}

func TestApplyRouteLatchMissing(t *testing.T) {
	state := newFakeRouteState(1)
	ordered := []RouteTarget{{Provider: "a", Model: "ma"}}
	policy := RoutePolicy{Escalation: &configdomain.EscalationConfig{Target: RouteTarget{Provider: "b", Model: "mb"}}}
	got, decision, latched := applyRouteLatch(state, "sess", "m", ordered, policy, nil, time.Now())
	if latched || len(got) != 1 || got[0].Provider != "a" {
		t.Fatalf("missing latch should leave order unchanged, got %+v", got)
	}
	if decision != nil {
		t.Fatalf("missing latch decision = %+v, want nil", decision)
	}
}

// TestApplyRouteLatchTargetAbsent: the latch target was filtered out of the
// current ordered set (capability narrowing or operator disable). The latch
// must report no hit — order unchanged, no latch decision, latched=false — so
// bands/selector still run (same strictness as resolveLatchGrade).
func TestApplyRouteLatchTargetAbsent(t *testing.T) {
	state := newFakeRouteState(1)
	state.SetLatch("sess", "m", Latch{Target: "b/mb", Since: time.Now(), BadRuns: 0}, 1)
	ordered := []RouteTarget{{Provider: "a", Model: "ma"}}
	policy := RoutePolicy{Escalation: &configdomain.EscalationConfig{Target: RouteTarget{Provider: "b", Model: "mb"}}}
	got, decision, latched := applyRouteLatch(state, "sess", "m", ordered, policy, nil, time.Now())
	if latched {
		t.Fatalf("latch target absent from ordered must not latch, got %+v", got)
	}
	if decision != nil {
		t.Fatalf("absent latch decision = %+v, want nil", decision)
	}
	if len(got) != 1 || got[0].Provider != "a" {
		t.Fatalf("order changed: %+v, want unchanged", got)
	}
}

// routePolicyLatchSnapshot builds a route with a never-matching band and an
// escalation latch to provider b.
func routePolicyLatchSnapshot(t *testing.T, h *harness) (Snapshot, *fakeUpstream, *fakeUpstream) {
	upA := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	})
	var bCalls atomic.Int32
	upB := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if bCalls.Add(1) <= 2 {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	})
	cfg := &Config{
		Providers: map[string]Provider{
			"a": {OpenAIBaseURL: upA.srv.URL, Provider: "test-static"},
			"b": {OpenAIBaseURL: upB.srv.URL, Provider: "test-static"},
		},
		Routes: map[string][]RouteTarget{
			"m": {{Provider: "a", Model: "ma"}, {Provider: "b", Model: "mb"}},
		},
		RoutePolicies: map[string]RoutePolicy{
			"m": {
				Bands: []configdomain.RouteBand{{
					When:   configdomain.BandWhen{HasImage: boolp(true)},
					Target: RouteTarget{Provider: "a", Model: "ma"},
				}},
				Escalation: &configdomain.EscalationConfig{
					BadSignals:  []string{"upstream_error"},
					Consecutive: 2,
					Target:      RouteTarget{Provider: "b", Model: "mb"},
					Dwell:       "30m",
				},
			},
		},
	}
	return h.snapshot(cfg), upA, upB
}

func TestServeLatchEscalatesAfterConsecutiveBadRuns(t *testing.T) {
	h := newHarness()
	snap, upA, upB := routePolicyLatchSnapshot(t, h)
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`

	for i := 0; i < 2; i++ {
		w := h.serveWithSession(snap, "openai", "/v1/chat/completions", body, "sess-x")
		if w.Code != http.StatusBadGateway {
			t.Fatalf("request %d: status = %d, want 502", i, w.Code)
		}
	}
	if upA.hits() != 2 || upB.hits() != 2 {
		t.Fatalf("after two bad runs: a=%d b=%d, want 2/2", upA.hits(), upB.hits())
	}

	// Third request should latch to b and succeed on the first try.
	w := h.serveWithSession(snap, "openai", "/v1/chat/completions", body, "sess-x")
	if w.Code != http.StatusOK {
		t.Fatalf("third status = %d, want 200", w.Code)
	}
	if upA.hits() != 2 {
		t.Fatalf("latch did not skip a: a=%d, want 2", upA.hits())
	}
	if upB.hits() != 3 {
		t.Fatalf("latch did not hit b first: b=%d, want 3", upB.hits())
	}
}

func TestServeLatchGoodRunResetsCounter(t *testing.T) {
	h := newHarness()
	upA := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	})
	var bCalls atomic.Int32
	upB := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		// fail on calls 1 and 3, succeed on calls 2 and 4.
		if n := bCalls.Add(1); n == 1 || n == 3 {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	})
	cfg := &Config{
		Providers: map[string]Provider{
			"a": {OpenAIBaseURL: upA.srv.URL, Provider: "test-static"},
			"b": {OpenAIBaseURL: upB.srv.URL, Provider: "test-static"},
		},
		Routes: map[string][]RouteTarget{
			"m": {{Provider: "a", Model: "ma"}, {Provider: "b", Model: "mb"}},
		},
		RoutePolicies: map[string]RoutePolicy{
			"m": {
				Bands: []configdomain.RouteBand{{
					When:   configdomain.BandWhen{HasImage: boolp(true)},
					Target: RouteTarget{Provider: "a", Model: "ma"},
				}},
				Escalation: &configdomain.EscalationConfig{
					BadSignals:  []string{"upstream_error"},
					Consecutive: 2,
					Target:      RouteTarget{Provider: "b", Model: "mb"},
					Dwell:       "30m",
				},
			},
		},
	}
	snap := h.snapshot(cfg)
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`

	// 1: bad run (a fail, b fail call 1).
	if w := h.serveWithSession(snap, "openai", "/v1/chat/completions", body, "sess-y"); w.Code != http.StatusBadGateway {
		t.Fatalf("first status = %d, want 502", w.Code)
	}
	// 2: good run (a fail, b succeed call 2) -> resets counter.
	if w := h.serveWithSession(snap, "openai", "/v1/chat/completions", body, "sess-y"); w.Code != http.StatusOK {
		t.Fatalf("second status = %d, want 200", w.Code)
	}
	// 3: single bad run after reset (a fail, b fail call 3) -> not enough.
	if w := h.serveWithSession(snap, "openai", "/v1/chat/completions", body, "sess-y"); w.Code != http.StatusBadGateway {
		t.Fatalf("third status = %d, want 502", w.Code)
	}
	// 4: still no latch, so it tries a first before b (call 4 succeeds).
	if w := h.serveWithSession(snap, "openai", "/v1/chat/completions", body, "sess-y"); w.Code != http.StatusOK {
		t.Fatalf("fourth status = %d, want 200", w.Code)
	}
	if upA.hits() != 4 {
		t.Fatalf("a was skipped by unexpected latch: a=%d, want 4", upA.hits())
	}
}

func TestServeLatchSkippedForForceProviderAndPin(t *testing.T) {
	h := newHarness()
	upA := newFakeUpstream(t, openaiOKResponder("from-a"))
	upB := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	})
	cfg := &Config{
		Providers: map[string]Provider{
			"a": {OpenAIBaseURL: upA.srv.URL, Provider: "test-static"},
			"b": {OpenAIBaseURL: upB.srv.URL, Provider: "test-static"},
		},
		Routes: map[string][]RouteTarget{
			"m": {{Provider: "a", Model: "ma"}, {Provider: "b", Model: "mb"}},
		},
		RoutePolicies: map[string]RoutePolicy{
			"m": {
				Bands: []configdomain.RouteBand{{
					When:   configdomain.BandWhen{HasImage: boolp(true)},
					Target: RouteTarget{Provider: "a", Model: "ma"},
				}},
				Escalation: &configdomain.EscalationConfig{
					BadSignals:  []string{"upstream_error"},
					Consecutive: 1,
					Target:      RouteTarget{Provider: "b", Model: "mb"},
					Dwell:       "30m",
				},
			},
		},
	}
	snap := h.snapshot(cfg)
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	// Without force/pin the latch would send the next request straight to b.
	h.state.SetLatch("sess-z", "m", Latch{Target: "b/mb", Since: time.Now(), BadRuns: 0}, snap.Generation)

	// force-provider a must skip the latch and hit a only.
	w := h.serveWithSession(snap, "openai", "/v1/chat/completions", body, "sess-z", "x-mp-force-provider", "a")
	if w.Code != http.StatusOK {
		t.Fatalf("force-provider status = %d, want 200", w.Code)
	}
	if upA.hits() != 1 || upB.hits() != 0 {
		t.Fatalf("force-provider: a=%d b=%d, want 1/0", upA.hits(), upB.hits())
	}

	// pin on a must also skip the latch.
	h.state.pins["m"] = true
	w = h.serveWithSession(snap, "openai", "/v1/chat/completions", body, "sess-z")
	if w.Code != http.StatusOK {
		t.Fatalf("pin status = %d, want 200", w.Code)
	}
	if upA.hits() != 2 || upB.hits() != 0 {
		t.Fatalf("pin: a=%d b=%d, want 2/0", upA.hits(), upB.hits())
	}
}

func TestServeEscalationBypassesResponseCache(t *testing.T) {
	h := newHarness()
	up := newFakeUpstream(t, openaiOKResponder("cached"))
	cfg := &Config{
		Providers: map[string]Provider{"up": {OpenAIBaseURL: up.srv.URL, Provider: "test-static"}},
		Routes:    map[string][]RouteTarget{"m": {{Provider: "up", Model: "real"}}},
		RoutePolicies: map[string]RoutePolicy{
			"m": {
				Bands: []configdomain.RouteBand{{
					When:   configdomain.BandWhen{HasImage: boolp(true)},
					Target: RouteTarget{Provider: "up", Model: "real"},
				}},
				Escalation: &configdomain.EscalationConfig{
					BadSignals:  []string{"upstream_error"},
					Consecutive: 2,
					Target:      RouteTarget{Provider: "up", Model: "real"},
				},
			},
		},
	}
	snap := h.snapshot(cfg)
	snap.Cache = responsecache.New(responsecache.Options{TTL: time.Hour, MaxEntries: 8, MaxBodyBytes: 1 << 16})
	body := openaiChatBody()

	if w := h.serve(snap, "openai", "/v1/chat/completions", body, nil); w.Code != 200 {
		t.Fatalf("first status = %d", w.Code)
	}
	w := h.serve(snap, "openai", "/v1/chat/completions", body, nil)
	if w.Header().Get("x-mp-cache") == "hit" {
		t.Fatal("escalation route must bypass response cache")
	}
	if up.hits() != 2 {
		t.Fatalf("upstream hits = %d, want 2 (no cache hit)", up.hits())
	}
}

func TestServeEmptyOKTriggersLatch(t *testing.T) {
	h := newHarness()
	up := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.Header().Set("content-length", "0")
		w.WriteHeader(http.StatusOK)
	})
	cfg := &Config{
		Providers: map[string]Provider{"up": {OpenAIBaseURL: up.srv.URL, Provider: "test-static"}},
		Routes:    map[string][]RouteTarget{"m": {{Provider: "up", Model: "real"}}},
		RoutePolicies: map[string]RoutePolicy{
			"m": {
				Escalation: &configdomain.EscalationConfig{
					BadSignals:  []string{"empty_ok"},
					Consecutive: 1,
					Target:      RouteTarget{Provider: "up", Model: "real"},
					Dwell:       "30m",
				},
			},
		},
	}
	snap := h.snapshot(cfg)
	body := openaiChatBody()

	// First request: empty body commits, counts as one bad signal -> latch.
	if w := h.serveWithSession(snap, "openai", "/v1/chat/completions", body, "sess-empty"); w.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200", w.Code)
	}
	latch, ok := h.state.LatchValue("sess-empty", "m")
	if !ok || latch.Target != "up/real" || latch.BadRuns != 0 {
		t.Fatalf("latch = %+v ok=%v, want target=up/real badRuns=0", latch, ok)
	}
}

func TestServeEmptyOKNotTriggeredForNormalBody(t *testing.T) {
	h := newHarness()
	up := newFakeUpstream(t, openaiOKResponder("hi"))
	cfg := &Config{
		Providers: map[string]Provider{"up": {OpenAIBaseURL: up.srv.URL, Provider: "test-static"}},
		Routes:    map[string][]RouteTarget{"m": {{Provider: "up", Model: "real"}}},
		RoutePolicies: map[string]RoutePolicy{
			"m": {
				Escalation: &configdomain.EscalationConfig{
					BadSignals:  []string{"empty_ok"},
					Consecutive: 1,
					Target:      RouteTarget{Provider: "up", Model: "real"},
					Dwell:       "30m",
				},
			},
		},
	}
	snap := h.snapshot(cfg)
	body := openaiChatBody()

	if w := h.serveWithSession(snap, "openai", "/v1/chat/completions", body, "sess-nonempty"); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if _, ok := h.state.LatchValue("sess-nonempty", "m"); ok {
		t.Fatal("normal body must not trigger empty_ok latch")
	}
}

func TestServeRepeatTurnTriggersLatch(t *testing.T) {
	h := newHarness()
	up := newFakeUpstream(t, openaiOKResponder("ok"))
	cfg := &Config{
		Providers: map[string]Provider{"up": {OpenAIBaseURL: up.srv.URL, Provider: "test-static"}},
		Routes:    map[string][]RouteTarget{"m": {{Provider: "up", Model: "real"}}},
		RoutePolicies: map[string]RoutePolicy{
			"m": {
				Escalation: &configdomain.EscalationConfig{
					BadSignals:  []string{"repeat_turn"},
					Consecutive: 1,
					Target:      RouteTarget{Provider: "up", Model: "real"},
					Dwell:       "30m",
				},
			},
		},
	}
	snap := h.snapshot(cfg)
	body := openaiChatBody()

	// First request: not a repeat.
	if w := h.serveWithSession(snap, "openai", "/v1/chat/completions", body, "sess-rep"); w.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200", w.Code)
	}
	if _, ok := h.state.LatchValue("sess-rep", "m"); ok {
		t.Fatal("first request must not be treated as repeat")
	}

	// Second identical request: repeat within dwell window -> latch.
	if w := h.serveWithSession(snap, "openai", "/v1/chat/completions", body, "sess-rep"); w.Code != http.StatusOK {
		t.Fatalf("second status = %d, want 200", w.Code)
	}
	latch, ok := h.state.LatchValue("sess-rep", "m")
	if !ok || latch.Target != "up/real" {
		t.Fatalf("repeat did not latch: %+v ok=%v", latch, ok)
	}
}

func TestServeRepeatTurnDifferentTurnDoesNotTrigger(t *testing.T) {
	h := newHarness()
	up := newFakeUpstream(t, openaiOKResponder("ok"))
	cfg := &Config{
		Providers: map[string]Provider{"up": {OpenAIBaseURL: up.srv.URL, Provider: "test-static"}},
		Routes:    map[string][]RouteTarget{"m": {{Provider: "up", Model: "real"}}},
		RoutePolicies: map[string]RoutePolicy{
			"m": {
				Escalation: &configdomain.EscalationConfig{
					BadSignals:  []string{"repeat_turn"},
					Consecutive: 1,
					Target:      RouteTarget{Provider: "up", Model: "real"},
					Dwell:       "30m",
				},
			},
		},
	}
	snap := h.snapshot(cfg)

	body1 := `{"model":"m","messages":[{"role":"user","content":"first"}]}`
	body2 := `{"model":"m","messages":[{"role":"user","content":"second"}]}`

	if w := h.serveWithSession(snap, "openai", "/v1/chat/completions", body1, "sess-diff"); w.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200", w.Code)
	}
	if w := h.serveWithSession(snap, "openai", "/v1/chat/completions", body2, "sess-diff"); w.Code != http.StatusOK {
		t.Fatalf("second status = %d, want 200", w.Code)
	}
	if _, ok := h.state.LatchValue("sess-diff", "m"); ok {
		t.Fatal("different turns must not trigger repeat_turn latch")
	}
}

func TestServeEmptyOKWithStreamDoesNotTrigger(t *testing.T) {
	h := newHarness()
	up := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Empty content frames still carry SSE framing bytes, so the final
		// client-facing body is non-zero and must NOT be flagged as empty_ok.
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"\"}}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	})
	cfg := &Config{
		Providers: map[string]Provider{"up": {OpenAIBaseURL: up.srv.URL, Provider: "test-static"}},
		Routes:    map[string][]RouteTarget{"m": {{Provider: "up", Model: "real"}}},
		RoutePolicies: map[string]RoutePolicy{
			"m": {
				Escalation: &configdomain.EscalationConfig{
					BadSignals:  []string{"empty_ok"},
					Consecutive: 1,
					Target:      RouteTarget{Provider: "up", Model: "real"},
					Dwell:       "30m",
				},
			},
		},
	}
	snap := h.snapshot(cfg)
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`

	if w := h.serveWithSession(snap, "openai", "/v1/chat/completions", body, "sess-stream"); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if _, ok := h.state.LatchValue("sess-stream", "m"); ok {
		t.Fatal("streaming body with framing bytes must not trigger empty_ok")
	}
}

// TestServeLatchScopedToRoute: the escalation latch is keyed by (session,
// route), same as the repeat_turn window. A latch escalated on route ra must
// NOT front the same target on route rb — even when the session is identical
// and rb's ordered set contains the latch target.
func TestServeLatchScopedToRoute(t *testing.T) {
	h := newHarness()
	upA := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	})
	upB := newFakeUpstream(t, openaiOKResponder("from-b"))
	upC := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	})
	escalation := func() *configdomain.EscalationConfig {
		return &configdomain.EscalationConfig{
			BadSignals:  []string{"upstream_error"},
			Consecutive: 1,
			Target:      RouteTarget{Provider: "b", Model: "mb"},
			Dwell:       "30m",
		}
	}
	cfg := &Config{
		Providers: map[string]Provider{
			"a": {OpenAIBaseURL: upA.srv.URL, Provider: "test-static"},
			"b": {OpenAIBaseURL: upB.srv.URL, Provider: "test-static"},
			"c": {OpenAIBaseURL: upC.srv.URL, Provider: "test-static"},
		},
		Routes: map[string][]RouteTarget{
			"ra": {{Provider: "a", Model: "ma"}},
			"rb": {{Provider: "c", Model: "mc"}, {Provider: "b", Model: "mb"}},
		},
		RoutePolicies: map[string]RoutePolicy{
			"ra": {Escalation: escalation()},
			"rb": {Escalation: escalation()},
		},
	}
	snap := h.snapshot(cfg)

	// One bad run on route ra escalates the session latch to b/mb (consecutive=1).
	w := h.serveWithSession(snap, "openai", "/v1/chat/completions", `{"model":"ra","messages":[{"role":"user","content":"hi"}]}`, "sess-iso")
	if w.Code != http.StatusBadGateway {
		t.Fatalf("route ra status = %d, want 502", w.Code)
	}

	// Same session on route rb: the latch taken on ra must NOT front b/mb here —
	// rb starts with its natural first target c and fails over to b.
	w = h.serveWithSession(snap, "openai", "/v1/chat/completions", `{"model":"rb","messages":[{"role":"user","content":"hi"}]}`, "sess-iso")
	if w.Code != http.StatusOK {
		t.Fatalf("route rb status = %d, want 200", w.Code)
	}
	if upC.hits() != 1 {
		t.Fatalf("route rb must try its own first target first: c hits = %d, want 1 (latch from route ra leaked)", upC.hits())
	}
	if upB.hits() != 1 {
		t.Fatalf("route rb failover should reach b once: b hits = %d, want 1", upB.hits())
	}
}

// TestServeLatchBadRunsScopedToRoute: the bad-run streak is keyed by (session,
// route) — a good run on route rb must NOT reset the streak route ra is
// accumulating toward its escalation threshold.
func TestServeLatchBadRunsScopedToRoute(t *testing.T) {
	h := newHarness()
	upA := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	})
	var bCalls atomic.Int32
	upB := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		// Fail the first two calls (route ra's two bad runs), succeed after.
		if bCalls.Add(1) <= 2 {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	})
	upC := newFakeUpstream(t, openaiOKResponder("from-c"))
	cfg := &Config{
		Providers: map[string]Provider{
			"a": {OpenAIBaseURL: upA.srv.URL, Provider: "test-static"},
			"b": {OpenAIBaseURL: upB.srv.URL, Provider: "test-static"},
			"c": {OpenAIBaseURL: upC.srv.URL, Provider: "test-static"},
		},
		Routes: map[string][]RouteTarget{
			"ra": {{Provider: "a", Model: "ma"}, {Provider: "b", Model: "mb"}},
			"rb": {{Provider: "c", Model: "mc"}},
		},
		RoutePolicies: map[string]RoutePolicy{
			"ra": {Escalation: &configdomain.EscalationConfig{
				BadSignals:  []string{"upstream_error"},
				Consecutive: 2,
				Target:      RouteTarget{Provider: "b", Model: "mb"},
				Dwell:       "30m",
			}},
			"rb": {Escalation: &configdomain.EscalationConfig{
				BadSignals:  []string{"upstream_error"},
				Consecutive: 2,
				Target:      RouteTarget{Provider: "c", Model: "mc"},
				Dwell:       "30m",
			}},
		},
	}
	snap := h.snapshot(cfg)
	raBody := `{"model":"ra","messages":[{"role":"user","content":"hi"}]}`
	rbBody := `{"model":"rb","messages":[{"role":"user","content":"hi"}]}`

	// 1: first bad run on ra (a down, b call 1 down) -> streak 1.
	if w := h.serveWithSession(snap, "openai", "/v1/chat/completions", raBody, "sess-br"); w.Code != http.StatusBadGateway {
		t.Fatalf("first ra status = %d, want 502", w.Code)
	}
	// Good run on rb (c ok) -> must NOT reset ra's streak.
	if w := h.serveWithSession(snap, "openai", "/v1/chat/completions", rbBody, "sess-br"); w.Code != http.StatusOK {
		t.Fatalf("rb status = %d, want 200", w.Code)
	}
	// 2: second bad run on ra (a down, b call 2 down) -> streak reaches 2 -> latch b/mb.
	if w := h.serveWithSession(snap, "openai", "/v1/chat/completions", raBody, "sess-br"); w.Code != http.StatusBadGateway {
		t.Fatalf("second ra status = %d, want 502", w.Code)
	}
	// 3: latched request on ra fronts b (call 3 ok) without hitting a again.
	if w := h.serveWithSession(snap, "openai", "/v1/chat/completions", raBody, "sess-br"); w.Code != http.StatusOK {
		t.Fatalf("third ra status = %d, want 200", w.Code)
	}
	if upA.hits() != 2 {
		t.Fatalf("a hits = %d, want 2 (streak reset by route rb good run leaked; latch never escalated)", upA.hits())
	}
}

// TestServeConversionFailureNotCountedAsBadRun: a client-shape conversion
// failure (400 — the request cannot be represented for ANY candidate and no
// upstream was ever contacted) must NOT feed the escalation upstream_error
// signal. It is the client's problem, same as a guard interception, not an
// upstream bad run; retrying the same unconvertible request must never
// escalate a latch.
func TestServeConversionFailureNotCountedAsBadRun(t *testing.T) {
	h := newHarness()
	anthropicOK := func(text string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("content-type", "application/json")
			io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":`+strconv_(text)+`}],"usage":{"input_tokens":1,"output_tokens":1}}`)
		}
	}
	upA := newFakeUpstream(t, anthropicOK("from-a"))
	upB := newFakeUpstream(t, anthropicOK("from-b"))
	cfg := &Config{
		Providers: map[string]Provider{
			"a": {AnthropicBaseURL: upA.srv.URL, Provider: "test-static"},
			"b": {AnthropicBaseURL: upB.srv.URL, Provider: "test-static"},
		},
		Routes: map[string][]RouteTarget{
			"m": {{Provider: "a", Model: "ma", Protocol: "anthropic"}, {Provider: "b", Model: "mb", Protocol: "anthropic"}},
		},
		RoutePolicies: map[string]RoutePolicy{
			"m": {Escalation: &configdomain.EscalationConfig{
				BadSignals:  []string{"upstream_error"},
				Consecutive: 2,
				Target:      RouteTarget{Provider: "b", Model: "mb"},
				Dwell:       "30m",
			}},
		},
	}
	snap := h.snapshot(cfg)
	// n=2 cannot be represented by any anthropic backend: every candidate
	// fails conversion, no upstream is contacted, the client gets a 400.
	badBody := `{"model":"m","n":2,"messages":[{"role":"user","content":"hi"}]}`
	for i := 0; i < 2; i++ {
		w := h.serveWithSession(snap, "openai", "/v1/chat/completions", badBody, "sess-conv")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("unconvertible request %d: status = %d, want 400 (body %s)", i, w.Code, w.Body.String())
		}
	}
	if upA.hits() != 0 || upB.hits() != 0 {
		t.Fatalf("unconvertible request must not contact upstreams: a=%d b=%d", upA.hits(), upB.hits())
	}
	// A convertible request from the same session must take the natural order —
	// the 400s above must not have escalated a latch to b/mb.
	w := h.serveWithSession(snap, "openai", "/v1/chat/completions", openaiChatBody(), "sess-conv")
	if w.Code != http.StatusOK {
		t.Fatalf("convertible status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if upA.hits() != 1 || upB.hits() != 0 {
		t.Fatalf("client-shape 400s escalated a latch: a=%d b=%d, want 1/0 (natural order)", upA.hits(), upB.hits())
	}
}

// TestServeLatchConcurrentBadRunsCountedAtomically: concurrent requests of the
// same (session, route) each contribute exactly one bad run — the outcome
// recording must be one atomic read-modify-write per request, so the final
// streak equals the number of requests (no lost updates).
func TestServeLatchConcurrentBadRunsCountedAtomically(t *testing.T) {
	h := newHarness()
	up := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	})
	cfg := &Config{
		Providers: map[string]Provider{"up": {OpenAIBaseURL: up.srv.URL, Provider: "test-static"}},
		Routes:    map[string][]RouteTarget{"m": {{Provider: "up", Model: "real"}}},
		RoutePolicies: map[string]RoutePolicy{
			"m": {Escalation: &configdomain.EscalationConfig{
				BadSignals:  []string{"upstream_error"},
				Consecutive: 1 << 30, // never escalates: every bad run stays in BadRuns
				Target:      RouteTarget{Provider: "up", Model: "real"},
				Dwell:       "30m",
			}},
		},
	}
	snap := h.snapshot(cfg)
	body := openaiChatBody()
	const n = 32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if w := h.serveWithSession(snap, "openai", "/v1/chat/completions", body, "sess-conc"); w.Code != http.StatusBadGateway {
				t.Errorf("status = %d, want 502", w.Code)
			}
		}()
	}
	wg.Wait()
	latch, ok := h.state.LatchValue("sess-conc", "m")
	if !ok {
		t.Fatal("no latch recorded for concurrent bad runs")
	}
	if latch.BadRuns != n {
		t.Fatalf("BadRuns = %d, want %d (lost updates in LatchValue->SetLatch read-modify-write)", latch.BadRuns, n)
	}
}

func (h *harness) serveWithSession(snap Snapshot, proto, target, body, session string, extraHeaders ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	if session != "" {
		r.Header.Set("x-claude-code-session-id", session)
	}
	for i := 0; i+1 < len(extraHeaders); i += 2 {
		r.Header.Set(extraHeaders[i], extraHeaders[i+1])
	}
	w := httptest.NewRecorder()
	Serve(h.svc, h.state, snap, proto, w, r, "req-latch")
	return w
}

// TestServePlanningFailureNotCountedAsBadRun: a target whose plan cannot even
// be built (unknown provider config) is dropped BEFORE any upstream contact.
// With no target ever tried, the terminal 502 is a config-level failure, not
// an upstream verdict — it must not feed the escalation upstream_error signal
// (same exemption as the client-shape conversion failure above).
func TestServePlanningFailureNotCountedAsBadRun(t *testing.T) {
	h := newHarness()
	cfg := &Config{
		Providers: map[string]Provider{
			"a": {OpenAIBaseURL: "http://127.0.0.1:1", Provider: "test-static"},
		},
		Routes: map[string][]RouteTarget{
			"m": {{Provider: "ghost", Model: "mg"}},
		},
		RoutePolicies: map[string]RoutePolicy{
			"m": {Escalation: &configdomain.EscalationConfig{
				BadSignals:  []string{"upstream_error"},
				Consecutive: 1,
				Target:      RouteTarget{Provider: "a", Model: "ma"},
				Dwell:       "30m",
			}},
		},
	}
	snap := h.snapshot(cfg)
	body := openaiChatBody()
	for i := 0; i < 2; i++ {
		w := h.serveWithSession(snap, "openai", "/v1/chat/completions", body, "sess-plan")
		if w.Code != http.StatusBadGateway {
			t.Fatalf("request %d: status = %d, want 502", i, w.Code)
		}
	}
	if _, ok := h.state.LatchValue("sess-plan", "m"); ok {
		t.Fatal("planTarget failure (unknown provider) with no tried target must not escalate the latch")
	}
}

// TestServeNilProviderFailsClosedWithoutBadRun: a route target whose runtime
// provider implementation is missing (not logged in / unresolved pooled
// parent) must fail closed in serveOnce — before any upstream contact, without
// counting the target as tried or the outcome as an upstream bad run. This is
// the same explicit nil check the executor/decisions/fusion layers make, kept
// symmetric at the orchestration layer.
func TestServeNilProviderFailsClosedWithoutBadRun(t *testing.T) {
	h := newHarness()
	up := newFakeUpstream(t, openaiOKResponder("must-not-be-hit"))
	cfg := &Config{
		Providers: map[string]Provider{
			"a": {OpenAIBaseURL: up.srv.URL, Provider: "test-static"},
		},
		Routes: map[string][]RouteTarget{
			"m": {{Provider: "a", Model: "ma"}},
		},
		RoutePolicies: map[string]RoutePolicy{
			"m": {Escalation: &configdomain.EscalationConfig{
				BadSignals:  []string{"upstream_error"},
				Consecutive: 1,
				Target:      RouteTarget{Provider: "a", Model: "ma"},
				Dwell:       "30m",
			}},
		},
	}
	snap := h.snapshot(cfg)
	delete(snap.Providers, "a") // runtime implementation missing
	body := openaiChatBody()
	for i := 0; i < 2; i++ {
		w := h.serveWithSession(snap, "openai", "/v1/chat/completions", body, "sess-nil")
		if w.Code != http.StatusBadGateway {
			t.Fatalf("request %d: status = %d, want 502", i, w.Code)
		}
	}
	if up.hits() != 0 {
		t.Fatalf("nil provider must fail closed before any upstream contact: hits = %d", up.hits())
	}
	if _, ok := h.state.LatchValue("sess-nil", "m"); ok {
		t.Fatal("missing provider implementation must not escalate the latch")
	}
}

// TestRecordLatchOutcomePlanningErrExemption: the planning-level exemption is
// scoped to passes where NO upstream was ever tried — once a target was
// contacted, the pass is an honest upstream_error again.
func TestRecordLatchOutcomePlanningErrExemption(t *testing.T) {
	state := newFakeRouteState(1)
	p := pipeline{state: state}
	cfg := &Config{RoutePolicies: map[string]RoutePolicy{
		"m": {Escalation: &configdomain.EscalationConfig{
			BadSignals:  []string{"upstream_error"},
			Consecutive: 1,
			Target:      RouteTarget{Provider: "b", Model: "mb"},
			Dwell:       "30m",
		}},
	}}
	req := serveRequest{
		runtime:    Snapshot{Cfg: cfg, Generation: 1},
		sessionKey: "sess-plan-unit",
		exposed:    "m",
	}
	// Planning-level failure, nothing tried: no bad run recorded.
	p.recordLatchOutcome(req, serveResult{planningErr: true, tried: map[string]bool{}})
	if _, ok := state.LatchValue("sess-plan-unit", "m"); ok {
		t.Fatal("planning-level failure with no tried target must not record a bad run")
	}
	// A target WAS tried this pass: the terminal error counts (consecutive=1
	// escalates immediately).
	p.recordLatchOutcome(req, serveResult{planningErr: true, sawHard: true, tried: map[string]bool{"a": true}})
	latch, ok := state.LatchValue("sess-plan-unit", "m")
	if !ok || latch.Target != "b/mb" {
		t.Fatalf("tried upstream error must escalate: latch = %+v ok = %v", latch, ok)
	}
}
