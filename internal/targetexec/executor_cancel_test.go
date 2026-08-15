package targetexec

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/protocol"
)

// Regression: a client disconnect while waiting for upstream response headers
// used to be classified as a hard upstream failure (circuit tick + failover
// effects). The caller walking away says nothing about provider health.

type canceledCallerDoer struct{ calls int }

func (d *canceledCallerDoer) Do(request *http.Request) (*http.Response, error) {
	d.calls++
	return nil, request.Context().Err()
}

type transportErrorDoer struct{ calls int }

func (d *transportErrorDoer) Do(*http.Request) (*http.Response, error) {
	d.calls++
	return nil, errors.New("connection refused")
}

func cancelAttempt(request *http.Request) Attempt {
	plan := NewPlan(PlanInput{
		Target:          configdomain.RouteTarget{Provider: "upstream", Model: "model"},
		ProviderConfig:  configdomain.Provider{Provider: "test", OpenAIBaseURL: "https://upstream.test"},
		Provider:        &executorTestProvider{},
		ClientProtocol:  protocol.OpenAI,
		BackendProtocol: protocol.OpenAI,
		ClientPath:      "/v1/chat/completions",
	})
	return NewAttempt(Runtime{}, plan,
		Exchange{Request: request, Writer: httptest.NewRecorder(), Body: []byte(`{"model":"model"}`)},
		Scope{}, Policy{})
}

func TestExecutorClientCancelIsNotProviderFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	attempt := cancelAttempt(httptest.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions", nil).WithContext(ctx))
	doer := &canceledCallerDoer{}
	state := &executorState{}
	effects := &executorEffects{}

	result := (Executor{Client: doer, State: state, Effects: effects}).Execute(attempt)

	if result.Committed || result.Outcome != OutcomeClientGone || doer.calls != 1 ||
		state.failures != 0 || state.success != 0 || state.rateLimits != 0 ||
		state.releases != 1 || // half-open probe slot returned without a verdict
		effects.failures != 0 || effects.failovers != 0 || effects.rateLimits != 0 ||
		effects.logs != 0 || effects.commits != 0 {
		t.Fatalf("result=%+v calls=%d state=%+v effects=%+v", result, doer.calls, state, effects)
	}
}

// Contrast control: a transport failure with a LIVE caller context is a real
// upstream connectivity problem and must keep counting toward the circuit.
func TestExecutorTransportErrorWithLiveCallerStillFails(t *testing.T) {
	attempt := cancelAttempt(httptest.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions", nil))
	doer := &transportErrorDoer{}
	state := &executorState{}
	effects := &executorEffects{}

	result := (Executor{Client: doer, State: state, Effects: effects}).Execute(attempt)

	if result.Committed || result.Outcome != OutcomeFailedHard || doer.calls != 1 ||
		state.failures != 1 || state.releases != 0 ||
		effects.failures != 1 || effects.failovers != 1 {
		t.Fatalf("result=%+v calls=%d state=%+v effects=%+v", result, doer.calls, state, effects)
	}
}
