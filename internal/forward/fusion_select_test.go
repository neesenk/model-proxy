package forward

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"model-proxy/internal/fusion"
)

// selectorHarness mirrors legHarness with a decisions-protocol provider.
func selectorHarness(t *testing.T, up *fakeUpstream) (*harness, pipeline, fusionCtx, SelectorConfig, fusion.SelectRequest) {
	t.Helper()
	h := newHarness()
	cfg := &Config{
		Providers: map[string]Provider{"p": {DecisionsBaseURL: up.srv.URL + "/v1", Provider: "typesafe"}},
		Routes:    map[string][]RouteTarget{"m": {{Provider: "p", Model: "mm"}}},
	}
	snap := h.snapshot(cfg)
	fc := fusionCtx{
		runtime: snap, proto: "anthropic", calledModel: "m", upPath: "/v1/messages",
		agent: "agent-x", flc: LogCtx{RequestID: "req-leg", Exposed: "m"},
	}
	sel := SelectorConfig{
		Target:  RouteTarget{Provider: "p", Model: "jev-1.13", Protocol: "decisions"},
		Timeout: "800ms",
	}
	req := fusion.SelectRequest{
		State: map[string]any{"task": "what is 2+2?"},
		Candidates: []fusion.SelectorCandidate{
			{Target: RouteTarget{Provider: "a", Model: "ma"}, ID: "c0", Rubric: "cheap"},
			{Target: RouteTarget{Provider: "b", Model: "mb"}, ID: "c1"},
		},
		ChoiceInstructions:     "pick one",
		DifficultyInstructions: "rate it",
		DifficultyLevels:       []string{"easy", "hard"},
	}
	return h, pipeline{svc: h.svc, state: h.state}, fc, sel, req
}

func decisionsResponseBody() []byte {
	return []byte(`{
		"model": "jev-1.13.0",
		"answers": {
			"model_choice": {"type": "choice", "choice": "c1",
				"probabilities": {"c0": 0.2, "c1": 0.8}, "confidence": 0.87},
			"difficulty": {"type": "score", "score": 1.5, "confidence": 0.7}
		},
		"usage": {"input_tokens": 300, "output_tokens": 12}
	}`)
}

func TestCallFusionSelectorSuccess(t *testing.T) {
	up := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/systemone" {
			t.Errorf("upstream path = %q, want /v1/systemone on the decisions base", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer key-p" {
			t.Errorf("Authorization = %q, want the provider credential", got)
		}
		w.Write(decisionsResponseBody())
	})
	h, p, fc, sel, req := selectorHarness(t, up)
	res := p.callFusionSelector(context.Background(), fc, sel, req)
	if res.Err != nil {
		t.Fatalf("selector result = %+v", res)
	}

	// The recorded request body must be the decisions wire shape.
	var body struct {
		Model     string `json:"model"`
		State     any    `json:"state"`
		Questions map[string]struct {
			Type     string `json:"type"`
			Criteria any    `json:"criteria"`
		} `json:"questions"`
	}
	if err := json.Unmarshal([]byte(up.lastBody()), &body); err != nil {
		t.Fatalf("decode recorded body: %v", err)
	}
	if body.Model != "jev-1.13" {
		t.Errorf("model = %q, want the selector target model", body.Model)
	}
	choice, ok := body.Questions["model_choice"]
	if !ok || choice.Type != "choice" {
		t.Errorf("model_choice question = %+v", choice)
	}
	criteria, _ := choice.Criteria.(map[string]any)
	if criteria["c0"] != "a/ma — cheap" || criteria["c1"] != "b/mb" {
		t.Errorf("choice criteria = %+v", criteria)
	}
	if difficulty, ok := body.Questions["difficulty"]; !ok || difficulty.Type != "score" {
		t.Errorf("difficulty question = %+v", difficulty)
	}
	if res.ChoiceID != "c1" || res.Confidence != 0.87 || res.Difficulty != 1.5 ||
		res.Probabilities["c1"] != 0.8 || res.Model != "jev-1.13.0" ||
		res.Usage.Input != 300 || res.Usage.Output != 12 {
		t.Errorf("selector result = %+v", res)
	}
	if h.gate.successes["p"] != 1 {
		t.Errorf("gate successes = %d, want 1", h.gate.successes["p"])
	}
	startSeen, endSeen := false, false
	for _, e := range h.events.Snapshot() {
		if e.RequestID == "fusion-select-req-leg" && e.Type == "start" && e.Provider == "fusion-select:jev-1.13" {
			startSeen = true
		}
		if e.RequestID == "fusion-select-req-leg" && e.Type == "end" && e.Status == 200 {
			endSeen = true
		}
	}
	if !startSeen || !endSeen {
		t.Errorf("selector live event pair missing (start=%v end=%v)", startSeen, endSeen)
	}
}

