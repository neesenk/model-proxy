package forward

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	responsecache "model-proxy/internal/cache"
	"model-proxy/internal/protocol"
	"model-proxy/internal/targetexec"
)

// TestNewTargetAttemptOnlyGroupsPreparedInputs (moved from app): the factory
// must only group already-prepared inputs — no rewriting, conversion or
// expansion.
func TestNewTargetAttemptOnlyGroupsPreparedInputs(t *testing.T) {
	cfg := &Config{}
	cache := responsecache.New(responsecache.Options{
		TTL: time.Hour, MaxEntries: 1, MaxBodyBytes: 1,
	})
	runtime := Snapshot{Cfg: cfg, Generation: 41, Cache: cache}
	plan := targetexec.NewPlan(targetexec.PlanInput{
		Target:          RouteTarget{Provider: "upstream", Model: "target-model"},
		ProviderConfig:  Provider{OpenAIBaseURL: "https://example.invalid", Provider: "test-static"},
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

// TestNewTargetAttemptNilConfigTolerated: a snapshot without config must not
// nil-deref (Scheduling zero value).
func TestNewTargetAttemptNilConfigTolerated(t *testing.T) {
	attempt := newTargetAttempt(
		Snapshot{Generation: 7},
		targetexec.Plan{},
		targetexec.Exchange{},
		targetexec.Scope{},
		targetexec.Policy{},
	)
	if attempt.Runtime().Generation != 7 {
		t.Fatalf("generation = %d, want 7", attempt.Runtime().Generation)
	}
}

// TestRequestLogInputMapsRuntimeValuesAndPrefersOriginalBody (moved from app):
// the log input prefers the ORIGINAL client body over the rewritten upstream
// body and snapshots the response header.
func TestRequestLogInputMapsRuntimeValuesAndPrefersOriginalBody(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	request.Header.Set("x-claude-code-session-id", "session-1")
	response := &http.Response{
		StatusCode: http.StatusCreated,
		Header: http.Header{
			"Content-Type": {"application/json"},
			"X-Request-Id": {"upstream-id"},
		},
	}
	original := []byte(`{"model":"public-model","messages":[]}`)
	rewritten := []byte(`{"model":"backend-model","messages":[]}`)
	startedAt := time.Unix(1_000, 0)

	input := BuildRequestLogInput(
		LogCtx{
			RequestID: "request-1",
			Attempt:   2,
			Exposed:   "public-route",
			OrigBody:  original,
		},
		request,
		"anthropic",
		"public-model",
		RouteTarget{Provider: "provider-a", Model: "backend-model"},
		response,
		startedAt,
		rewritten,
	)

	if input.RequestID != "request-1" || input.SessionID != "session-1" {
		t.Errorf("identity = request:%q session:%q, want request-1/session-1", input.RequestID, input.SessionID)
	}
	if input.Protocol != "anthropic" || input.Method != http.MethodPost || input.Path != "/v1/messages" {
		t.Errorf("transport = protocol:%q method:%q path:%q", input.Protocol, input.Method, input.Path)
	}
	if input.CalledModel != "public-model" || input.UpstreamModel != "backend-model" ||
		input.Exposed != "public-route" || input.Provider != "provider-a" {
		t.Errorf(
			"routing = called:%q upstream:%q exposed:%q provider:%q",
			input.CalledModel,
			input.UpstreamModel,
			input.Exposed,
			input.Provider,
		)
	}
	if input.Attempt != 2 || input.Status != http.StatusCreated || !input.StartedAt.Equal(startedAt) {
		t.Errorf("attempt/status/start = %d/%d/%s", input.Attempt, input.Status, startedAt)
	}
	if got := string(input.RequestBody); got != string(original) {
		t.Errorf("request body = %s, want original client body %s (not rewritten %s)", got, original, rewritten)
	}

	response.Header.Set("Content-Type", "text/plain")
	if got := input.ResponseHeader.Get("Content-Type"); got != "application/json" {
		t.Errorf("response header snapshot changed to %q after source mutation", got)
	}
}

// TestRequestLogInputFallsBackToUpstreamBodyForInternalLeg (moved from app):
// an internal leg without an original body logs the actual upstream body.
func TestRequestLogInputFallsBackToUpstreamBodyForInternalLeg(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header)}
	upstream := []byte(`{"model":"panel-model","input":"prompt"}`)

	input := BuildRequestLogInput(
		LogCtx{RequestID: "fusion-panel-1", Exposed: "fusion-route"},
		request,
		"responses",
		"fusion-route",
		RouteTarget{Provider: "provider-b", Model: "panel-model"},
		response,
		time.Now(),
		upstream,
	)

	if got := string(input.RequestBody); got != string(upstream) {
		t.Errorf("request body = %s, want internal upstream body %s", got, upstream)
	}
}

// TestRequestLogInputCarriesAgent: the detected agent is threaded from the
// pipeline's LogCtx through to the request-log input.
func TestRequestLogInputCarriesAgent(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header)}

	input := BuildRequestLogInput(
		LogCtx{RequestID: "req-agent", Exposed: "route", Agent: "claude-code"},
		request,
		"anthropic",
		"m",
		RouteTarget{Provider: "p", Model: "m"},
		response,
		time.Now(),
		nil,
	)
	if input.Agent != "claude-code" {
		t.Errorf("agent = %q, want claude-code", input.Agent)
	}
}

// TestRequestLogInputCarriesConversionDiagnostics: per-attempt conversion
// diagnostics ride the log context into the request-log input.
func TestRequestLogInputCarriesConversionDiagnostics(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header)}

	input := BuildRequestLogInput(
		LogCtx{
			RequestID:   "req-diag",
			Exposed:     "route",
			Diagnostics: []targetexec.ConversionDiagnostic{{Code: "tools_dropped", Detail: "2 server tools"}},
		},
		request,
		"anthropic",
		"m",
		RouteTarget{Provider: "p", Model: "m"},
		response,
		time.Now(),
		nil,
	)
	if len(input.Diagnostics) != 1 || input.Diagnostics[0].Code != "tools_dropped" {
		t.Fatalf("diagnostics = %+v, want one tools_dropped entry", input.Diagnostics)
	}
}
