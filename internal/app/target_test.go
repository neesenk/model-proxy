package app

import (
	responsecache "model-proxy/internal/cache"
	"model-proxy/internal/catalog"
	"model-proxy/internal/observe/requestlog"
	"model-proxy/internal/protocol"
	"model-proxy/internal/provider"
	"model-proxy/internal/targetexec"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ---- target_plan_test.go ----

func TestTargetPlanOwnsWirePreparation(t *testing.T) {
	p := newTestProxy(t, &Config{
		Providers: map[string]Provider{
			"up": {
				Provider:         testProviderID,
				OpenAIBaseURL:    "https://chat.example/v1",
				AnthropicBaseURL: "https://anthropic.example",
			},
		},
	})
	plan, err := p.planTarget(targetPlanInput{
		runtime:     p.SnapshotRuntime(),
		target:      RouteTarget{Provider: "up", Model: "claude", Protocol: "anthropic"},
		clientProto: "openai",
		clientPath:  "/chat/completions",
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.BackendProtocol() != "anthropic" ||
		plan.BaseURL() != "https://anthropic.example" ||
		plan.UpstreamPath() != "/v1/messages" ||
		plan.Provider() == nil {
		t.Fatalf("unexpected target plan: %+v", plan)
	}

	if plan.Target() != (RouteTarget{Provider: "up", Model: "claude", Protocol: "anthropic"}) ||
		plan.ProviderID() != testProviderID {
		t.Fatalf("root target facts not frozen in plan: %+v", plan)
	}
}

func TestTargetPlanRejectsUnknownProvider(t *testing.T) {
	p := newTestProxy(t, &Config{})
	if _, err := p.planTarget(targetPlanInput{
		runtime: p.SnapshotRuntime(),
		target:  RouteTarget{Provider: "missing", Model: "m"},
	}); err == nil {
		t.Fatal("unknown provider unexpectedly produced a target plan")
	}
}

// ---- target_attempt_test.go ----

func TestNewTargetAttemptOnlyGroupsPreparedInputs(t *testing.T) {
	cfg := &Config{}
	cache := responsecache.New(responsecache.Options{
		TTL: time.Hour, MaxEntries: 1, MaxBodyBytes: 1,
	})
	runtime := RuntimeSnapshot{Cfg: cfg, Generation: 41, Cache: cache}
	plan := targetexec.NewPlan(targetexec.PlanInput{
		Target:          RouteTarget{Provider: "upstream", Model: "target-model"},
		ProviderConfig:  Provider{OpenAIBaseURL: "https://example.invalid", Provider: testProviderID},
		ClientProtocol:  protocol.Responses,
		BackendProtocol: protocol.OpenAI,
		ClientPath:      "/v1/responses",
		ImageOK:         true,
	})
	req := httptest.NewRequest("POST", "/v1/responses", nil)
	writer := httptest.NewRecorder()
	body := []byte(`{"model":"client-model"}`)
	retryTarget := RouteTarget{Provider: "retry", Model: "larger"}

	attempt := newTargetAttempt(
		runtime,
		plan,
		targetexec.Exchange{Request: req, Writer: writer, Body: body},
		targetexec.Scope{
			CalledModel:      "client-model",
			Agent:            "test-agent",
			CacheKey:         "cache-key",
			Log:              targetexec.LogContext{RequestID: "req-1", Exposed: "public-model"},
			ResponsesHistory: []any{"history"},
			ResponsesSession: "session-1",
		},
		targetexec.Policy{
			Force:      true,
			LastTarget: true,
			ContextRetry: func() []RouteTarget {
				return []RouteTarget{retryTarget}
			},
		},
	)

	attemptRuntime := attempt.Runtime()
	if attemptRuntime.Cache != cache || attemptRuntime.Generation != 41 ||
		attemptRuntime.Scheduling != cfg.Scheduling {
		t.Fatalf("runtime group changed: %+v", attemptRuntime)
	}
	attemptPlan := attempt.Plan()
	if attemptPlan.Target() != plan.Target() ||
		attemptPlan.ClientProtocol() != protocol.Responses ||
		attemptPlan.BackendProtocol() != protocol.OpenAI ||
		attemptPlan.BaseURL() != plan.BaseURL() ||
		attemptPlan.UpstreamPath() != plan.UpstreamPath() {
		t.Fatalf("plan group changed: %+v", attemptPlan)
	}
	exchange := attempt.Exchange()
	if exchange.Request != req || exchange.Writer != writer || string(exchange.Body) != string(body) {
		t.Fatalf("exchange group changed: %+v", exchange)
	}
	scope := attempt.Scope()
	if scope.CalledModel != "client-model" ||
		scope.Agent != "test-agent" ||
		scope.CacheKey != "cache-key" ||
		scope.Log.RequestID != "req-1" ||
		scope.ResponsesSession != "session-1" {
		t.Fatalf("scope group changed: %+v", scope)
	}
	policy := attempt.Policy()
	if !policy.Force || !policy.LastTarget {
		t.Fatalf("policy group changed: %+v", policy)
	}
	retried := policy.ContextRetry()
	if len(retried) != 1 || retried[0] != retryTarget {
		t.Fatalf("context retry = %+v, want %+v", retried, retryTarget)
	}
	if string(exchange.Body) != `{"model":"client-model"}` {
		t.Fatalf("factory rewrote body: %s", exchange.Body)
	}
}

func TestFusionSynthesizerDoesNotDispatchShadow(t *testing.T) {
	panelA := newFakeUpstream(t, anthropicDraftResponder("draft-a"))
	panelB := newFakeUpstream(t, anthropicDraftResponder("draft-b"))
	synth := newFakeUpstream(t, anthropicSSEResponder("final"))
	shadowHit := make(chan struct{}, 1)
	shadow := newFakeUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		select {
		case shadowHit <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	})

	cfg := &Config{
		Providers: map[string]Provider{
			"panel-a": {Provider: testProviderID, AnthropicBaseURL: panelA.srv.URL},
			"panel-b": {Provider: testProviderID, AnthropicBaseURL: panelB.srv.URL},
			"synth":   {Provider: testProviderID, AnthropicBaseURL: synth.srv.URL},
			"shadow":  {Provider: testProviderID, AnthropicBaseURL: shadow.srv.URL},
		},
		Routes: map[string][]RouteTarget{
			"hard": {{Provider: "fusion", Model: "recipe"}},
		},
		Fusion: map[string]FusionConfig{
			"recipe": {
				Panel: []RouteTarget{
					{Provider: "panel-a", Model: "draft-a"},
					{Provider: "panel-b", Model: "draft-b"},
				},
				Synthesizer: RouteTarget{Provider: "synth", Model: "final"},
			},
		},
		Shadow: map[string]ShadowTarget{
			"hard": {Provider: "shadow", Model: "candidate"},
		},
	}
	p := newTestProxy(t, cfg)
	for _, name := range []string{"panel-a", "panel-b", "synth", "shadow"} {
		p.providers[name] = &testProv{key: name}
	}
	logger := requestlog.New(requestlog.Options{
		Directory: t.TempDir(), MaxFileSize: 1 << 20, MaxBodyBytes: 1 << 10,
	})
	go logger.Run()
	t.Cleanup(logger.Shutdown)
	p.reqLog = logger
	server := httptest.NewServer(http.HandlerFunc(p.Handler))
	t.Cleanup(server.Close)

	if body := postAnthropic(t, server, fusionClientBody); !strings.Contains(body, "final") {
		t.Fatalf("client body missing synthesizer response: %s", body)
	}
	// Close waits for every lifecycle-admitted Shadow task. Once it returns,
	// an empty channel proves the synthesizer never scheduled Shadow without
	// relying on a timing window.
	p.Close()
	select {
	case <-shadowHit:
		t.Fatal("Fusion synthesizer recursively dispatched the route's Shadow target")
	default:
	}
}

// ---- targetexec_adapter_test.go ----

// TestCommittedTTFTStaleGenerationDropped (A3 regression): a committed 2xx
// from a PRE-RELOAD in-flight request must not fold its TTFT into the new
// generation's quality EWMA. Pre-fix Committed called recordAttemptQuality
// without a generation, so GenerationArg(nil)=0 made the gate always-true and
// stale TTFT samples polluted the post-reload quality map (the error-rate
// samples were already gated — only this path leaked).
func TestCommittedTTFTStaleGenerationDropped(t *testing.T) {
	p := newTestProxy(t, &Config{
		Providers: map[string]Provider{},
		Routes:    map[string][]RouteTarget{},
	})

	committed := targetexec.AttemptDTO{
		Target:           RouteTarget{Provider: "p", Model: "m"},
		Response:         &http.Response{StatusCode: http.StatusOK},
		TTFTMilliseconds: 500,
	}

	// The request started under generation 1 (the constructor's generation);
	// a reload moves the manager to generation 2 while it is in flight.
	stale := targetExecutionEffects{proxy: p, generation: 1}
	p.runtimeState.ReplaceGeneration(2)
	stale.Committed(committed)
	if q := p.runtimeState.Dashboard(time.Now()).Quality; len(q) != 0 {
		t.Fatalf("stale-generation TTFT wrote into the new quality map: %+v", q)
	}

	// Control: a commit on the CURRENT generation lands (decayed by the tiny
	// record→dashboard interval, so assert a tight window around 500ms).
	fresh := targetExecutionEffects{proxy: p, generation: 2}
	fresh.Committed(committed)
	if got := p.runtimeState.Dashboard(time.Now()).Quality["p"].TTFTMilliseconds; got < 490 || got > 500 {
		t.Fatalf("current-generation TTFT = %dms, want ~500ms recorded", got)
	}
}

// ---- dispatch_context_test.go ----

func TestRuntimeSnapshotKeepsOneReloadGeneration(t *testing.T) {
	p := newTestProxy(t, &Config{
		Providers: map[string]Provider{"old": {Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"m": {{Provider: "old", Model: "old-model"}}},
	})
	p.catalog = catalog.New(nil)

	snapshot := p.SnapshotRuntime()
	if snapshot.Cfg != p.cfg ||
		snapshot.Generation != p.configGeneration.Load() ||
		snapshot.Providers["old"] == nil ||
		len(snapshot.ExpandedRoutes["m"]) != 1 ||
		snapshot.Catalog != p.catalog ||
		snapshot.Cache != p.cache {
		t.Fatalf("incomplete runtime snapshot: %+v", snapshot)
	}

	newCfg := &Config{
		Providers: map[string]Provider{"new": {Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"m": {{Provider: "new", Model: "new-model"}}},
	}
	p.mu.Lock()
	p.cfg = newCfg
	p.providers = map[string]provider.Provider{}
	p.expandedRoutes = newCfg.Routes
	p.configGeneration.Add(1)
	p.mu.Unlock()

	if snapshot.Cfg.Providers["old"].Provider != testProviderID ||
		snapshot.ExpandedRoutes["m"][0].Provider != "old" ||
		snapshot.Generation == p.configGeneration.Load() {
		t.Fatalf("captured snapshot changed across reload swap: %+v", snapshot)
	}
}
