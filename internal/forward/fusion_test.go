package forward

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"model-proxy/internal/fusion"
	"model-proxy/internal/observe/counters"
	"model-proxy/internal/protocol"
)

func anthropicDraftResponder(text string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		fmt.Fprintf(w, `{"id":"msg_d","type":"message","role":"assistant","content":[{"type":"text","text":%q}],"usage":{"input_tokens":11,"output_tokens":7}}`, text)
	}
}

func anthropicSSEResponder(text string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_s\",\"model\":\"synth\",\"usage\":{\"input_tokens\":50}}}\n\n")
		io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":%q}}\n\n", text)
		io.WriteString(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":9}}\n\n")
		io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}
}

const fusionAnthropicBody = `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`

func fusionRecipeConfig(panelA, panelB, synth *fakeUpstream) *Config {
	providers := map[string]Provider{}
	if panelA != nil {
		providers["panel-a"] = Provider{AnthropicBaseURL: panelA.srv.URL, Provider: "test-static"}
	}
	if panelB != nil {
		providers["panel-b"] = Provider{AnthropicBaseURL: panelB.srv.URL, Provider: "test-static"}
	}
	if synth != nil {
		providers["synth"] = Provider{AnthropicBaseURL: synth.srv.URL, Provider: "test-static"}
	}
	recipe := FusionConfig{Synthesizer: RouteTarget{Provider: "synth", Model: "ms"}}
	if panelA != nil {
		recipe.Panel = append(recipe.Panel, RouteTarget{Provider: "panel-a", Model: "ma"})
	}
	if panelB != nil {
		recipe.Panel = append(recipe.Panel, RouteTarget{Provider: "panel-b", Model: "mb"})
	}
	return &Config{
		Providers: providers,
		Routes:    map[string][]RouteTarget{"m": {{Provider: "fusion", Model: "recipe"}}},
		Fusion:    map[string]FusionConfig{"recipe": recipe},
	}
}

// TestServeFusionFanOutSynthesis: all panel members answer → the synthesizer
// streams to the client, the run is metered, and each leg recorded its
// success on the gate.
func TestServeFusionFanOutSynthesis(t *testing.T) {
	pa := newFakeUpstream(t, anthropicDraftResponder("draft-A"))
	pb := newFakeUpstream(t, anthropicDraftResponder("draft-B"))
	ps := newFakeUpstream(t, anthropicSSEResponder("final answer"))
	h := newHarness()
	cfg := fusionRecipeConfig(pa, pb, ps)
	w := h.serve(h.snapshot(cfg), "anthropic", "/v1/messages", fusionAnthropicBody, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "final answer") {
		t.Fatalf("client body missing synthesis: %s", w.Body.String())
	}
	for _, up := range []*fakeUpstream{pa, pb, ps} {
		if up.hits() != 1 {
			t.Errorf("upstream hits = %d, want 1 per leg", up.hits())
		}
	}
	// The synthesis body carries both candidates plus the original question.
	if !strings.Contains(ps.lastBody(), "draft-A") || !strings.Contains(ps.lastBody(), "draft-B") {
		t.Errorf("synthesis body missing candidates: %s", ps.lastBody())
	}
	if got := h.svc.Metrics.Snapshot()[counters.PMKey{Provider: "fusion", Model: "recipe"}].Requests; got != 1 {
		t.Errorf("fusion runs metric = %d, want 1", got)
	}
	if h.gate.successes["panel-a"] != 1 || h.gate.successes["panel-b"] != 1 {
		t.Errorf("panel successes = %d/%d, want 1/1", h.gate.successes["panel-a"], h.gate.successes["panel-b"])
	}
	// Fusion commits never dispatch the route's Shadow.
	if len(h.shadow) != 0 {
		t.Errorf("shadow dispatches = %+v, want none from a fusion commit", h.shadow)
	}
}

