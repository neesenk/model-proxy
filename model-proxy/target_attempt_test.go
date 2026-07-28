package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewTargetAttemptOnlyGroupsPreparedInputs(t *testing.T) {
	cfg := &Config{}
	cache := &responseCache{}
	runtime := runtimeSnapshot{cfg: cfg, generation: 41, cache: cache}
	plan := targetPlan{
		target:       RouteTarget{Provider: "upstream", Model: "target-model"},
		clientProto:  "responses",
		backendProto: "openai",
		baseURL:      "https://example.invalid",
		upPath:       "/v1/chat/completions",
	}
	req := httptest.NewRequest("POST", "/v1/responses", nil)
	writer := httptest.NewRecorder()
	body := []byte(`{"model":"client-model"}`)
	retryTarget := RouteTarget{Provider: "retry", Model: "larger"}

	attempt := newTargetAttempt(
		runtime,
		plan,
		attemptExchange{request: req, writer: writer, body: body},
		attemptScope{
			calledModel:      "client-model",
			agent:            "test-agent",
			cacheKey:         "cache-key",
			log:              forwardLogCtx{requestID: "req-1", exposed: "public-model"},
			responsesHistory: []any{"history"},
			responsesSession: "session-1",
		},
		attemptPolicy{
			force:      true,
			lastTarget: true,
			contextRetry: func() []RouteTarget {
				return []RouteTarget{retryTarget}
			},
		},
	)

	if attempt.runtime.cfg != cfg || attempt.runtime.cache != cache || attempt.runtime.generation != 41 {
		t.Fatalf("runtime group changed: %+v", attempt.runtime)
	}
	if attempt.plan.target != plan.target ||
		attempt.plan.clientProto != "responses" ||
		attempt.plan.backendProto != "openai" ||
		attempt.plan.baseURL != plan.baseURL ||
		attempt.plan.upPath != plan.upPath {
		t.Fatalf("plan group changed: %+v", attempt.plan)
	}
	if attempt.exchange.request != req || attempt.exchange.writer != writer || string(attempt.exchange.body) != string(body) {
		t.Fatalf("exchange group changed: %+v", attempt.exchange)
	}
	if attempt.scope.calledModel != "client-model" ||
		attempt.scope.agent != "test-agent" ||
		attempt.scope.cacheKey != "cache-key" ||
		attempt.scope.log.requestID != "req-1" ||
		attempt.scope.responsesSession != "session-1" {
		t.Fatalf("scope group changed: %+v", attempt.scope)
	}
	if !attempt.policy.force || !attempt.policy.lastTarget {
		t.Fatalf("policy group changed: %+v", attempt.policy)
	}
	retried := attempt.policy.contextRetry()
	if len(retried) != 1 || retried[0] != retryTarget {
		t.Fatalf("context retry = %+v, want %+v", retried, retryTarget)
	}
	if string(attempt.exchange.body) != `{"model":"client-model"}` {
		t.Fatalf("factory rewrote body: %s", attempt.exchange.body)
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
			"panel-a": {Provider: "static", AnthropicBaseURL: panelA.srv.URL},
			"panel-b": {Provider: "static", AnthropicBaseURL: panelB.srv.URL},
			"synth":   {Provider: "static", AnthropicBaseURL: synth.srv.URL},
			"shadow":  {Provider: "static", AnthropicBaseURL: shadow.srv.URL},
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
	logger := newRequestLogger(t.TempDir(), 1<<20, 1<<10, 0)
	go logger.loop()
	t.Cleanup(logger.shutdown)
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
