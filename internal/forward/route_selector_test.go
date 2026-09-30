package forward

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	responsecache "model-proxy/internal/cache"
	configdomain "model-proxy/internal/config"
)

// selectorDecisionResponder returns an upstream responder that answers a
// System One decisions request with the given choice and confidence.
func selectorDecisionResponder(choice string, confidence float64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{
			"model": "jev-1.13.0",
			"answers": {
				"model_choice": {"type": "choice", "choice": "` + choice + `", "confidence": ` + strconv.FormatFloat(confidence, 'f', -1, 64) + `},
				"difficulty": {"type": "score", "score": 2.0, "confidence": 0.7}
			},
			"usage": {"input_tokens": 100, "output_tokens": 10}
		}`))
	}
}

// routeSelectorSnapshot builds a route with two concrete targets and a route
// selector that picks among them via the decisions provider.
func routeSelectorSnapshot(t *testing.T, h *harness, upA, upB, upSel *fakeUpstream) Snapshot {
	t.Helper()
	cfg := &Config{
		Providers: map[string]Provider{
			"a":   {OpenAIBaseURL: upA.srv.URL, Provider: "test-static"},
			"b":   {OpenAIBaseURL: upB.srv.URL, Provider: "test-static"},
			"sel": {DecisionsBaseURL: upSel.srv.URL + "/v1", Provider: "typesafe"},
		},
		Routes: map[string][]RouteTarget{
			"m": {{Provider: "a", Model: "ma"}, {Provider: "b", Model: "mb"}},
		},
		RoutePolicies: map[string]RoutePolicy{
			"m": {
				Selector: &SelectorConfig{
					Target:     RouteTarget{Provider: "sel", Model: "jev-1.13.0", Protocol: "decisions"},
					Mode:       "enforce",
					Confidence: 0.55,
					Timeout:    "800ms",
				},
			},
		},
	}
	return h.snapshot(cfg)
}

func TestServeRouteSelectorEnforcePrefersChoice(t *testing.T) {
	h := newHarness()
	upA := newFakeUpstream(t, openaiOKResponder("from-a"))
	upB := newFakeUpstream(t, openaiOKResponder("from-b"))
	upSel := newFakeUpstream(t, selectorDecisionResponder("c1", 0.87))
	snap := routeSelectorSnapshot(t, h, upA, upB, upSel)
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}"`

	w := h.serve(snap, "openai", "/v1/chat/completions", body, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if upB.hits() != 1 {
		t.Fatalf("selector chose c1 (provider b) but b hits = %d, want 1", upB.hits())
	}
	if upA.hits() != 0 {
		t.Fatalf("selector should have skipped a: a hits = %d, want 0", upA.hits())
	}
	if upSel.hits() != 1 {
		t.Fatalf("selector calls = %d, want 1", upSel.hits())
	}
	if r := h.fx.capturedRouting(); len(r) != 1 || r[0] == nil {
		t.Fatalf("routing decision missing: %+v", r)
	} else if r[0].Source != "selector" || r[0].Target != "b/mb" || r[0].Selector == nil || !r[0].Selector.Enforced {
		t.Fatalf("enforce routing = %+v, want source=selector enforced", r[0])
	}
}

func TestServeRouteSelectorLowConfidenceKeepsOrder(t *testing.T) {
	h := newHarness()
	upA := newFakeUpstream(t, openaiOKResponder("from-a"))
	upB := newFakeUpstream(t, openaiOKResponder("from-b"))
	upSel := newFakeUpstream(t, selectorDecisionResponder("c1", 0.54))
	snap := routeSelectorSnapshot(t, h, upA, upB, upSel)
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}"`

	w := h.serve(snap, "openai", "/v1/chat/completions", body, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if upA.hits() != 1 {
		t.Fatalf("low confidence should keep scheduled order: a hits = %d, want 1", upA.hits())
	}
	if r := h.fx.capturedRouting(); len(r) != 1 || r[0] == nil {
		t.Fatalf("routing decision missing: %+v", r)
	} else if r[0].Source != "fallback" || r[0].Selector == nil || r[0].Selector.Enforced {
		t.Fatalf("low-confidence routing = %+v, want source=fallback selector not enforced", r[0])
	}
}

func TestServeRouteSelectorFailureKeepsOrder(t *testing.T) {
	h := newHarness()
	upA := newFakeUpstream(t, openaiOKResponder("from-a"))
	upB := newFakeUpstream(t, openaiOKResponder("from-b"))
	upSel := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	snap := routeSelectorSnapshot(t, h, upA, upB, upSel)
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}"`

	w := h.serve(snap, "openai", "/v1/chat/completions", body, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if upA.hits() != 1 {
		t.Fatalf("selector failure should fall back to scheduled order: a hits = %d, want 1", upA.hits())
	}
	if r := h.fx.capturedRouting(); len(r) != 1 || r[0] == nil {
		t.Fatalf("routing decision missing: %+v", r)
	} else if r[0].Source != "fallback" || r[0].Selector == nil || r[0].Selector.Enforced || r[0].Selector.Err == "" {
		t.Fatalf("selector-failure routing = %+v, want source=fallback selector err set", r[0])
	}
}

func TestServeRouteSelectorShadowKeepsOrder(t *testing.T) {
	h := newHarness()
	upA := newFakeUpstream(t, openaiOKResponder("from-a"))
	upB := newFakeUpstream(t, openaiOKResponder("from-b"))
	upSel := newFakeUpstream(t, selectorDecisionResponder("c1", 0.87))
	snap := routeSelectorSnapshot(t, h, upA, upB, upSel)
	pol := snap.Cfg.RoutePolicies["m"]
	pol.Selector.Mode = "shadow"
	snap.Cfg.RoutePolicies["m"] = pol
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}"`

	w := h.serve(snap, "openai", "/v1/chat/completions", body, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if upA.hits() != 1 {
		t.Fatalf("shadow mode should not reorder: a hits = %d, want 1", upA.hits())
	}
	if upSel.hits() != 1 {
		t.Fatalf("shadow mode should still call selector: selector hits = %d, want 1", upSel.hits())
	}
	if r := h.fx.capturedRouting(); len(r) != 1 || r[0] == nil {
		t.Fatalf("routing decision missing: %+v", r)
	} else if r[0].Source != "fallback" || r[0].Selector == nil || r[0].Selector.Enforced {
		t.Fatalf("shadow routing = %+v, want source=fallback selector not enforced", r[0])
	}
}

func TestServeRouteSelectorBypassesResponseCache(t *testing.T) {
	h := newHarness()
	up := newFakeUpstream(t, openaiOKResponder("cached"))
	upSel := newFakeUpstream(t, selectorDecisionResponder("c0", 0.87))
	cfg := &Config{
		Providers: map[string]Provider{
			"up":  {OpenAIBaseURL: up.srv.URL, Provider: "test-static"},
			"sel": {DecisionsBaseURL: upSel.srv.URL + "/v1", Provider: "typesafe"},
		},
		Routes: map[string][]RouteTarget{
			"m": {{Provider: "up", Model: "real"}},
		},
		RoutePolicies: map[string]RoutePolicy{
			"m": {
				Selector: &SelectorConfig{
					Target:     RouteTarget{Provider: "sel", Model: "jev-1.13.0", Protocol: "decisions"},
					Mode:       "shadow",
					Confidence: 0.55,
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
		t.Fatal("selector route must bypass response cache")
	}
	if up.hits() != 2 {
		t.Fatalf("upstream hits = %d, want 2 (no cache hit)", up.hits())
	}
}

func TestServeRouteSelectorLatchWins(t *testing.T) {
	h := newHarness()
	upA := newFakeUpstream(t, openaiOKResponder("from-a"))
	upB := newFakeUpstream(t, openaiOKResponder("from-b"))
	upSel := newFakeUpstream(t, selectorDecisionResponder("c1", 0.87))
	snap := routeSelectorSnapshot(t, h, upA, upB, upSel)
	pol := snap.Cfg.RoutePolicies["m"]
	pol.Escalation = &configdomain.EscalationConfig{
		BadSignals:  []string{"upstream_error"},
		Consecutive: 1,
		Target:      RouteTarget{Provider: "a", Model: "ma"},
		Dwell:       "30m",
	}
	snap.Cfg.RoutePolicies["m"] = pol
	h.state.SetLatch("sess-sel", "m", Latch{Target: "a/ma", Since: time.Now(), BadRuns: 0}, snap.Generation)

	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}"`
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("x-claude-code-session-id", "sess-sel")
	w := httptest.NewRecorder()
	Serve(h.svc, h.state, snap, "openai", w, r, "req-sel")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if upA.hits() != 1 {
		t.Fatalf("active latch should send request to a: a hits = %d, want 1", upA.hits())
	}
	if upSel.hits() != 0 {
		t.Fatalf("active latch should skip selector: selector hits = %d, want 0", upSel.hits())
	}
}

// TestServeLatchTargetAbsentFallsThroughToSelector: a session latch whose
// target is not part of the current ordered set (e.g. the provider was
// disabled after the latch was taken) must NOT suppress the rest of the
// policy chain — the selector still runs and the decision must not be
// recorded as source=latch.
func TestServeLatchTargetAbsentFallsThroughToSelector(t *testing.T) {
	h := newHarness()
	upA := newFakeUpstream(t, openaiOKResponder("from-a"))
	upB := newFakeUpstream(t, openaiOKResponder("from-b"))
	upSel := newFakeUpstream(t, selectorDecisionResponder("c0", 0.9))
	snap := routeSelectorSnapshot(t, h, upA, upB, upSel)
	pol := snap.Cfg.RoutePolicies["m"]
	pol.Escalation = &configdomain.EscalationConfig{
		BadSignals:  []string{"upstream_error"},
		Consecutive: 1,
		Target:      RouteTarget{Provider: "ghost", Model: "mg"},
		Dwell:       "30m",
	}
	snap.Cfg.RoutePolicies["m"] = pol
	h.state.SetLatch("sess-absent", "m", Latch{Target: "ghost/mg", Since: time.Now(), BadRuns: 0}, snap.Generation)

	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}"`
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("x-claude-code-session-id", "sess-absent")
	w := httptest.NewRecorder()
	Serve(h.svc, h.state, snap, "openai", w, r, "req-absent")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if upSel.hits() != 1 {
		t.Fatalf("absent latch target must not suppress the selector: selector hits = %d, want 1", upSel.hits())
	}
	if upA.hits() != 1 {
		t.Fatalf("selector chose c0 (provider a): a hits = %d, want 1", upA.hits())
	}
	if r := h.fx.capturedRouting(); len(r) != 1 || r[0] == nil {
		t.Fatalf("routing decision missing: %+v", r)
	} else if r[0].Source != "selector" {
		t.Fatalf("routing = %+v, want source=selector (latch did not reorder anything)", r[0])
	}
}

// TestServeRouteSelectorCalledOnceAcrossCooldownRetries: the selector contract
// is "at most one decisions call per request". When every target is cooling
// and forward waits/retries, later rounds must reuse the first round's
// selector outcome instead of paying the decisions call again.
func TestServeRouteSelectorCalledOnceAcrossCooldownRetries(t *testing.T) {
	h := newHarness()
	rateLimited := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}
	upA := newFakeUpstream(t, rateLimited)
	upB := newFakeUpstream(t, rateLimited)
	upSel := newFakeUpstream(t, selectorDecisionResponder("c1", 0.9))
	snap := routeSelectorSnapshot(t, h, upA, upB, upSel)
	snap.Cfg.Scheduling.RetryWait = "2s"
	h.state.allDown = true
	h.state.allRateLimited = true
	h.state.earliest = time.Now().Add(5 * time.Millisecond)

	w := h.serve(snap, "openai", "/v1/chat/completions", openaiChatBody(), nil)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (all targets rate-limited)", w.Code)
	}
	if upA.hits()+upB.hits() < 3 {
		t.Fatalf("expected the wait-retry loop to run more than one pass: a=%d b=%d", upA.hits(), upB.hits())
	}
	if upSel.hits() != 1 {
		t.Fatalf("selector decisions calls = %d, want at most 1 per request", upSel.hits())
	}
	if ends := h.endEvents("route-select-req-test"); len(ends) != 1 {
		t.Fatalf("route-select live end events = %d, want 1 (no duplicate leg per retry round)", len(ends))
	}
}