// TestServeFusionSynthesizerUnavailableFailsOver: a synthesizer that cannot be
// resolved fails the run closed; the route's next target answers instead.
func TestServeFusionSynthesizerUnavailableFailsOver(t *testing.T) {
	pa := newFakeUpstream(t, anthropicDraftResponder("draft-A"))
	backup := newFakeUpstream(t, anthropicDraftResponder("backup-answer"))
	h := newHarness()
	cfg := fusionRecipeConfig(pa, nil, nil) // no synth provider configured
	cfg.Providers["backup"] = Provider{AnthropicBaseURL: backup.srv.URL, Provider: "test-static"}
	cfg.Routes["m"] = append(cfg.Routes["m"], RouteTarget{Provider: "backup", Model: "mb2"})
	w := h.serve(h.snapshot(cfg), "anthropic", "/v1/messages", fusionAnthropicBody, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "backup-answer") {
		t.Fatalf("status = %d body = %s, want the failover target's answer", w.Code, w.Body.String())
	}
	if backup.hits() != 1 {
		t.Errorf("backup hits = %d, want 1", backup.hits())
	}
}

// TestServeFusionUndefinedRecipeSkipped: a route target naming an undefined
// recipe is skipped like an unbuildable target.
func TestServeFusionUndefinedRecipeSkipped(t *testing.T) {
	backup := newFakeUpstream(t, anthropicDraftResponder("plain"))
	h := newHarness()
	cfg := &Config{
		Providers: map[string]Provider{"backup": {AnthropicBaseURL: backup.srv.URL, Provider: "test-static"}},
		Routes: map[string][]RouteTarget{"m": {
			{Provider: "fusion", Model: "ghost-recipe"},
			{Provider: "backup", Model: "mb"},
		}},
	}
	w := h.serve(h.snapshot(cfg), "anthropic", "/v1/messages", fusionAnthropicBody, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "plain") {
		t.Fatalf("status = %d body = %s, want the plain target after skipping", w.Code, w.Body.String())
	}
}

// TestServeFusionPanelFailureDegradesToSynthesizer: one panel member
// rate-limited (leg unavailable) degrades the run — the synthesizer still
// answers and the leg's 429 was recorded.
func TestServeFusionPanelFailureDegradesToSynthesizer(t *testing.T) {
	pa := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	pb := newFakeUpstream(t, anthropicDraftResponder("draft-B"))
	ps := newFakeUpstream(t, anthropicSSEResponder("degraded final"))
	h := newHarness()
	cfg := fusionRecipeConfig(pa, pb, ps)
	// Quorum 2 with one member rate-limited: the run degrades and answers
	// directly via the synthesizer.
	cfg.Fusion["recipe"] = FusionConfig{
		Panel:       cfg.Fusion["recipe"].Panel,
		Synthesizer: cfg.Fusion["recipe"].Synthesizer,
		MinPanel:    2,
	}
	w := h.serve(h.snapshot(cfg), "anthropic", "/v1/messages", fusionAnthropicBody, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "degraded final") {
		t.Fatalf("status = %d body = %s, want the degraded synthesis", w.Code, w.Body.String())
	}
	if _, ok := h.gate.rateLimits["panel-a"]; !ok {
		t.Error("panel-a 429 never recorded on the gate")
	}
	if got := h.svc.Metrics.Snapshot()[counters.PMKey{Provider: "fusion", Model: "recipe"}].Failovers; got != 1 {
		t.Errorf("fusion degraded metric = %d, want 1", got)
	}
}

// TestServeFusionResponsesExpansion: a responses client whose panel/synth
// backends are stateless (openai) gets previous_response_id chain expansion on
// every leg (expandFusionResponses), and the run still commits.
func TestServeFusionResponsesExpansion(t *testing.T) {
	pa := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		fmt.Fprintf(w, `{"id":"chatcmpl_d","choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":21,"completion_tokens":9}}`, "draft-A")
	})
	ps := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, `{"id":"chatcmpl_s","choices":[{"index":0,"message":{"role":"assistant","content":"resp final"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`)
	})
	h := newHarness()
	h.svc.ResponsesState = protocol.NewResponsesStateStore(t.TempDir() + "/responses_state.json")
	providers := map[string]Provider{
		"panel-a": {OpenAIBaseURL: pa.srv.URL, Provider: "test-static"},
		"synth":   {OpenAIBaseURL: ps.srv.URL, Provider: "test-static"},
	}
	cfg := &Config{
		Providers: providers,
		Routes:    map[string][]RouteTarget{"m": {{Provider: "fusion", Model: "recipe"}}},
		Fusion: map[string]FusionConfig{"recipe": {
			Panel:       []RouteTarget{{Provider: "panel-a", Model: "ma"}},
			Synthesizer: RouteTarget{Provider: "synth", Model: "ms"},
		}},
	}
	body := `{"model":"m","input":"hi","previous_response_id":"resp_missing"}`
	w := h.serve(h.snapshot(cfg), "responses", "/v1/responses", body, map[string]string{"x-claude-code-session-id": "rs-1"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s, want the responses-protocol commit", w.Code, w.Body.String())
	}
	if pa.hits() != 1 || ps.hits() != 1 {
		t.Errorf("hits panel=%d synth=%d, want 1/1", pa.hits(), ps.hits())
	}
}