func TestCallFusionSelectorFailureClasses(t *testing.T) {
	t.Run("429 records rate limit", func(t *testing.T) {
		up := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
		})
		h, p, fc, sel, req := selectorHarness(t, up)
		res := p.callFusionSelector(context.Background(), fc, sel, req)
		if res.Err == nil {
			t.Fatal("want the 429 failure")
		}
		if _, ok := h.gate.rateLimits["p"]; !ok {
			t.Error("rate limit not recorded")
		}
		if h.gate.failures["p"] != 0 {
			t.Errorf("429 must not poison the circuit: failures = %d", h.gate.failures["p"])
		}
	})
	t.Run("5xx records circuit failure", func(t *testing.T) {
		up := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) })
		h, p, fc, sel, req := selectorHarness(t, up)
		res := p.callFusionSelector(context.Background(), fc, sel, req)
		if res.Err == nil {
			t.Fatal("want the 5xx failure")
		}
		if h.gate.failures["p"] != 1 {
			t.Errorf("failures = %d, want 1", h.gate.failures["p"])
		}
	})
	t.Run("empty answers 200 locks the model", func(t *testing.T) {
		up := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"model": "jev", "answers": {}}`))
		})
		h, p, fc, sel, req := selectorHarness(t, up)
		res := p.callFusionSelector(context.Background(), fc, sel, req)
		if res.Err == nil {
			t.Fatal("empty answers must fail")
		}
		if h.gate.modelFailures["p/jev-1.13"] != 1 {
			t.Errorf("model failures = %v, want 1", h.gate.modelFailures)
		}
	})
	t.Run("missing model_choice locks the model", func(t *testing.T) {
		up := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"model": "jev", "answers": {"difficulty": {"type": "score", "score": 1}}}`))
		})
		h, p, fc, sel, req := selectorHarness(t, up)
		res := p.callFusionSelector(context.Background(), fc, sel, req)
		if res.Err == nil || !strings.Contains(res.Err.Error(), "model_choice") {
			t.Fatalf("res = %+v, want the missing-choice failure", res)
		}
		if h.gate.modelFailures["p/jev-1.13"] != 1 {
			t.Errorf("model failures = %v, want 1", h.gate.modelFailures)
		}
	})
	t.Run("selector-own timeout falls back without poisoning the circuit", func(t *testing.T) {
		up := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(150 * time.Millisecond)
			w.Write([]byte(`{}`))
		})
		h, p, fc, sel, req := selectorHarness(t, up)
		sel.Timeout = "20ms"
		res := p.callFusionSelector(context.Background(), fc, sel, req)
		if res.Err == nil {
			t.Fatal("want the timeout failure")
		}
		// The selector's own short budget expiring is a policy fallback, not
		// an upstream-health verdict: the decisions upstream may well have
		// been about to answer fine, and RecordFailure would count toward the
		// provider circuit breaker that chat/panel legs on the same provider
		// depend on (fusion-shadow-cache.md selector contract).
		if h.gate.failures["p"] != 0 {
			t.Errorf("failures = %d, want 0 (selector budget expiry must not open the provider circuit)", h.gate.failures["p"])
		}
	})
	t.Run("transport error still records circuit failure", func(t *testing.T) {
		up := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			w.Write(decisionsResponseBody())
		})
		h, p, fc, sel, req := selectorHarness(t, up)
		up.srv.Close() // connection refused: a REAL provider failure
		res := p.callFusionSelector(context.Background(), fc, sel, req)
		if res.Err == nil {
			t.Fatal("want the transport failure")
		}
		if h.gate.failures["p"] != 1 {
			t.Errorf("failures = %d, want 1 (real transport errors keep counting)", h.gate.failures["p"])
		}
	})
}