// TestServeRouteGradeSelectorCalledOnceAcrossCooldownRetries: the graded
// selector variant shares the same at-most-once-per-request contract.
func TestServeRouteGradeSelectorCalledOnceAcrossCooldownRetries(t *testing.T) {
	h := newHarness()
	rateLimited := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}
	upA := newFakeUpstream(t, rateLimited)
	upB := newFakeUpstream(t, rateLimited)
	// g0 picks the first grade so the "any" fallback still appends the second
	// grade's target — both upstreams are tried every pass.
	upSel := newFakeUpstream(t, selectorDecisionResponder("g0", 0.9))
	snap := routeGradeSelectorSnapshot(t, h, upA, upB, upSel)
	snap.Cfg.Scheduling.RetryWait = "2s"
	h.state.allDown = true
	h.state.allRateLimited = true
	h.state.earliest = time.Now().Add(5 * time.Millisecond)

	w := h.serve(snap, "openai", "/v1/chat/completions", openaiChatBody(), nil)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (all targets rate-limited)", w.Code)
	}
	if upA.hits()+upB.hits() < 3 {
		t.Fatalf("expected the wait-retry loop to run more than one pass: a=%d b=%d", upA.hits(), upB.hits())
	}
	if upSel.hits() != 1 {
		t.Fatalf("grade selector decisions calls = %d, want at most 1 per request", upSel.hits())
	}
}

