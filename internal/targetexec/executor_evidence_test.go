package targetexec

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/protocol"
	"model-proxy/internal/provider"
)

// authErrProvider overrides AuthHeaders with a fixed error: the executor's
// auth-layer failure path (in production the aqp mint / codex OAuth refresh
// inside AuthHeaders).
type authErrProvider struct {
	executorTestProvider
	err error
}

func (p *authErrProvider) AuthHeaders(*http.Request) error { return p.err }

// TestExecutorAuthErrorRecordsObservedStatusAndEvidence pins the 2026-11-11
// incident contract: an auth-layer failure carrying the auth endpoint's
// OBSERVED HTTP status must surface both the status and the error text on the
// failover leg. Before the fix the Result carried neither, so forward
// collapsed every such leg to a synthetic 502 with an empty response body —
// "mint aqp key: HTTP 401: Session expired" existed only in serve.log and the
// Requests page showed an unexplained 502.
func TestExecutorAuthErrorRecordsObservedStatusAndEvidence(t *testing.T) {
	prov := &authErrProvider{err: fmt.Errorf("mint aqp key: %w: %s",
		provider.HTTPStatusError{Code: http.StatusUnauthorized}, `{"message":"Session expired"}`)}
	attempt, _ := testAttempt(prov, `{"model":"model"}`, Policy{LastTarget: true})
	doer := &sequenceDoer{}
	result := (Executor{Client: doer, State: &executorState{}, Effects: &executorEffects{}}).Execute(attempt)
	if result.Committed || result.Outcome != OutcomeFailedHard {
		t.Fatalf("committed=%v outcome=%v, want hard fail", result.Committed, result.Outcome)
	}
	if result.AttemptStatus != http.StatusUnauthorized {
		t.Errorf("AttemptStatus = %d, want 401 (the observed auth-endpoint status, not the synthetic 502)", result.AttemptStatus)
	}
	if body := string(result.AttemptBody); !strings.Contains(body, "mint aqp key") || !strings.Contains(body, "Session expired") {
		t.Errorf("AttemptBody = %q, want the auth error text as evidence", body)
	}
	if doer.calls != 0 {
		t.Errorf("upstream calls = %d, want 0 (failure is pre-upstream)", doer.calls)
	}
}

// TestExecutorAuthErrorWithoutStatusKeepsSyntheticFallback: an auth failure
// with no observed HTTP status (cookie file unreadable, network error to the
// auth endpoint) leaves AttemptStatus at 0 — forward's documented fallback
// then records the synthetic 502, which stays honest ("gateway-side failure,
// no observation") because the body carries the actual error text.
func TestExecutorAuthErrorWithoutStatusKeepsSyntheticFallback(t *testing.T) {
	prov := &authErrProvider{err: errors.New("read sso cookie: no such file")}
	attempt, _ := testAttempt(prov, `{"model":"model"}`, Policy{LastTarget: true})
	result := (Executor{Client: &sequenceDoer{}, State: &executorState{}, Effects: &executorEffects{}}).Execute(attempt)
	if result.Outcome != OutcomeFailedHard {
		t.Fatalf("outcome = %v, want hard fail", result.Outcome)
	}
	if result.AttemptStatus != 0 {
		t.Errorf("AttemptStatus = %d, want 0 (no observed status)", result.AttemptStatus)
	}
	if body := string(result.AttemptBody); !strings.Contains(body, "read sso cookie") {
		t.Errorf("AttemptBody = %q, want the auth error text as evidence", body)
	}
}

// TestExecutorHeaderResolveErrorRecordsEvidence: a configured header that
// cannot resolve (env: reference to an unset variable) fails the target before
// the upstream call; the leg must still carry the reason.
func TestExecutorHeaderResolveErrorRecordsEvidence(t *testing.T) {
	const unsetVar = "MP_TEST_HEADER_VAR_DEFINITELY_UNSET"
	os.Unsetenv(unsetVar)
	prov := &executorTestProvider{}
	writer := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions", nil)
	plan := NewPlan(PlanInput{
		Target:          configdomain.RouteTarget{Provider: "upstream", Model: "model"},
		ProviderConfig:  configdomain.Provider{Provider: "test", OpenAIBaseURL: "https://upstream.test", Headers: map[string]string{"X-Probe": "env:" + unsetVar}},
		Provider:        prov,
		ClientProtocol:  protocol.OpenAI,
		BackendProtocol: protocol.OpenAI,
		ClientPath:      "/v1/chat/completions",
	})
	attempt := NewAttempt(Runtime{}, plan, Exchange{Request: request, Writer: writer, Body: []byte(`{"model":"model"}`)}, Scope{}, Policy{LastTarget: true})
	result := (Executor{Client: &sequenceDoer{}, State: &executorState{}, Effects: &executorEffects{}}).Execute(attempt)
	if result.Outcome != OutcomeFailedHard || result.AttemptStatus != 0 {
		t.Fatalf("outcome=%v status=%d, want hard fail with no observed status", result.Outcome, result.AttemptStatus)
	}
	if body := string(result.AttemptBody); !strings.Contains(body, "environment variable") || !strings.Contains(body, unsetVar) {
		t.Errorf("AttemptBody = %q, want the header resolve error", body)
	}
}

// TestExecutorBuildReqErrorRecordsEvidence: a request the client transport
// cannot even build (invalid method) must still leave the reason on the leg.
func TestExecutorBuildReqErrorRecordsEvidence(t *testing.T) {
	prov := &executorTestProvider{}
	writer := httptest.NewRecorder()
	request := &http.Request{Method: "BAD METHOD", URL: &url.URL{Scheme: "http", Host: "proxy.test", Path: "/v1/chat/completions"}}
	plan := NewPlan(PlanInput{
		Target:          configdomain.RouteTarget{Provider: "upstream", Model: "model"},
		ProviderConfig:  configdomain.Provider{Provider: "test", OpenAIBaseURL: "https://upstream.test"},
		Provider:        prov,
		ClientProtocol:  protocol.OpenAI,
		BackendProtocol: protocol.OpenAI,
		ClientPath:      "/v1/chat/completions",
	})
	attempt := NewAttempt(Runtime{}, plan, Exchange{Request: request, Writer: writer, Body: []byte(`{"model":"model"}`)}, Scope{}, Policy{LastTarget: true})
	result := (Executor{Client: &sequenceDoer{}, State: &executorState{}, Effects: &executorEffects{}}).Execute(attempt)
	if result.Outcome != OutcomeFailedHard || result.AttemptStatus != 0 {
		t.Fatalf("outcome=%v status=%d, want hard fail with no observed status", result.Outcome, result.AttemptStatus)
	}
	if body := string(result.AttemptBody); !strings.Contains(body, "invalid method") {
		t.Errorf("AttemptBody = %q, want the build-request error", body)
	}
}
