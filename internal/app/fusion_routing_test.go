package app

import (
	"encoding/json"
	"io"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/forward"
	"model-proxy/internal/observe/counters"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- fusion_routing_test.go ----

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

	cfg := &configdomain.Config{
		Listen: "127.0.0.1:1",
		Providers: map[string]configdomain.Provider{
			"zhipu-draft": {AnthropicBaseURL: draftUp.srv.URL, Provider: "zhipu"},
			"zhipu-synth": {AnthropicBaseURL: synthUp.srv.URL, Provider: "zhipu"},
		},
		Routes: map[string][]configdomain.RouteTarget{"hard": {{Provider: "fusion", Model: "recipe"}}},
		Fusion: map[string]configdomain.FusionConfig{"recipe": {
			Panel:       []configdomain.RouteTarget{{Provider: "zhipu-draft", Model: "zdraft"}},
			Synthesizer: configdomain.RouteTarget{Provider: "zhipu-synth", Model: "gsynth"},
		}},
	}
	p := newTestProxy(t, cfg)
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	out := postAnthropic(t, px, fusionClientBody)
	if !strings.Contains(out, "pooled synthesis ok") {
		t.Fatalf("client missing synthesis — pooled Fusion members not resolved to virtuals: %s", out)
	}
	if draftUp.hits() != 1 {
		t.Errorf("pooled panel member (zhipu-draft) hits = %d, want exactly 1 (double fan-out regression)", draftUp.hits())
	}
	if synthUp.hits() != 1 {
		t.Errorf("pooled synthesizer (zhipu-synth) hits = %d, want exactly 1", synthUp.hits())
	}
}