// TestServeRouteSelectorBeatsBand pins the documented precedence chain
// (latch > selector > bands > static order): a confident enforce-mode choice
// overrides a matching band's preference. Candidates are numbered in the
// post-band order, so the band puts b at c0 and the selector's c1 (a) must win.
// routeGradeSelectorSnapshot builds a route with two grades and a selector
// that picks among the grades via the decisions provider.
func routeGradeSelectorSnapshot(t *testing.T, h *harness, upA, upB, upSel *fakeUpstream) Snapshot {
	t.Helper()
	cfg := &Config{
		Providers: map[string]Provider{
			"a":   {OpenAIBaseURL: upA.srv.URL, Provider: "test-static"},
			"b":   {OpenAIBaseURL: upB.srv.URL, Provider: "test-static"},
			"sel": {DecisionsBaseURL: upSel.srv.URL + "/v1", Provider: "typesafe"},
		},
		Routes: map[string][]RouteTarget{
			"m": {{Provider: "a", Model: "ma"}, {Provider: "b", Model: "mb"}},
		},
		RoutePolicies: map[string]RoutePolicy{
			"m": {
				Grades: map[string][]RouteTarget{
					"fast":   {{Provider: "a", Model: "ma"}},
					"strong": {{Provider: "b", Model: "mb"}},
				},
				Selector: &SelectorConfig{
					Target:     RouteTarget{Provider: "sel", Model: "jev-1.13.0", Protocol: "decisions"},
					Mode:       "enforce",
					Confidence: 0.55,
					Timeout:    "800ms",
				},
				Fallback: "any",
			},
		},
	}
	return h.snapshot(cfg)
}

