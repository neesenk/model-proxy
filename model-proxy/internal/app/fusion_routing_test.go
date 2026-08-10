package app

import (
	"encoding/json"
	"io"
	"model-proxy/internal/observe/counters"
	"net/http"
	"net/http/httptest"
	"reflect"
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
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
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
	if got := proxy.metrics.Snapshot()[counters.PMKey{Provider: "pb", Model: "mb"}].Failures; got != 1 {
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
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
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
		m := proxy.metrics.Snapshot()[counters.PMKey{Provider: "pa", Model: "ma"}]
		if m.Failures != 1 || m.Failovers != 1 {
			t.Errorf("conn-error leg metrics = failures %d failovers %d, want 1/1", m.Failures, m.Failovers)
		}
	})

	t.Run("401 after refresh counts failovers only", func(t *testing.T) {
		proxy, px := build(t, newFakeUpstream(t, statusResponder(http.StatusUnauthorized)))
		if out := postAnthropic(t, px, fusionClientBody); !strings.Contains(out, "final") {
			t.Fatalf("client body missing synthesis: %s", out)
		}
		m := proxy.metrics.Snapshot()[counters.PMKey{Provider: "pa", Model: "ma"}]
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
		m := proxy.metrics.Snapshot()[counters.PMKey{Provider: "pa", Model: "ma"}]
		if m.Failures != 1 || m.Failovers != 1 {
			t.Errorf("5xx leg metrics = failures %d failovers %d, want 1/1", m.Failures, m.Failovers)
		}
	})
}

// TestFusionLeg_RateLimitMatchesTargetExecutor proves panel legs use the same
// 429 outcome as a normal target: quota cooldown (not circuit failure), exact
// metrics, and an async refresh of the provider that actually returned 429.
func TestFusionLeg_RateLimitMatchesTargetExecutor(t *testing.T) {
	pa := newFakeUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":"insufficient_quota"}`)
	})
	pb := newFakeUpstream(t, anthropicDraftResponder("draft-B"))
	ps := newFakeUpstream(t, anthropicSSEResponder("final"))
	recipe := FusionConfig{
		Panel:       []RouteTarget{{Provider: "pa", Model: "ma"}, {Provider: "pb", Model: "mb"}},
		Synthesizer: RouteTarget{Provider: "ps", Model: "ms"},
		MinPanel:    1,
	}
	proxy, px := newFusionRig(t, recipe, map[string]*fakeUpstream{"pa": pa, "pb": pb, "ps": ps})
	refreshed := make(chan string, 1)
	proxy.providers["pa"] = &quotaCountProv{name: "pa", refreshed: refreshed}

	before := time.Now()
	if out := postAnthropic(t, px, fusionClientBody); !strings.Contains(out, "final") {
		t.Fatalf("client body missing synthesis after rate-limited panel leg: %s", out)
	}
	after := time.Now()

	metric := proxy.metrics.Snapshot()[counters.PMKey{Provider: "pa", Model: "ma"}]
	if metric.RateLimited429 != 1 || metric.Failovers != 1 || metric.Failures != 0 {
		t.Fatalf("429 panel metrics = %+v, want rate_limited/failovers/failures = 1/1/0", metric)
	}
	status, ok := proxy.runtimeState.Dashboard(time.Now()).Providers["pa"]
	if !ok || status.RateLimitKind != rlQuota ||
		status.RateLimitedUntil.Before(before.Add(120*time.Second)) ||
		status.RateLimitedUntil.After(after.Add(120*time.Second)) ||
		status.ConsecutiveFailures != 0 {
		t.Fatalf("429 panel runtime state = %+v, want quota +120s without circuit failure", status)
	}
	select {
	case name := <-refreshed:
		if name != "pa" {
			t.Fatalf("quota refresh provider=%q, want pa", name)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("429 panel leg did not refresh its provider quota")
	}
	proxy.quota.Stop()
	select {
	case name := <-refreshed:
		t.Fatalf("unexpected duplicate quota refresh for %q", name)
	default:
	}
}

// TestFusionToolsUnsupportedDegradesDirectly verifies a tools request still
// resolves the Fusion route, but a tool-blind synthesizer gate bypasses panel
// fan-out and forwards the original tools unchanged to that synthesizer.
func TestFusionToolsUnsupportedDegradesDirectly(t *testing.T) {
	pa := newFakeUpstream(t, anthropicDraftResponder("must not run"))
	pb := newFakeUpstream(t, anthropicDraftResponder("must not run"))
	ps := newFakeUpstream(t, anthropicToolUseSSEResponder())
	recipe := FusionConfig{
		Panel:       []RouteTarget{{Provider: "pa", Model: "ma"}, {Provider: "pb", Model: "mb"}},
		Synthesizer: RouteTarget{Provider: "ps", Model: "ms"},
	}
	proxy, px := newFusionRig(t, recipe, map[string]*fakeUpstream{"pa": pa, "pb": pb, "ps": ps})
	proxy.mu.Lock()
	config := proxy.cfg.Providers["ps"]
	config.Capabilities = map[string][]string{"ms": {"image"}}
	proxy.cfg.Providers["ps"] = config
	proxy.mu.Unlock()
	body := `{"model":"hard","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"weather?"}],"tools":[{"name":"get_weather","input_schema":{"type":"object"}}],"tool_choice":{"type":"auto"}}`
	if out := postAnthropic(t, px, body); !strings.Contains(out, `"type":"tool_use"`) {
		t.Fatalf("tool-blind degrade did not preserve synthesizer tool response: %s", out)
	}
	if pa.hits() != 0 || pb.hits() != 0 || ps.hits() != 1 {
		t.Fatalf("tool-blind gate hits panel/synth = %d/%d/%d, want 0/0/1", pa.hits(), pb.hits(), ps.hits())
	}
	var synthesis map[string]any
	if err := json.Unmarshal([]byte(ps.lastBody()), &synthesis); err != nil {
		t.Fatal(err)
	}
	var original map[string]any
	if err := json.Unmarshal([]byte(body), &original); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(synthesis["tools"], original["tools"]) {
		t.Fatalf("direct degraded synthesis changed tools:\n got: %v\nwant: %v", synthesis["tools"], original["tools"])
	}
	if !reflect.DeepEqual(synthesis["tool_choice"], original["tool_choice"]) {
		t.Fatalf("direct degraded synthesis changed tool_choice:\n got: %v\nwant: %v", synthesis["tool_choice"], original["tool_choice"])
	}
}
