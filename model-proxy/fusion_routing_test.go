package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestFusion_PooledParentMembers (regression for the unified resolver, #10): a
// Fusion recipe whose panel member AND synthesizer name POOLED parents must still
// run. Pre-fix the parent name had no runtime instance (only "name#<id>" virtuals
// exist), so the member was dropped as "not available" and the synthesizer had no
// impl — fusion broke the moment a second account was added. After the fix both
// resolve to a virtual via the resolver and run.
func TestFusion_PooledParentMembers(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	// Two pooled parents (same provider_id "zhipu", distinct names → distinct pool
	// files + distinct upstreams) so the panel member and synthesizer are separate.
	writePoolFile(t, "zhipu-draft", "zhipu", "ZA", "ZB")
	writePoolFile(t, "zhipu-synth", "zhipu", "GA", "GB")

	draftUp := newFakeUpstream(t, anthropicDraftResponder("draft-ok"))
	synthUp := newFakeUpstream(t, anthropicSSEResponder("pooled synthesis ok"))

	cfg := &Config{
		Listen: "127.0.0.1:1",
		Providers: map[string]Provider{
			"zhipu-draft": {AnthropicBaseURL: draftUp.srv.URL, Provider: "zhipu"},
			"zhipu-synth": {AnthropicBaseURL: synthUp.srv.URL, Provider: "zhipu"},
		},
		Routes: map[string][]RouteTarget{"hard": {{Provider: "fusion", Model: "recipe"}}},
		Fusion: map[string]FusionConfig{"recipe": {
			Panel:       []RouteTarget{{Provider: "zhipu-draft", Model: "zdraft"}},
			Synthesizer: RouteTarget{Provider: "zhipu-synth", Model: "gsynth"},
		}},
	}
	p := newTestProxy(t, cfg)
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	out := postAnthropic(t, px, fusionClientBody)
	if !strings.Contains(out, "pooled synthesis ok") {
		t.Fatalf("client missing synthesis — pooled Fusion members not resolved to virtuals: %s", out)
	}
	if draftUp.hits() == 0 {
		t.Error("pooled panel member (zhipu-draft) never hit — resolver did not resolve it to a virtual")
	}
	if synthUp.hits() == 0 {
		t.Error("pooled synthesizer (zhipu-synth) never hit — resolver did not resolve it to a virtual")
	}
}

// TestFusion_CircuitRecordFailure (plan #5): a member's 5xx feeds the standard
// circuit breaker (recordFailure), while surviving members still synthesize.
func TestFusion_CircuitRecordFailure(t *testing.T) {
	pa := newFakeUpstream(t, anthropicDraftResponder("draft-A"))
	pb := newFakeUpstream(t, statusResponder(500))
	pc := newFakeUpstream(t, anthropicDraftResponder("draft-C"))
	ps := newFakeUpstream(t, anthropicSSEResponder("final answer"))
	recipe := FusionConfig{
		Panel: []RouteTarget{
			{Provider: "pa", Model: "ma"},
			{Provider: "pb", Model: "mb"},
			{Provider: "pc", Model: "mc"},
		},
		Synthesizer: RouteTarget{Provider: "ps", Model: "ms"},
	}
	proxy, px := newFusionRig(t, recipe, map[string]*fakeUpstream{"pa": pa, "pb": pb, "pc": pc, "ps": ps})
	out := postAnthropic(t, px, fusionClientBody)
	if !strings.Contains(out, "final answer") {
		t.Fatalf("client body missing answer: %s", out)
	}
	failures := 0
	if status, ok := proxy.runtimeState.Dashboard(time.Now()).Providers["pb"]; ok {
		failures = status.ConsecutiveFailures
	}
	if failures != 1 {
		t.Errorf("failed member consecutiveFailures = %d, want 1", failures)
	}
	if got := proxy.metrics.snapshot()[pmKey{Provider: "pb", Model: "mb"}].Failures; got != 1 {
		t.Errorf("failed member metrics failures = %d, want 1", got)
	}
}

func TestFusionLegSharesTargetPolicies(t *testing.T) {
	t.Run("unsupported parameter is learned and retried", func(t *testing.T) {
		var mu sync.Mutex
		hits := 0
		pa := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			hits++
			n := hits
			mu.Unlock()
			if n == 1 {
				w.WriteHeader(http.StatusBadRequest)
				io.WriteString(w, `{"error":{"message":"Unsupported parameter: temperature"}}`)
				return
			}
			anthropicDraftResponder("draft-A")(w, r)
		})
		pb := newFakeUpstream(t, anthropicDraftResponder("draft-B"))
		ps := newFakeUpstream(t, anthropicSSEResponder("final"))
		recipe := FusionConfig{
			Panel:       []RouteTarget{{Provider: "pa", Model: "ma"}, {Provider: "pb", Model: "mb"}},
			Synthesizer: RouteTarget{Provider: "ps", Model: "ms"},
		}
		proxy, px := newFusionRig(t, recipe, map[string]*fakeUpstream{"pa": pa, "pb": pb, "ps": ps})
		body := `{"model":"hard","max_tokens":100,"temperature":0.2,"stream":true,"messages":[{"role":"user","content":"solve X"}]}`
		if out := postAnthropic(t, px, body); !strings.Contains(out, "final") {
			t.Fatalf("client body missing synthesis: %s", out)
		}
		if pa.hits() != 2 {
			t.Fatalf("unsupported-parameter panel hits = %d, want 2", pa.hits())
		}
		var retry map[string]any
		if err := json.Unmarshal([]byte(pa.lastBody()), &retry); err != nil {
			t.Fatal(err)
		}
		if _, exists := retry["temperature"]; exists {
			t.Errorf("retry still carries learned temperature: %s", pa.lastBody())
		}
		learned := proxy.runtimeState.ParamBlocked("pa", "ma", "temperature")
		if !learned {
			t.Error("fusion leg did not persist unsupported parameter")
		}
	})

	for _, tc := range []struct {
		name      string
		responder http.HandlerFunc
	}{
		{"model denied", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"error":{"message":"You do not have access to model ma"}}`)
		}},
		{"empty success", statusResponder(http.StatusOK)},
	} {
		t.Run(tc.name+" locks only the model", func(t *testing.T) {
			pa := newFakeUpstream(t, tc.responder)
			pb := newFakeUpstream(t, anthropicDraftResponder("draft-B"))
			pc := newFakeUpstream(t, anthropicDraftResponder("draft-C"))
			ps := newFakeUpstream(t, anthropicSSEResponder("final"))
			recipe := FusionConfig{
				Panel: []RouteTarget{
					{Provider: "pa", Model: "ma"},
					{Provider: "pb", Model: "mb"},
					{Provider: "pc", Model: "mc"},
				},
				Synthesizer: RouteTarget{Provider: "ps", Model: "ms"},
				MinPanel:    2,
			}
			proxy, px := newFusionRig(t, recipe, map[string]*fakeUpstream{
				"pa": pa, "pb": pb, "pc": pc, "ps": ps,
			})
			if out := postAnthropic(t, px, fusionClientBody); !strings.Contains(out, "final") {
				t.Fatalf("client body missing synthesis: %s", out)
			}
			if !proxy.modelLocked("pa", "ma", time.Now()) {
				t.Error("fusion leg failure did not lock (pa, ma)")
			}
			if proxy.modelLocked("pa", "other", time.Now()) {
				t.Error("fusion leg failure poisoned another model")
			}
		})
	}
}

// TestFusion_SynthesizerPoolExhaustedFailsClosed (P0-1): when the synthesizer's
// pooled parent has NO healthy account, the synthesizer must fail CLOSED —
// fusion returns false, the route's next target serves, and the synthesizer's
// upstream sees ZERO requests (previously the unresolved pooled-parent name
// fell through with a nil impl and shipped an UNAUTHENTICATED request).
func TestFusion_SynthesizerPoolExhaustedFailsClosed(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	writePoolFile(t, "zhipu-draft", "zhipu", "ZA", "ZB")
	writePoolFile(t, "zhipu-synth", "zhipu", "GA", "GB")

	draftUp := newFakeUpstream(t, anthropicDraftResponder("draft-ok"))
	synthUp := newFakeUpstream(t, anthropicSSEResponder("should never be sent"))
	directUp := newFakeUpstream(t, anthropicSSEResponder("direct fallback ok"))

	cfg := &Config{
		Listen: "127.0.0.1:1",
		Providers: map[string]Provider{
			"zhipu-draft": {AnthropicBaseURL: draftUp.srv.URL, Provider: "zhipu"},
			"zhipu-synth": {AnthropicBaseURL: synthUp.srv.URL, Provider: "zhipu"},
			"direct":      {AnthropicBaseURL: directUp.srv.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{"hard": {
			{Provider: "fusion", Model: "recipe", Priority: 1},
			{Provider: "direct", Model: "dfull", Priority: 2},
		}},
		Fusion: map[string]FusionConfig{"recipe": {
			Panel:       []RouteTarget{{Provider: "zhipu-draft", Model: "zdraft"}},
			Synthesizer: RouteTarget{Provider: "zhipu-synth", Model: "gsynth"},
		}},
	}
	p := newTestProxy(t, cfg)
	// Rate-limit BOTH synthesizer accounts → the resolver finds no healthy virtual.
	for _, vid := range p.poolIndex["zhipu-synth"] {
		p.recordRateLimit(vid, time.Now().Add(time.Hour), rlTransient)
	}
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	out := postAnthropic(t, px, fusionClientBody)
	if !strings.Contains(out, "direct fallback ok") {
		t.Fatalf("expected route failover to the direct target, got: %s", out)
	}
	if synthUp.hits() != 0 {
		t.Errorf("synthesizer upstream hit %d times — a bare unauthenticated request leaked", synthUp.hits())
	}
	if draftUp.hits() == 0 {
		t.Error("panel member should still have been tried")
	}
}

// TestFusionLeg_FailureMetricsAlignTryTarget (review fix): abandoned fusion
// legs record the same metrics shape as tryTarget — a connection error and a
// 5xx count Failures+Failovers, a 401-after-refresh counts Failovers only.
func TestFusionLeg_FailureMetricsAlignTryTarget(t *testing.T) {
	build := func(t *testing.T, pa *fakeUpstream) (*Proxy, *httptest.Server) {
		pb := newFakeUpstream(t, anthropicDraftResponder("draft-B"))
		ps := newFakeUpstream(t, anthropicSSEResponder("final"))
		recipe := FusionConfig{
			Panel:       []RouteTarget{{Provider: "pa", Model: "ma"}, {Provider: "pb", Model: "mb"}},
			Synthesizer: RouteTarget{Provider: "ps", Model: "ms"},
			MinPanel:    1,
		}
		return newFusionRig(t, recipe, map[string]*fakeUpstream{"pa": pa, "pb": pb, "ps": ps})
	}

	t.Run("connection error counts failures and failovers", func(t *testing.T) {
		dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		dead.Close() // connection refused — no cleanup needed
		proxy, px := build(t, &fakeUpstream{srv: dead})
		if out := postAnthropic(t, px, fusionClientBody); !strings.Contains(out, "final") {
			t.Fatalf("client body missing synthesis: %s", out)
		}
		m := proxy.metrics.snapshot()[pmKey{Provider: "pa", Model: "ma"}]
		if m.Failures != 1 || m.Failovers != 1 {
			t.Errorf("conn-error leg metrics = failures %d failovers %d, want 1/1", m.Failures, m.Failovers)
		}
	})

	t.Run("401 after refresh counts failovers only", func(t *testing.T) {
		proxy, px := build(t, newFakeUpstream(t, statusResponder(http.StatusUnauthorized)))
		if out := postAnthropic(t, px, fusionClientBody); !strings.Contains(out, "final") {
			t.Fatalf("client body missing synthesis: %s", out)
		}
		m := proxy.metrics.snapshot()[pmKey{Provider: "pa", Model: "ma"}]
		if m.Failures != 0 || m.Failovers != 1 {
			t.Errorf("401 leg metrics = failures %d failovers %d, want 0/1", m.Failures, m.Failovers)
		}
		// …but the circuit still sees the failure (same as tryTarget).
		failures := 0
		if status, ok := proxy.runtimeState.Dashboard(time.Now()).Providers["pa"]; ok {
			failures = status.ConsecutiveFailures
		}
		if failures != 1 {
			t.Errorf("401 leg consecutiveFailures = %d, want 1", failures)
		}
	})

	t.Run("5xx counts failures and failovers", func(t *testing.T) {
		proxy, px := build(t, newFakeUpstream(t, statusResponder(http.StatusInternalServerError)))
		if out := postAnthropic(t, px, fusionClientBody); !strings.Contains(out, "final") {
			t.Fatalf("client body missing synthesis: %s", out)
		}
		m := proxy.metrics.snapshot()[pmKey{Provider: "pa", Model: "ma"}]
		if m.Failures != 1 || m.Failovers != 1 {
			t.Errorf("5xx leg metrics = failures %d failovers %d, want 1/1", m.Failures, m.Failovers)
		}
	})
}