func TestServeRouteSelectorGradeEnforcePrefersChoice(t *testing.T) {
	h := newHarness()
	upA := newFakeUpstream(t, openaiOKResponder("from-a"))
	upB := newFakeUpstream(t, openaiOKResponder("from-b"))
	upSel := newFakeUpstream(t, selectorDecisionResponder("g1", 0.87))
	snap := routeGradeSelectorSnapshot(t, h, upA, upB, upSel)
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`

	w := h.serve(snap, "openai", "/v1/chat/completions", body, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if upB.hits() != 1 {
		t.Fatalf("selector chose g1 (strong grade / provider b) but b hits = %d, want 1", upB.hits())
	}
	if upA.hits() != 0 {
		t.Fatalf("selector should have skipped a: a hits = %d, want 0", upA.hits())
	}
	if upSel.hits() != 1 {
		t.Fatalf("selector calls = %d, want 1", upSel.hits())
	}
	if r := h.fx.capturedRouting(); len(r) != 1 || r[0] == nil {
		t.Fatalf("routing decision missing: %+v", r)
	} else if r[0].Source != "selector" || r[0].Grade != "strong" || r[0].Selector == nil || r[0].Selector.Choice != "g1" {
		t.Fatalf("grade enforce routing = %+v, want source=selector grade=strong choice=g1", r[0])
	}
}

func TestServeRouteSelectorBeatsBand(t *testing.T) {
	h := newHarness()
	upA := newFakeUpstream(t, openaiOKResponder("from-a"))
	upB := newFakeUpstream(t, openaiOKResponder("from-b"))
	upSel := newFakeUpstream(t, selectorDecisionResponder("c1", 0.9))
	snap := routeSelectorSnapshot(t, h, upA, upB, upSel)
	policy := snap.Cfg.RoutePolicies["m"]
	policy.Bands = []configdomain.RouteBand{{
		When:   configdomain.BandWhen{FollowUp: boolp(true)},
		Target: RouteTarget{Provider: "b", Model: "mb"},
	}}
	snap.Cfg.RoutePolicies["m"] = policy

	body := `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"},{"role":"user","content":"more"}]}`
	w := h.serve(snap, "openai", "/v1/chat/completions", body, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if upA.hits() != 1 || upB.hits() != 0 {
		t.Fatalf("selector must beat the band: a hits = %d (want 1), b hits = %d (want 0)", upA.hits(), upB.hits())
	}
	if upSel.hits() != 1 {
		t.Fatalf("selector calls = %d, want 1", upSel.hits())
	}
}

// dropProviderOnLaterRounds returns a Schedule fake that keeps the full
// target set on the first round and drops provider from every later round —
// the wait-retry round's re-scheduled set a cached selector preference is
// re-applied to.
func dropProviderOnLaterRounds(calls *atomic.Int32, provider string) func(cfg *Config, parentOf map[string]string, exposed, sessionKey string, targets []RouteTarget, routeKeys map[string]bool, generation uint64) []RouteTarget {
	return func(cfg *Config, parentOf map[string]string, exposed, sessionKey string, targets []RouteTarget, routeKeys map[string]bool, generation uint64) []RouteTarget {
		if calls.Add(1) == 1 {
			return targets
		}
		out := make([]RouteTarget, 0, len(targets))
		for _, t := range targets {
			if t.Provider != provider {
				out = append(out, t)
			}
		}
		return out
	}
}

// TestServeRouteSelectorRetryRoundWithoutPreferredReportsFallback: the
// selector's enforce preference is paid once and re-applied to the retry
// round's re-scheduled set. When the preferred target was scheduled out
// (still cooling), the round serves the natural order and the committed
// routing decision must report fallback — selector choice recorded, not
// enforced — instead of claiming the selector determined the order.
func TestServeRouteSelectorRetryRoundWithoutPreferredReportsFallback(t *testing.T) {
	h := newHarness()
	var aCalls atomic.Int32
	upA := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if aCalls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		openaiOKResponder("from-a")(w, r)
	})
	upB := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	upSel := newFakeUpstream(t, selectorDecisionResponder("c1", 0.9))
	snap := routeSelectorSnapshot(t, h, upA, upB, upSel)
	snap.Cfg.Scheduling.RetryWait = "2s"
	h.state.allDown = true
	h.state.allRateLimited = true
	h.state.earliest = time.Now().Add(5 * time.Millisecond)
	var scheduleCalls atomic.Int32
	h.svc.Schedule = dropProviderOnLaterRounds(&scheduleCalls, "b")

	w := h.serve(snap, "openai", "/v1/chat/completions", openaiChatBody(), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a serves the retry round)", w.Code)
	}
	if upSel.hits() != 1 {
		t.Fatalf("selector decisions calls = %d, want at most 1 per request", upSel.hits())
	}
	if upB.hits() != 1 {
		t.Fatalf("b hits = %d, want 1 (only the first round tried the preferred target)", upB.hits())
	}
	if upA.hits() != 2 {
		t.Fatalf("a hits = %d, want 2 (first-round failover + retry-round commit)", upA.hits())
	}
	if r := h.fx.capturedRouting(); len(r) != 1 || r[0] == nil {
		t.Fatalf("routing decision missing: %+v", r)
	} else if r[0].Source != "fallback" || r[0].Selector == nil || r[0].Selector.Enforced || r[0].Selector.Choice != "c1" {
		t.Fatalf("retry-round routing = %+v, want source=fallback with the cached selector choice recorded but not enforced", r[0])
	}
}

