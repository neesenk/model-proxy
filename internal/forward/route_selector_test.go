package forward

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
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
	h.state.SetLatch("sess-sel", Latch{Target: "a/ma", Since: time.Now(), BadRuns: 0}, snap.Generation)

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
