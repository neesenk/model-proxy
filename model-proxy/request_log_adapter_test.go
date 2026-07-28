package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

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

	input := requestLogInput(
		forwardLogCtx{
			requestID: "request-1",
			attempt:   2,
			exposed:   "public-route",
			origBody:  original,
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
		t.Errorf("attempt/status/start = %d/%d/%s", input.Attempt, input.Status, input.StartedAt)
	}
	if got := string(input.RequestBody); got != string(original) {
		t.Errorf("request body = %s, want original client body %s (not rewritten %s)", got, original, rewritten)
	}

	response.Header.Set("Content-Type", "text/plain")
	if got := input.ResponseHeader.Get("Content-Type"); got != "application/json" {
		t.Errorf("response header snapshot changed to %q after source mutation", got)
	}
}

func TestRequestLogInputFallsBackToUpstreamBodyForInternalLeg(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header)}
	upstream := []byte(`{"model":"panel-model","input":"prompt"}`)

	input := requestLogInput(
		forwardLogCtx{requestID: "fusion-panel-1", exposed: "fusion-route"},
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