// TestServeRouteGradeSelectorRetryRoundWithoutGradeReportsFallback: the graded
// variant of the retry-round honesty rule. Round 2's re-scheduled set has no
// representative of the cached enforced grade, so the committed decision must
// not attribute the served order to that grade.
func TestServeRouteGradeSelectorRetryRoundWithoutGradeReportsFallback(t *testing.T) {
	h := newHarness()
	// "any" fallback only appends grades AFTER the enforced one, so round 1
	// tries b alone; a first serves the retry round.
	upA := newFakeUpstream(t, openaiOKResponder("from-a"))
	upB := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	upSel := newFakeUpstream(t, selectorDecisionResponder("g1", 0.9))
	snap := routeGradeSelectorSnapshot(t, h, upA, upB, upSel)
	snap.Cfg.Scheduling.RetryWait = "2s"
	h.state.allDown = true
	h.state.allRateLimited = true
	h.state.earliest = time.Now().Add(5 * time.Millisecond)
	var scheduleCalls atomic.Int32
	h.svc.Schedule = dropProviderOnLaterRounds(&scheduleCalls, "b")

	w := h.serve(snap, "openai", "/v1/chat/completions", openaiChatBody(), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a serves the retry round)", w.Code)
	}
	if upSel.hits() != 1 {
		t.Fatalf("selector decisions calls = %d, want at most 1 per request", upSel.hits())
	}
	if upB.hits() != 1 {
		t.Fatalf("b hits = %d, want 1 (only the first round tried the enforced grade)", upB.hits())
	}
	if upA.hits() != 1 {
		t.Fatalf("a hits = %d, want 1 (the retry-round commit)", upA.hits())
	}
	if r := h.fx.capturedRouting(); len(r) != 1 || r[0] == nil {
		t.Fatalf("routing decision missing: %+v", r)
	} else if r[0].Source != "fallback" || r[0].Grade != "" || r[0].Selector == nil || r[0].Selector.Enforced || r[0].Selector.Choice != "g1" {
		t.Fatalf("retry-round grade routing = %+v, want source=fallback, no grade, selector choice recorded but not enforced", r[0])
	}
}