// TestExpandFusionResponsesPassthrough: non-responses clients and native
// responses backends never expand.
func TestExpandFusionResponsesPassthrough(t *testing.T) {
	h := newHarness()
	p := pipeline{svc: h.svc, state: h.state}
	body := []byte(`{"model":"m"}`)
	fc := fusionCtx{proto: "anthropic"}
	if got, history := p.expandFusionResponses(fc, "openai", body); string(got) != string(body) || history != nil {
		t.Errorf("non-responses proto expanded: %s %v", got, history)
	}
	fc = fusionCtx{proto: "responses"}
	if got, history := p.expandFusionResponses(fc, "responses", body); string(got) != string(body) || history != nil {
		t.Errorf("native responses backend expanded: %s %v", got, history)
	}
	// responses client + stateless backend but no state store: passthrough.
	if got, history := p.expandFusionResponses(fc, "openai", body); string(got) != string(body) || history != nil {
		t.Errorf("nil state store expanded: %s %v", got, history)
	}
}

// --- direct callFusionLeg branch tests ---

func legHarness(t *testing.T, up *fakeUpstream) (*harness, pipeline, fusionCtx, RouteTarget) {
	t.Helper()
	h := newHarness()
	cfg := &Config{
		Providers: map[string]Provider{"p": {AnthropicBaseURL: up.srv.URL, Provider: "test-static"}},
		Routes:    map[string][]RouteTarget{"m": {{Provider: "p", Model: "mm"}}},
	}
	snap := h.snapshot(cfg)
	fc := fusionCtx{
		runtime: snap, proto: "anthropic", calledModel: "m", upPath: "/v1/messages",
		agent: "agent-x", flc: LogCtx{RequestID: "req-leg", Exposed: "m"},
	}
	return h, pipeline{svc: h.svc, state: h.state}, fc, RouteTarget{Provider: "p", Model: "mm"}
}

func callLeg(p pipeline, fc fusionCtx, target RouteTarget) fusion.LegResult {
	return p.callFusionLeg(context.Background(), fc, 0, "fusion-panel", target, []byte(fusionAnthropicBody))
}

// TestCallFusionLegSuccess: a clean 200 yields text + usage and records
// success/tokens on the ports; the leg's live event pair is emitted.
func TestCallFusionLegSuccess(t *testing.T) {
	up := newFakeUpstream(t, anthropicDraftResponder("candidate"))
	h, p, fc, target := legHarness(t, up)
	res := callLeg(p, fc, target)
	if res.Err != nil || res.Text != "candidate" || res.Usage.Input != 11 || res.Usage.Output != 7 {
		t.Fatalf("leg result = %+v, want the candidate draft with usage", res)
	}
	if h.gate.successes["p"] != 1 {
		t.Errorf("gate successes = %d, want 1", h.gate.successes["p"])
	}
	startSeen, endSeen := false, false
	for _, e := range h.events.Snapshot() {
		if e.RequestID == "fusion-panel-0-req-leg" && e.Type == "start" && e.Provider == "fusion-panel:mm" {
			startSeen = true
		}
		if e.RequestID == "fusion-panel-0-req-leg" && e.Type == "end" && e.Status == 200 {
			endSeen = true
		}
	}
	if !startSeen || !endSeen {
		t.Errorf("leg live event pair missing (start=%v end=%v)", startSeen, endSeen)
	}
}