// TestFusion_CircuitRecordFailure (plan #5): a member's 5xx feeds the standard
// circuit breaker (recordFailure), while surviving members still synthesize.
func TestFusion_CircuitRecordFailure(t *testing.T) {
	pa := newFakeUpstream(t, anthropicDraftResponder("draft-A"))
	pb := newFakeUpstream(t, statusResponder(500))
	pc := newFakeUpstream(t, anthropicDraftResponder("draft-C"))
	ps := newFakeUpstream(t, anthropicSSEResponder("final answer"))
	recipe := configdomain.FusionConfig{
		Panel: []configdomain.RouteTarget{
			{Provider: "pa", Model: "ma"},
			{Provider: "pb", Model: "mb"},
			{Provider: "pc", Model: "mc"},
		},
		Synthesizer: configdomain.RouteTarget{Provider: "ps", Model: "ms"},
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
		recipe := configdomain.FusionConfig{
			Panel:       []configdomain.RouteTarget{{Provider: "pa", Model: "ma"}, {Provider: "pb", Model: "mb"}},
			Synthesizer: configdomain.RouteTarget{Provider: "ps", Model: "ms"},
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
			recipe := configdomain.FusionConfig{
				Panel: []configdomain.RouteTarget{
					{Provider: "pa", Model: "ma"},
					{Provider: "pb", Model: "mb"},
					{Provider: "pc", Model: "mc"},
				},
				Synthesizer: configdomain.RouteTarget{Provider: "ps", Model: "ms"},
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

	cfg := &configdomain.Config{
		Listen: "127.0.0.1:1",
		Providers: map[string]configdomain.Provider{
			"zhipu-draft": {AnthropicBaseURL: draftUp.srv.URL, Provider: "zhipu"},
			"zhipu-synth": {AnthropicBaseURL: synthUp.srv.URL, Provider: "zhipu"},
			"direct":      {AnthropicBaseURL: directUp.srv.URL, Provider: testProviderID},
		},
		Routes: map[string][]configdomain.RouteTarget{"hard": {
			{Provider: "fusion", Model: "recipe", Priority: 1},
			{Provider: "direct", Model: "dfull", Priority: 2},
		}},
		Fusion: map[string]configdomain.FusionConfig{"recipe": {
			Panel:       []configdomain.RouteTarget{{Provider: "zhipu-draft", Model: "zdraft"}},
			Synthesizer: configdomain.RouteTarget{Provider: "zhipu-synth", Model: "gsynth"},
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
	if draftUp.hits() != 1 {
		t.Errorf("panel member hits = %d, want exactly 1 (still tried once)", draftUp.hits())
	}
}

// TestFusionLeg_FailureMetricsAlignTryTarget (review fix): abandoned fusion
// legs record the same metrics shape as tryTarget — a connection error and a
// 5xx count Failures+Failovers, a 401-after-refresh counts Failovers only.
func TestFusionLeg_FailureMetricsAlignTryTarget(t *testing.T) {
	build := func(t *testing.T, pa *fakeUpstream) (*Proxy, *httptest.Server) {
		pb := newFakeUpstream(t, anthropicDraftResponder("draft-B"))
		ps := newFakeUpstream(t, anthropicSSEResponder("final"))
		recipe := configdomain.FusionConfig{
			Panel:       []configdomain.RouteTarget{{Provider: "pa", Model: "ma"}, {Provider: "pb", Model: "mb"}},
			Synthesizer: configdomain.RouteTarget{Provider: "ps", Model: "ms"},
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
	recipe := configdomain.FusionConfig{
		Panel:       []configdomain.RouteTarget{{Provider: "pa", Model: "ma"}, {Provider: "pb", Model: "mb"}},
		Synthesizer: configdomain.RouteTarget{Provider: "ps", Model: "ms"},
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
	recipe := configdomain.FusionConfig{
		Panel:       []configdomain.RouteTarget{{Provider: "pa", Model: "ma"}, {Provider: "pb", Model: "mb"}},
		Synthesizer: configdomain.RouteTarget{Provider: "ps", Model: "ms"},
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

// Regression: a panel leg that already received its response headers but is
// still streaming its body when quorum + grace cancels the panel must NOT be
// recorded as a provider failure (docs/architecture/fusion-shadow-cache.md:
// branches actively cancelled by quorum/grace don't count). Pre-fix the body
// read error path lacked the Do path's cancellation guard and poisoned the
// circuit + evFailures for a healthy upstream.
func TestFusionLeg_CancelDuringBodyReadIsNotProviderFailure(t *testing.T) {
	slowBody := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		// Truncated JSON body: headers delivered, body never completes.
		io.WriteString(w, `{"id":"msg_slow","type":"message","role":"assistant","content":[{"type":"text","text":"slow"`)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	})
	fast := newFakeUpstream(t, anthropicDraftResponder("draft-fast"))
	synth := newFakeUpstream(t, anthropicSSEResponder("final"))
	recipe := configdomain.FusionConfig{
		Panel: []configdomain.RouteTarget{
			{Provider: "slow", Model: "mslow"},
			{Provider: "fast", Model: "mfast"},
		},
		Synthesizer: configdomain.RouteTarget{Provider: "synth", Model: "msynth"},
		MinPanel:    1, // fast leg alone satisfies quorum, then grace cuts slow
	}
	proxy, px := newFusionRig(t, recipe, map[string]*fakeUpstream{"slow": slowBody, "fast": fast, "synth": synth})

	previousGrace := forward.FusionGracePeriod
	forward.FusionGracePeriod = 30 * time.Millisecond
	defer func() { forward.FusionGracePeriod = previousGrace }()

	if out := postAnthropic(t, px, fusionClientBody); !strings.Contains(out, "final") {
		t.Fatalf("client body missing synthesis: %s", out)
	}
	if m := proxy.metrics.Snapshot()[counters.PMKey{Provider: "slow", Model: "mslow"}]; m.Failures != 0 || m.Failovers != 0 {
		t.Errorf("cancelled leg metrics = failures %d failovers %d, want 0/0", m.Failures, m.Failovers)
	}
	if status, ok := proxy.runtimeState.Dashboard(time.Now()).Providers["slow"]; ok && status.ConsecutiveFailures != 0 {
		t.Errorf("cancelled leg consecutiveFailures = %d, want 0", status.ConsecutiveFailures)
	}
}

// ---- fusion_protocol_test.go ----

// TestFusion_CrossProtocolPanel (plan #4): an anthropic client request fans out
// to an openai-protocol member — the member receives a converted openai body
// (system folded into messages), and its openai-shaped draft + usage still feed
// the synthesis and the counters.
func TestFusion_CrossProtocolPanel(t *testing.T) {
	pa := newFakeUpstream(t, anthropicDraftResponder("draft-A"))
	pb := newFakeUpstream(t, openaiDraftResponder("draft-B"))
	ps := newFakeUpstream(t, anthropicSSEResponder("final answer"))
	recipe := configdomain.FusionConfig{
		Panel: []configdomain.RouteTarget{
			{Provider: "pa", Model: "ma"},
			{Provider: "pb", Model: "mb", Protocol: "openai"},
		},
		Synthesizer: configdomain.RouteTarget{Provider: "ps", Model: "ms"},
	}
	proxy, px := newFusionRig(t, recipe, map[string]*fakeUpstream{"pa": pa, "pb": pb, "ps": ps})
	body := `{"model":"hard","max_tokens":100,"stream":true,"system":"be brief","messages":[{"role":"user","content":"solve X"}]}`
	out := postAnthropic(t, px, body)
	if !strings.Contains(out, "final answer") {
		t.Fatalf("client body missing answer: %s", out)
	}
	// The openai member got a converted chat/completions request.
	if pb.lastPath() != "/chat/completions" {
		t.Errorf("openai member path = %q, want /chat/completions", pb.lastPath())
	}
	var ob map[string]any
	if err := json.Unmarshal([]byte(pb.lastBody()), &ob); err != nil {
		t.Fatalf("openai member body not JSON: %v", err)
	}
	if ob["model"] != "mb" {
		t.Errorf("openai member model = %v, want mb", ob["model"])
	}
	if ob["stream"] != false {
		t.Errorf("openai member stream = %v, want false", ob["stream"])
	}
	msgs, ok := ob["messages"].([]any)
	if !ok || len(msgs) == 0 {
		t.Fatalf("openai member messages missing: %v", ob["messages"])
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "be brief" {
		t.Errorf("openai member first message = %v, want folded system prompt", first)
	}
	// Both drafts (anthropic + openai member) reached the synthesis.
	var synth struct {
		System string `json:"system"`
	}
	if err := json.Unmarshal([]byte(ps.lastBody()), &synth); err != nil {
		t.Fatalf("synthesis body not JSON: %v", err)
	}
	for _, d := range []string{"draft-A", "draft-B"} {
		if !strings.Contains(synth.System, d) {
			t.Errorf("synthesis missing %q: %q", d, synth.System)
		}
	}
	// Openai-shaped usage was parsed into the counter.
	toks := proxy.tokens.Snapshot()
	if u := toks[counters.TokenKey{Provider: "pb", Model: "mb"}]; u.Input != 21 || u.Output != 9 {
		t.Errorf("openai member usage = %+v, want {21 9}", u)
	}
}

// TestFusionLeg_WireVerdict404Correction (review fix): a panel leg converted
// to /responses by the wire verdict that comes back 404 must flip the verdict
// (noteWireResponsesMiss) and must NOT lock the model — the verdict was wrong,
// not the model. The NEXT request's leg then goes out as chat. Pre-fix the leg
// recorded a model failure and the verdict never flipped.
func TestFusionLeg_WireVerdict404Correction(t *testing.T) {
	pa := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/responses" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		openaiDraftResponder("draft-A")(w, r)
	})
	pb := newFakeUpstream(t, anthropicDraftResponder("draft-B"))
	ps := newFakeUpstream(t, anthropicSSEResponder("final"))
	cfg := &configdomain.Config{
		Listen: "127.0.0.1:1",
		Providers: map[string]configdomain.Provider{
			// pa has ONLY an openai base: with no declared protocol and a
			// responses=yes verdict the leg is verdict-driven to /responses.
			"pa": {OpenAIBaseURL: pa.srv.URL, Provider: testProviderID},
			"pb": {AnthropicBaseURL: pb.srv.URL, Provider: testProviderID},
			"ps": {AnthropicBaseURL: ps.srv.URL, Provider: testProviderID},
		},
		Routes: map[string][]configdomain.RouteTarget{"hard": {{Provider: "fusion", Model: "recipe"}}},
		Fusion: map[string]configdomain.FusionConfig{"recipe": {
			Panel:       []configdomain.RouteTarget{{Provider: "pa", Model: "ma"}, {Provider: "pb", Model: "mb"}},
			Synthesizer: configdomain.RouteTarget{Provider: "ps", Model: "ms"},
			MinPanel:    1,
		}},
	}
	proxy := newTestProxy(t, cfg)
	for _, name := range []string{"pa", "pb", "ps"} {
		proxy.providers[name] = &testProv{key: name}
	}
	proxy.setWireCaps("pa", wireCaps{BaseURL: pa.srv.URL, Responses: triYes, Chat: triUnknown, ProbedAt: time.Now()})
	px := httptest.NewServer(http.HandlerFunc(proxy.Handler))
	defer px.Close()

	// Request 1: the pa leg is verdict-driven to /responses and 404s.
	if out := postAnthropic(t, px, fusionClientBody); !strings.Contains(out, "final") {
		t.Fatalf("client body missing synthesis: %s", out)
	}
	if pa.lastPath() != "/responses" {
		t.Fatalf("pa leg path = %q, want verdict-driven /responses", pa.lastPath())
	}
	caps, _ := proxy.wireVerdict("pa")
	if caps.Responses != triNo {
		t.Errorf("post-404 verdict responses = %s, want no (flipped)", caps.Responses)
	}
	if proxy.modelLocked("pa", "ma", time.Now()) {
		t.Error("(pa, ma) model-locked after a verdict-miss 404 — the verdict was wrong, not the model")
	}
	if m := proxy.metrics.Snapshot()[counters.PMKey{Provider: "pa", Model: "ma"}]; m.Failures != 0 || m.Failovers != 1 {
		t.Errorf("pa metrics = failures %d failovers %d, want 0/1 (leg abandoned like tryTarget's failover)", m.Failures, m.Failovers)
	}

	// Request 2: the flipped verdict sends the pa leg to chat — its draft now
	// feeds the synthesis, and the model is STILL not locked.
	if out := postAnthropic(t, px, fusionClientBody); !strings.Contains(out, "final") {
		t.Fatalf("request 2 client body missing synthesis: %s", out)
	}
	if pa.lastPath() != "/chat/completions" {
		t.Errorf("request 2 pa leg path = %q, want /chat/completions after verdict flip", pa.lastPath())
	}
	var synth struct {
		System string `json:"system"`
	}
	if err := json.Unmarshal([]byte(ps.lastBody()), &synth); err != nil {
		t.Fatalf("synthesis body not JSON: %v", err)
	}
	if !strings.Contains(synth.System, "draft-A") {
		t.Errorf("request 2 synthesis missing pa's chat draft: %q", synth.System)
	}
	if proxy.modelLocked("pa", "ma", time.Now()) {
		t.Error("(pa, ma) model-locked after the recovered chat leg")
	}
}

// TestFusionLeg_NativeResponsesBackendDraft (review fix E4): a panel leg whose
// backend speaks the native responses protocol (target declares
// protocol:responses) gets a responses-shaped JSON back. extractCandidateText
// must parse THAT shape — pre-fix it only understood chat/anthropic, extracted
// "", and the leg was misjudged as an empty draft and model-locked forever.
func TestFusionLeg_NativeResponsesBackendDraft(t *testing.T) {
	pa := newFakeUpstream(t, responsesDraftResponder("draft-R"))
	pb := newFakeUpstream(t, anthropicDraftResponder("draft-B"))
	ps := newFakeUpstream(t, anthropicSSEResponder("final"))
	recipe := configdomain.FusionConfig{
		Panel: []configdomain.RouteTarget{
			{Provider: "pa", Model: "ma", Protocol: "responses"},
			{Provider: "pb", Model: "mb"},
		},
		Synthesizer: configdomain.RouteTarget{Provider: "ps", Model: "ms"},
		MinPanel:    1,
	}
	proxy, px := newFusionRig(t, recipe, map[string]*fakeUpstream{"pa": pa, "pb": pb, "ps": ps})

	if out := postAnthropic(t, px, fusionClientBody); !strings.Contains(out, "final") {
		t.Fatalf("client body missing synthesis: %s", out)
	}
	if pa.lastPath() != "/responses" {
		t.Fatalf("pa leg path = %q, want native /responses", pa.lastPath())
	}
	// The responses-shaped draft fed the synthesis (not discarded as empty).
	var synth struct {
		System string `json:"system"`
	}
	if err := json.Unmarshal([]byte(ps.lastBody()), &synth); err != nil {
		t.Fatalf("synthesis body not JSON: %v", err)
	}
	if !strings.Contains(synth.System, "draft-R") {
		t.Errorf("synthesis missing pa's responses draft: %q", synth.System)
	}
	if proxy.modelLocked("pa", "ma", time.Now()) {
		t.Error("(pa, ma) model-locked — a good responses draft was misjudged as empty")
	}
	if m := proxy.metrics.Snapshot()[counters.PMKey{Provider: "pa", Model: "ma"}]; m.Failovers != 0 || m.Requests != 1 {
		t.Errorf("pa metrics = requests %d failovers %d, want 1/0 (successful leg)", m.Requests, m.Failovers)
	}
}

// TestExtractCandidateText pins the per-protocol draft extraction: anthropic
// content[] and chat choices[] parse under their own backend protocol, and the
// responses output[] shape parses only when the leg's backend is responses.
func TestExtractCandidateText(t *testing.T) {
	cases := []struct {
		name         string
		backendProto string
		body         string
		want         string
	}{
		{"anthropic", "anthropic", `{"content":[{"type":"text","text":"A"}]}`, "A"},
		{"chat", "openai", `{"choices":[{"message":{"content":"B"}}]}`, "B"},
		{"responses", "responses", `{"output":[{"type":"reasoning"},{"type":"message","content":[{"type":"output_text","text":"C"}]}]}`, "C"},
		{"responses multi-part", "responses", `{"output":[{"type":"message","content":[{"type":"output_text","text":"C1"},{"type":"output_text","text":"C2"}]}]}`, "C1C2"},
		{"responses shape under chat proto stays empty", "openai", `{"output":[{"type":"message","content":[{"type":"output_text","text":"C"}]}]}`, ""},
		{"garbage", "responses", `not json`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := extractCandidateText([]byte(c.body), c.backendProto); got != c.want {
				t.Errorf("extractCandidateText(%s) = %q, want %q", c.backendProto, got, c.want)
			}
		})
	}
}

// TestFusion_ResponsesChainRestored (review fix): a responses-protocol client
// on a fusion route with stateless (chat) backends gets the same
// previous_response_id handling as forward — turn 1's final synthesizer answer
// is recorded, and turn 2's legs + synthesizer receive the EXPANDED history
// (no previous_response_id leaked upstream, no broken chain).
func TestFusion_ResponsesChainRestored(t *testing.T) {
	pa := newFakeUpstream(t, openaiDraftResponder("draft-A"))
	pb := newFakeUpstream(t, openaiDraftResponder("draft-B"))
	ps := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, `{"id":"chat_s1","choices":[{"index":0,"message":{"role":"assistant","content":"a1"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3}}`)
	})
	recipe := configdomain.FusionConfig{
		Panel: []configdomain.RouteTarget{
			{Provider: "pa", Model: "ma", Protocol: "openai"},
			{Provider: "pb", Model: "mb", Protocol: "openai"},
		},
		Synthesizer: configdomain.RouteTarget{Provider: "ps", Model: "ms", Protocol: "openai"},
	}
	_, px := newFusionRig(t, recipe, map[string]*fakeUpstream{"pa": pa, "pb": pb, "ps": ps})

	post := func(body string) []byte {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, px.URL+"/v1/responses", strings.NewReader(body))
		req.Header.Set("content-type", "application/json")
		req.Header.Set("x-claude-code-session-id", "sess")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("status=%d body=%s", resp.StatusCode, out)
		}
		return out
	}

	// Turn 1: the synthesizer's converted responses answer carries id chat_s1,
	// which the executor records (history = input + answer output).
	first := post(`{"model":"hard","input":"q1"}`)
	var firstResp map[string]any
	if err := json.Unmarshal(first, &firstResp); err != nil || firstResp["id"] != "chat_s1" {
		t.Fatalf("turn 1 response = %s", first)
	}

	// Turn 2: chained on chat_s1. Every upstream body must carry the restored
	// history (q1, a1, q2) and must NOT leak previous_response_id.
	second := post(`{"model":"hard","previous_response_id":"chat_s1","input":"q2"}`)
	if !strings.Contains(string(second), "a1") {
		t.Fatalf("turn 2 response = %s", second)
	}
	assertRestored := func(name, body string, wantCandidates bool) {
		t.Helper()
		if strings.Contains(body, "previous_response_id") {
			t.Errorf("%s body leaks previous_response_id: %s", name, body)
		}
		var req map[string]any
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatalf("%s body not JSON: %v", name, err)
		}
		msgs, _ := req["messages"].([]any)
		if len(msgs) < 3 {
			t.Fatalf("%s chat messages = %d, want ≥3 (q1, a1, q2): %s", name, len(msgs), body)
		}
		if asMap(msgs[0])["content"] != "q1" {
			t.Errorf("%s first message = %v, want user q1", name, msgs[0])
		}
		if asMap(msgs[1])["role"] != "assistant" || asMap(msgs[1])["content"] != "a1" {
			t.Errorf("%s second message = %v, want assistant a1 (restored from the chain)", name, msgs[1])
		}
		if wantCandidates && !strings.Contains(body, "CANDIDATE 1") {
			t.Errorf("%s body missing injected candidate section: %s", name, body)
		}
	}
	assertRestored("panel pa", pa.lastBody(), false)
	assertRestored("panel pb", pb.lastBody(), false)
	assertRestored("synthesizer", ps.lastBody(), true)
}