// TestServeRouteGradeLatchEmptyGradeFallsThroughToSelector: the graded
// counterpart of the absent-target rule. A session latched to a grade whose
// every target is out of the current ordered set (operator disable,
// capability filtering) must not suppress the selector or record
// source=latch — buildGradeOrdered serves the natural/fallback order either
// way, and the decision must attribute it to whatever actually produced it.
func TestServeRouteGradeLatchEmptyGradeFallsThroughToSelector(t *testing.T) {
	h := newHarness()
	upA := newFakeUpstream(t, openaiOKResponder("from-a"))
	upB := newFakeUpstream(t, openaiOKResponder("from-b"))
	upSel := newFakeUpstream(t, selectorDecisionResponder("g1", 0.9))
	snap := routeGradeSelectorSnapshot(t, h, upA, upB, upSel)
	pol := snap.Cfg.RoutePolicies["m"]
	pol.Escalation = &configdomain.EscalationConfig{
		BadSignals:  []string{"upstream_error"},
		Consecutive: 1,
		Target:      RouteTarget{Provider: "a", Model: "ma"},
		Dwell:       "30m",
	}
	// A declared grade with no representative among the route's targets: the
	// latch resolves to it, but its group is empty in every scheduled round.
	pol.Grades["ghost"] = []RouteTarget{{Provider: "c", Model: "mc"}}
	snap.Cfg.RoutePolicies["m"] = pol
	h.state.SetLatch("sess-ghost", "m", Latch{Target: "grade:ghost", Since: time.Now(), BadRuns: 0}, snap.Generation)

	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}"`
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("x-claude-code-session-id", "sess-ghost")
	w := httptest.NewRecorder()
	Serve(h.svc, h.state, snap, "openai", w, r, "req-ghost")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if upSel.hits() != 1 {
		t.Fatalf("empty-grade latch must not suppress the selector: selector hits = %d, want 1", upSel.hits())
	}
	if upB.hits() != 1 {
		t.Fatalf("selector chose g1 (strong grade / provider b) but b hits = %d, want 1", upB.hits())
	}
	if r := h.fx.capturedRouting(); len(r) != 1 || r[0] == nil {
		t.Fatalf("routing decision missing: %+v", r)
	} else if r[0].Source != "selector" || r[0].Grade != "strong" {
		t.Fatalf("routing = %+v, want source=selector grade=strong (inert latch must not claim attribution)", r[0])
	}
}