// TestCallFusionLegFailureClasses: each upstream verdict class records the
// matching effect on the gate (and ONLY that effect).
func TestCallFusionLegFailureClasses(t *testing.T) {
	t.Run("429 records rate limit, no circuit failure", func(t *testing.T) {
		up := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
		})
		h, p, fc, target := legHarness(t, up)
		res := callLeg(p, fc, target)
		if res.Err == nil || res.Status != 429 {
			t.Fatalf("res = %+v, want the 429 leg failure", res)
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
		h, p, fc, target := legHarness(t, up)
		res := callLeg(p, fc, target)
		if res.Err == nil || res.Status != 502 {
			t.Fatalf("res = %+v, want the 5xx leg failure", res)
		}
		if h.gate.failures["p"] != 1 {
			t.Errorf("failures = %d, want 1", h.gate.failures["p"])
		}
	})
	t.Run("401 records failure without hard counter", func(t *testing.T) {
		up := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
		h, p, fc, target := legHarness(t, up)
		res := callLeg(p, fc, target)
		if res.Err == nil || res.Status != 401 {
			t.Fatalf("res = %+v, want the 401 leg failure", res)
		}
		if h.gate.failures["p"] != 1 {
			t.Errorf("failures = %d, want 1", h.gate.failures["p"])
		}
	})
	t.Run("404 without verdict locks the model", func(t *testing.T) {
		up := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
		h, p, fc, target := legHarness(t, up)
		res := callLeg(p, fc, target)
		if res.Err == nil || res.Status != 404 {
			t.Fatalf("res = %+v, want the 404 leg failure", res)
		}
		if h.gate.modelFailures["p/mm"] != 1 {
			t.Errorf("model failures = %d, want 1", h.gate.modelFailures["p/mm"])
		}
		if len(h.gate.wireMisses) != 0 {
			t.Errorf("wire misses = %v, want none without a verdict", h.gate.wireMisses)
		}
	})
	t.Run("404 after wire verdict flips verdict, model NOT locked", func(t *testing.T) {
		up := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
		h, p, fc, target := legHarness(t, up)
		h.svc.ResolveBackendProto = func(declared, provName string, provCfg Provider, model, clientProto string, parentOf map[string]string) (string, bool) {
			return "responses", true // verdict-driven switch to /responses
		}
		p = pipeline{svc: h.svc, state: h.state}
		// The /responses leg targets the provider's openai base.
		provCfg := fc.runtime.Cfg.Providers["p"]
		provCfg.OpenAIBaseURL = up.srv.URL
		fc.runtime.Cfg.Providers["p"] = provCfg
		res := callLeg(p, fc, target)
		if res.Err == nil || res.Status != 404 {
			t.Fatalf("res = %+v, want the 404 leg failure", res)
		}
		if len(h.gate.wireMisses) != 1 || h.gate.wireMisses[0] != "p" {
			t.Errorf("wire misses = %v, want [p]", h.gate.wireMisses)
		}
		if h.gate.modelFailures["p/mm"] != 0 {
			t.Errorf("model failures = %d, want 0 (verdict wrong, not model)", h.gate.modelFailures["p/mm"])
		}
	})
	t.Run("4xx client-class does not poison circuit", func(t *testing.T) {
		up := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) })
		h, p, fc, target := legHarness(t, up)
		res := callLeg(p, fc, target)
		if res.Err == nil || res.Status != 403 {
			t.Fatalf("res = %+v, want the 403 leg failure", res)
		}
		if h.gate.failures["p"] != 0 || h.gate.modelFailures["p/mm"] != 0 {
			t.Errorf("4xx must not record provider/model failure: %d/%d", h.gate.failures["p"], h.gate.modelFailures["p/mm"])
		}
	})
	t.Run("empty 200 locks the model", func(t *testing.T) {
		up := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("content-type", "application/json")
			io.WriteString(w, `{"id":"msg_e","type":"message","role":"assistant","content":[],"usage":{"input_tokens":1,"output_tokens":0}}`)
		})
		h, p, fc, target := legHarness(t, up)
		res := callLeg(p, fc, target)
		if res.Err == nil || res.Status != 200 {
			t.Fatalf("res = %+v, want the empty-draft leg failure", res)
		}
		if h.gate.modelFailures["p/mm"] != 1 {
			t.Errorf("model failures = %d, want 1 (empty draft)", h.gate.modelFailures["p/mm"])
		}
	})
}

