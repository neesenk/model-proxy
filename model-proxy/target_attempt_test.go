package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	responsecache "model-proxy/internal/cache"
	"model-proxy/internal/observe/requestlog"
	"model-proxy/internal/protocol"
	"model-proxy/internal/targetexec"
)

func TestNewTargetAttemptOnlyGroupsPreparedInputs(t *testing.T) {
	cfg := &Config{}
	cache := responsecache.New(responsecache.Options{
		TTL: time.Hour, MaxEntries: 1, MaxBodyBytes: 1,
	})
	runtime := runtimeSnapshot{cfg: cfg, generation: 41, cache: cache}
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
			Log:              targetLogContext(forwardLogCtx{requestID: "req-1", exposed: "public-model"}),
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
	server := httptest.NewServer(http.HandlerFunc(p.handler))
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
