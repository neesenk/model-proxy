package forward

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	responsecache "model-proxy/internal/cache"
	configdomain "model-proxy/internal/config"
)

func TestApplyRouteLatchActive(t *testing.T) {
	state := &fakeRouteState{latch: map[string]Latch{
		"sess": {Target: "b/mb", Since: time.Now(), BadRuns: 0},
	}}
	ordered := []RouteTarget{{Provider: "a", Model: "ma"}, {Provider: "b", Model: "mb"}}
	policy := RoutePolicy{
		Bands:      []configdomain.RouteBand{{When: configdomain.BandWhen{HasImage: boolp(true)}, Target: RouteTarget{Provider: "a", Model: "ma"}}},
		Escalation: &configdomain.EscalationConfig{Target: RouteTarget{Provider: "b", Model: "mb"}},
	}
	got, decision, latched := applyRouteLatch(state, "sess", ordered, policy, nil, time.Now())
	if !latched || got[0].Provider != "b" {
		t.Fatalf("latched order = %+v, want b first", got)
	}
	if decision == nil || decision.Source != "latch" || decision.Latch != "b/mb" || decision.Target != "b/mb" {
		t.Fatalf("latch decision = %+v, want source=latch latch=b/mb", decision)
	}
}

func TestApplyRouteLatchExpired(t *testing.T) {
	state := &fakeRouteState{latch: map[string]Latch{
		"sess": {Target: "b/mb", Since: time.Now().Add(-time.Hour), BadRuns: 0},
	}}
	ordered := []RouteTarget{{Provider: "a", Model: "ma"}, {Provider: "b", Model: "mb"}}
	policy := RoutePolicy{Escalation: &configdomain.EscalationConfig{Dwell: "30m", Target: RouteTarget{Provider: "b", Model: "mb"}}}
	got, decision, latched := applyRouteLatch(state, "sess", ordered, policy, nil, time.Now())
	if latched {
		t.Fatalf("expired latch should be ignored, got %+v", got)
	}
	if decision != nil {
		t.Fatalf("expired latch decision = %+v, want nil", decision)
	}
}

func TestApplyRouteLatchMissing(t *testing.T) {
	state := &fakeRouteState{latch: map[string]Latch{}}
	ordered := []RouteTarget{{Provider: "a", Model: "ma"}}
	policy := RoutePolicy{Escalation: &configdomain.EscalationConfig{Target: RouteTarget{Provider: "b", Model: "mb"}}}
	got, decision, latched := applyRouteLatch(state, "sess", ordered, policy, nil, time.Now())
	if latched || len(got) != 1 || got[0].Provider != "a" {
		t.Fatalf("missing latch should leave order unchanged, got %+v", got)
	}
	if decision != nil {
		t.Fatalf("missing latch decision = %+v, want nil", decision)
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
	h.state.SetLatch("sess-z", Latch{Target: "b/mb", Since: time.Now(), BadRuns: 0}, snap.Generation)

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
	latch, ok := h.state.LatchValue("sess-empty")
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
	if _, ok := h.state.LatchValue("sess-nonempty"); ok {
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
	if _, ok := h.state.LatchValue("sess-rep"); ok {
		t.Fatal("first request must not be treated as repeat")
	}

	// Second identical request: repeat within dwell window -> latch.
	if w := h.serveWithSession(snap, "openai", "/v1/chat/completions", body, "sess-rep"); w.Code != http.StatusOK {
		t.Fatalf("second status = %d, want 200", w.Code)
	}
	latch, ok := h.state.LatchValue("sess-rep")
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
	if _, ok := h.state.LatchValue("sess-diff"); ok {
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
	if _, ok := h.state.LatchValue("sess-stream"); ok {
		t.Fatal("streaming body with framing bytes must not trigger empty_ok")
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