// TestCallFusionLegGates: the three pre-upstream drops — circuit open, unknown
// provider, nil impl — never reach the wire.
func TestCallFusionLegGates(t *testing.T) {
	t.Run("circuit open refuses the slot", func(t *testing.T) {
		up := newFakeUpstream(t, anthropicDraftResponder("never"))
		h, p, fc, target := legHarness(t, up)
		h.gate.halfOpenOpen["p"] = true
		res := callLeg(p, fc, target)
		if res.Err == nil || up.hits() != 0 {
			t.Fatalf("res.Err = %v hits = %d, want slot refusal before the wire", res.Err, up.hits())
		}
	})
	t.Run("unknown provider fails at plan time", func(t *testing.T) {
		up := newFakeUpstream(t, anthropicDraftResponder("never"))
		_, p, fc, _ := legHarness(t, up)
		res := callLeg(p, fc, RouteTarget{Provider: "ghost", Model: "mm"})
		if res.Err == nil || up.hits() != 0 {
			t.Fatalf("res.Err = %v hits = %d, want plan failure", res.Err, up.hits())
		}
	})
	t.Run("cancelled leg is not a provider failure", func(t *testing.T) {
		up := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(50 * time.Millisecond)
			anthropicDraftResponder("late")(w, r)
		})
		h, p, fc, target := legHarness(t, up)
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(5 * time.Millisecond)
			cancel()
		}()
		res := p.callFusionLeg(ctx, fc, 0, "fusion-panel", target, []byte(fusionAnthropicBody))
		if res.Err == nil {
			t.Fatal("cancelled leg must error")
		}
		if h.gate.failures["p"] != 0 {
			t.Errorf("fusion-level cancel must not record a provider failure: %d", h.gate.failures["p"])
		}
	})
}

// TestCallFusionLegBuildError: a pre-wire (auth) failure drops the leg without
// recording a provider failure — the upstream was never contacted.
func TestCallFusionLegBuildError(t *testing.T) {
	up := newFakeUpstream(t, anthropicDraftResponder("never"))
	h, p, fc, target := legHarness(t, up)
	fc.runtime.Providers["p"] = &fakeProv{key: "k", authErr: errors.New("auth broken")}
	res := callLeg(p, fc, target)
	if res.Err == nil {
		t.Fatal("auth build error must fail the leg")
	}
	if h.gate.failures["p"] != 0 {
		t.Errorf("pre-wire build error must not record a provider failure: %d", h.gate.failures["p"])
	}
	if up.hits() != 0 {
		t.Errorf("upstream hits = %d, want 0", up.hits())
	}
}

// TestCallFusionSynthesizerFailurePaths: resolver failure and plan failure
// abort synthesis closed (no commit, no upstream call).
func TestCallFusionSynthesizerFailurePaths(t *testing.T) {
	h := newHarness()
	p := pipeline{svc: h.svc, state: h.state}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(fusionAnthropicBody))

	t.Run("unknown synthesizer provider", func(t *testing.T) {
		fc := fusionCtx{
			runtime: h.snapshot(&Config{Providers: map[string]Provider{}, Routes: map[string][]RouteTarget{}}),
			proto:   "anthropic", calledModel: "m", flc: LogCtx{RequestID: "r", Exposed: "m"},
		}
		if p.callFusionSynthesizer(fc, RouteTarget{Provider: "ghost", Model: "mm"}, []byte(fusionAnthropicBody), w, r, "") {
			t.Error("unknown synthesizer must not commit")
		}
	})
	t.Run("synthesizer plan failure", func(t *testing.T) {
		// Provider impl exists but the config generation has no such provider:
		// the resolver picks it, planTarget rejects it.
		snap := h.snapshot(&Config{Providers: map[string]Provider{}, Routes: map[string][]RouteTarget{}})
		snap.Providers["s"] = &fakeProv{key: "k"}
		fc := fusionCtx{
			runtime: snap, proto: "anthropic", calledModel: "m", flc: LogCtx{RequestID: "r", Exposed: "m"},
		}
		if p.callFusionSynthesizer(fc, RouteTarget{Provider: "s", Model: "mm"}, []byte(fusionAnthropicBody), w, r, "") {
			t.Error("unplannable synthesizer must not commit")
		}
	})
}
