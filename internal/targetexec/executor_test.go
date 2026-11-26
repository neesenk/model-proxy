package targetexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/protocol"
	"model-proxy/internal/provider"
)

type executorTestProvider struct {
	refreshes int
	authErr   error
}

func (p *executorTestProvider) AuthHeaders(*http.Request) error { return p.authErr }
func (p *executorTestProvider) Refresh() error                  { p.refreshes++; return nil }
func (p *executorTestProvider) RewriteRequest(url string, body []byte, _ string) (string, []byte) {
	return url, body
}
func (*executorTestProvider) Logout() error                           { return nil }
func (*executorTestProvider) Usage() error                            { return nil }
func (*executorTestProvider) FetchModels() ([]string, error)          { return nil, nil }
func (*executorTestProvider) Quota() (*provider.QuotaSnapshot, error) { return nil, nil }
func (*executorTestProvider) ProbeRequest(string) provider.ProbeRequest {
	return provider.ProbeRequest{}
}
func (*executorTestProvider) ExtraHeaders(*http.Request, []byte, string, string) {}
func (*executorTestProvider) FilterModelIDs(ids []string) ([]string, []string)   { return ids, nil }

type sequenceDoer struct {
	responses []*http.Response
	// errors, when non-nil at a call index, makes that call fail with the
	// error instead of returning the (then unused) response slot.
	errors []error
	bodies [][]byte
	calls  int
}

func (d *sequenceDoer) Do(request *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(request.Body)
	d.bodies = append(d.bodies, body)
	var err error
	if d.calls < len(d.errors) {
		err = d.errors[d.calls]
	}
	response := d.responses[d.calls]
	d.calls++
	if err != nil {
		return nil, err
	}
	return response, nil
}

type executorState struct {
	failures, modelFailures, releases, success, rateLimits, learned int
	wireMisses                                                      int
	rateLimitDecision                                               RateLimitDecision
	denials                                                         []recordedDenial
}

// recordedDenial is one credential denial the executor reported to the state
// port (State.RecordCredentialDenied).
type recordedDenial struct {
	provider string
	kind     CredentialDenialKind
	evidence string
}

func (*executorState) ModelLocked(configdomain.RouteTarget, time.Time) bool { return false }
func (*executorState) TakeHalfOpenSlot(string) bool                         { return true }
func (s *executorState) ReleaseHalfOpenSlot(string)                         { s.releases++ }
func (s *executorState) RecordSuccess(configdomain.RouteTarget)             { s.success++ }
func (s *executorState) RecordFailure(string)                               { s.failures++ }
func (s *executorState) RecordModelFailure(configdomain.RouteTarget)        { s.modelFailures++ }
func (s *executorState) RecordCredentialDenied(provider string, kind CredentialDenialKind, evidence []byte) {
	s.denials = append(s.denials, recordedDenial{provider: provider, kind: kind, evidence: string(evidence)})
}
func (s *executorState) RecordRateLimit(_ string, decision RateLimitDecision) {
	s.rateLimits++
	s.rateLimitDecision = decision
}
func (s *executorState) LearnParamBlock(configdomain.RouteTarget, string) bool {
	s.learned++
	return true
}
func (*executorState) ApplyParamBlock(_ configdomain.RouteTarget, body []byte) []byte { return body }
func (s *executorState) NoteWireResponsesMiss(configdomain.RouteTarget)               { s.wireMisses++ }

type executorEffects struct {
	failovers, failures, rateLimits, logs, commits int
	lastCommit                                     AttemptDTO
}

func (effects *executorEffects) Failover(configdomain.RouteTarget)    { effects.failovers++ }
func (effects *executorEffects) Failure(configdomain.RouteTarget)     { effects.failures++ }
func (effects *executorEffects) RateLimited(configdomain.RouteTarget) { effects.rateLimits++ }
func (effects *executorEffects) LogAttempt(AttemptDTO)                { effects.logs++ }
func (*executorEffects) CaptureResponse(body io.ReadCloser, _ AttemptDTO) io.ReadCloser {
	return body
}
func (*executorEffects) CaptureUsage(
	body io.ReadCloser,
	_ AttemptDTO,
	_ func(Usage),
) io.ReadCloser {
	return body
}
func (effects *executorEffects) Committed(attempt AttemptDTO) {
	effects.commits++
	effects.lastCommit = attempt
}

type executorResponses struct {
	jsonRecords int
	sseRecords  int
	body        []byte
}

func (responses *executorResponses) RecordJSON(_ string, _ []any, body []byte) bool {
	responses.jsonRecords++
	responses.body = append([]byte(nil), body...)
	return true
}

func (responses *executorResponses) RecordSSE(_ string, _ []any, body []byte) bool {
	responses.sseRecords++
	responses.body = append([]byte(nil), body...)
	return true
}

func testResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(bytes.NewBufferString(body))}
}
func testAttempt(provider provider.Provider, body string, policy Policy) (Attempt, *httptest.ResponseRecorder) {
	writer := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions", nil)
	plan := NewPlan(PlanInput{Target: configdomain.RouteTarget{Provider: "upstream", Model: "model"}, ProviderConfig: configdomain.Provider{Provider: "test", OpenAIBaseURL: "https://upstream.test"}, Provider: provider, ClientProtocol: protocol.OpenAI, BackendProtocol: protocol.OpenAI, ClientPath: "/v1/chat/completions"})
	return NewAttempt(Runtime{}, plan, Exchange{Request: request, Writer: writer, Body: []byte(body)}, Scope{}, policy), writer
}

func TestExecutorRetries401Then200(t *testing.T) {
	provider := &executorTestProvider{}
	attempt, writer := testAttempt(provider, `{"model":"model"}`, Policy{LastTarget: true})
	doer := &sequenceDoer{responses: []*http.Response{testResponse(401, "no"), testResponse(200, `{"ok":true}`)}}
	state := &executorState{}
	effects := &executorEffects{}
	result := (Executor{Client: doer, State: state, Effects: effects}).Execute(attempt)
	if !result.Committed || doer.calls != 2 || provider.refreshes != 1 || state.success != 1 ||
		state.failures != 0 || effects.failures != 0 || effects.failovers != 0 ||
		effects.logs != 1 || effects.commits != 1 || writer.Code != 200 {
		t.Fatalf("result=%+v calls=%d refresh=%d state=%+v effects=%+v status=%d",
			result, doer.calls, provider.refreshes, state, effects, writer.Code)
	}
}

func TestExecutorExhausted401RecordsFailoverWithoutFailureMetric(t *testing.T) {
	provider := &executorTestProvider{}
	attempt, _ := testAttempt(provider, `{"model":"model"}`, Policy{LastTarget: true})
	doer := &sequenceDoer{responses: []*http.Response{
		testResponse(http.StatusUnauthorized, "first"),
		testResponse(http.StatusUnauthorized, "second"),
	}}
	state := &executorState{}
	effects := &executorEffects{}

	result := (Executor{Client: doer, State: state, Effects: effects}).Execute(attempt)

	if result.Committed || result.Outcome != OutcomeFailedHard ||
		doer.calls != 2 || provider.refreshes != 1 ||
		state.failures != 1 || state.success != 0 || state.rateLimits != 0 ||
		effects.failovers != 1 || effects.failures != 0 || effects.rateLimits != 0 ||
		effects.logs != 0 || effects.commits != 0 {
		t.Fatalf("result=%+v calls=%d refresh=%d state=%+v effects=%+v",
			result, doer.calls, provider.refreshes, state, effects)
	}
	// The terminal 401 must carry the upstream's denial body as the leg's
	// failure evidence (same peek discipline as 429/5xx) — a bare 401 with an
	// empty body leaves the Requests page nothing to explain the failover.
	if result.AttemptStatus != http.StatusUnauthorized {
		t.Errorf("AttemptStatus = %d, want 401", result.AttemptStatus)
	}
	if body := string(result.AttemptBody); body != "second" {
		t.Errorf("AttemptBody = %q, want the second (terminal) 401 body", body)
	}
}

func TestExecutorUnsupportedParamDoesNotConsume401Retry(t *testing.T) {
	provider := &executorTestProvider{}
	attempt, _ := testAttempt(provider, `{"model":"model","store":false}`, Policy{LastTarget: true})
	doer := &sequenceDoer{responses: []*http.Response{testResponse(400, `{"error":{"message":"Unsupported parameter: 'store'"}}`), testResponse(401, "no"), testResponse(200, "ok")}}
	state := &executorState{}
	effects := &executorEffects{}
	result := (Executor{Client: doer, State: state, Effects: effects}).Execute(attempt)
	if !result.Committed || doer.calls != 3 || provider.refreshes != 1 || state.learned != 1 ||
		state.failures != 0 || effects.failures != 0 || effects.failovers != 0 ||
		bytes.Contains(doer.bodies[1], []byte(`"store"`)) ||
		bytes.Contains(doer.bodies[2], []byte(`"store"`)) {
		t.Fatalf("result=%+v calls=%d refresh=%d state=%+v effects=%+v retry2=%s retry3=%s",
			result, doer.calls, provider.refreshes, state, effects, doer.bodies[1], doer.bodies[2])
	}
}

func TestExecutorContextOverflowOnlyAbandonsForLargerTarget(t *testing.T) {
	provider := &executorTestProvider{}
	state := &executorState{}
	attempt, writer := testAttempt(provider, `{}`, Policy{LastTarget: true, ContextRetry: func() []configdomain.RouteTarget { return nil }})
	effects := &executorEffects{}
	result := (Executor{
		Client: &sequenceDoer{responses: []*http.Response{testResponse(400, `context_length_exceeded`)}}, State: state, Effects: effects,
	}).Execute(attempt)
	if !result.Committed || writer.Code != 400 {
		t.Fatalf("empty retry must commit: %+v status=%d", result, writer.Code)
	}
	attempt, _ = testAttempt(provider, `{}`, Policy{ContextRetry: func() []configdomain.RouteTarget { return []configdomain.RouteTarget{{Provider: "larger"}} }})
	effects = &executorEffects{}
	result = (Executor{
		Client: &sequenceDoer{responses: []*http.Response{testResponse(400, `context_length_exceeded`)}}, State: state, Effects: effects,
	}).Execute(attempt)
	if result.Committed || len(result.Retried) != 1 || effects.failovers != 1 || effects.failures != 0 {
		t.Fatalf("larger retry = %+v effects=%+v", result, effects)
	}
}

func TestExecutorVerdict404DoesNotLockModel(t *testing.T) {
	provider := &executorTestProvider{}
	attempt, _ := testAttempt(provider, `{}`, Policy{})
	plan := attempt.Plan()
	plan.viaResponsesVerdict = true
	attempt = NewAttempt(attempt.Runtime(), plan, attempt.Exchange(), attempt.Scope(), attempt.Policy())
	state := &executorState{}
	result := (Executor{Client: &sequenceDoer{responses: []*http.Response{testResponse(404, "missing")}}, State: state}).Execute(attempt)
	if result.Committed || state.wireMisses != 1 || state.modelFailures != 0 {
		t.Fatalf("verdict 404 = %+v misses=%d locks=%d", result, state.wireMisses, state.modelFailures)
	}
	attempt, _ = testAttempt(provider, `{}`, Policy{})
	state = &executorState{}
	(Executor{Client: &sequenceDoer{responses: []*http.Response{testResponse(404, "missing")}}, State: state}).Execute(attempt)
	if state.modelFailures != 1 {
		t.Fatalf("ordinary 404 locks=%d", state.modelFailures)
	}
}

func TestExecutor429And5xxFailover(t *testing.T) {
	provider := &executorTestProvider{}
	for _, status := range []int{429, 503} {
		attempt, _ := testAttempt(provider, `{}`, Policy{})
		state := &executorState{}
		effects := &executorEffects{}
		response := testResponse(status, "err")
		if status == 429 {
			response.Header.Set("Retry-After", "120")
		}
		before := time.Now()
		result := (Executor{
			Client: &sequenceDoer{responses: []*http.Response{response}}, State: state, Effects: effects,
		}).Execute(attempt)
		after := time.Now()
		if result.Committed || effects.failovers != 1 ||
			(status == 429 && (result.Outcome != OutcomeRateLimited || state.rateLimits != 1 ||
				effects.rateLimits != 1 || effects.failures != 0 || state.rateLimitDecision.Kind != RateLimitTransient ||
				state.rateLimitDecision.Until.Before(before.Add(120*time.Second)) ||
				state.rateLimitDecision.Until.After(after.Add(120*time.Second)))) ||
			(status == 503 && (result.Outcome != OutcomeFailedHard || state.failures != 1 || effects.failures != 1 ||
				effects.rateLimits != 0 || state.rateLimits != 0)) {
			t.Fatalf("status %d result=%+v state=%+v effects=%+v", status, result, state, effects)
		}
	}
}

// kimi-code answers the exhausted 5-hour coding-plan window with 403
// "You've reached your 5-hour usage limit" — that body must route into the
// reactive rate-limit cooldown + failover path (same as 429), not the
// plain-4xx commit that keeps dispatching into the dead account.
func TestExecutor403QuotaBodyRateLimits(t *testing.T) {
	provider := &executorTestProvider{}
	attempt, _ := testAttempt(provider, `{}`, Policy{})
	state := &executorState{}
	effects := &executorEffects{}
	body := `{"error":{"message":"You've reached your 5-hour usage limit. Your quota will reset when the current 5-hour window ends. To continue now, purchase extra usage or upgrade your plan: https://www.kimi.com/membership/subscription?tab=quota"}}`
	before := time.Now()
	result := (Executor{
		Client: &sequenceDoer{responses: []*http.Response{testResponse(403, body)}}, State: state, Effects: effects,
	}).Execute(attempt)
	if result.Committed || result.Outcome != OutcomeRateLimited || state.rateLimits != 1 ||
		effects.rateLimits != 1 || effects.failovers != 1 || effects.failures != 0 ||
		state.rateLimitDecision.Kind != RateLimitQuota ||
		state.rateLimitDecision.Until.Before(before) {
		t.Fatalf("403 quota result=%+v state=%+v effects=%+v", result, state, effects)
	}
}

// step-plan answers the exhausted Credit 月池 with 402 "quota_exceeded" —
// same body-proven quota rule on another non-429 status: reactive cooldown +
// failover instead of committing the raw denial.
func TestExecutor402QuotaBodyRateLimits(t *testing.T) {
	provider := &executorTestProvider{}
	attempt, _ := testAttempt(provider, `{}`, Policy{})
	state := &executorState{}
	effects := &executorEffects{}
	body := `{"error":{"type":"quota_exceeded","message":"Step Plan credit exhausted. Purchase a booster pack or wait for the monthly reset."}}`
	before := time.Now()
	result := (Executor{
		Client: &sequenceDoer{responses: []*http.Response{testResponse(402, body)}}, State: state, Effects: effects,
	}).Execute(attempt)
	if result.Committed || result.Outcome != OutcomeRateLimited || state.rateLimits != 1 ||
		effects.rateLimits != 1 || effects.failovers != 1 || effects.failures != 0 ||
		state.rateLimitDecision.Kind != RateLimitQuota ||
		state.rateLimitDecision.Until.Before(before) {
		t.Fatalf("402 quota result=%+v state=%+v effects=%+v", result, state, effects)
	}
}

// OpenRouter answers credit exhaustion with 402 error.metadata.limit_source
// (openrouter_credits / openrouter_key_limit /
// openrouter_in_flight_budget) — the body-proven quota rule must catch that
// shape too (backend-contracts.md), or the raw 402 keeps committing with no
// cooldown and no failover.
func TestExecutor402OpenRouterLimitSourceRateLimits(t *testing.T) {
	provider := &executorTestProvider{}
	attempt, _ := testAttempt(provider, `{}`, Policy{})
	state := &executorState{}
	effects := &executorEffects{}
	body := `{"error":{"message":"Not enough credits: 0.05 required, but only 0.02 remaining. Please add more credits and try again.","code":402,"metadata":{"limit_source":"openrouter_credits"}}}`
	before := time.Now()
	result := (Executor{
		Client: &sequenceDoer{responses: []*http.Response{testResponse(402, body)}}, State: state, Effects: effects,
	}).Execute(attempt)
	if result.Committed || result.Outcome != OutcomeRateLimited || state.rateLimits != 1 ||
		effects.rateLimits != 1 || effects.failovers != 1 || effects.failures != 0 ||
		state.rateLimitDecision.Kind != RateLimitQuota ||
		state.rateLimitDecision.Until.Before(before) {
		t.Fatalf("openrouter 402 result=%+v state=%+v effects=%+v", result, state, effects)
	}
}

// A 402 without quota proof in the body must keep the generic path: committed
// to the client as-is, no cooldown, no failover (payment-required is not
// inherently quota exhaustion).
func TestExecutor402PlainBodyCommits(t *testing.T) {
	provider := &executorTestProvider{}
	attempt, _ := testAttempt(provider, `{}`, Policy{})
	state := &executorState{}
	effects := &executorEffects{}
	result := (Executor{
		Client: &sequenceDoer{responses: []*http.Response{testResponse(402, `{"error":{"message":"payment required"}}`)}}, State: state, Effects: effects,
	}).Execute(attempt)
	if !result.Committed || state.rateLimits != 0 || effects.rateLimits != 0 || effects.failovers != 0 {
		t.Fatalf("402 plain result=%+v state=%+v effects=%+v", result, state, effects)
	}
}

// A 403 without quota proof in the body must keep the generic path: committed
// to the client as-is, no cooldown, no failover (intentional-behaviors #3).
func TestExecutor403PlainBodyCommits(t *testing.T) {
	provider := &executorTestProvider{}
	attempt, _ := testAttempt(provider, `{}`, Policy{})
	state := &executorState{}
	effects := &executorEffects{}
	result := (Executor{
		Client: &sequenceDoer{responses: []*http.Response{testResponse(403, `{"error":"forbidden"}`)}}, State: state, Effects: effects,
	}).Execute(attempt)
	if !result.Committed || state.rateLimits != 0 || effects.rateLimits != 0 || effects.failovers != 0 {
		t.Fatalf("plain 403 result=%+v state=%+v effects=%+v", result, state, effects)
	}
}

func TestExecutor403FailureScope(t *testing.T) {
	const nginx = "<html>\n<head><title>403 Forbidden</title></head>\n<body>\n<center><h1>403 Forbidden</h1></center>\n<hr><center>nginx</center>\n</body>\n</html>"
	// 2026-11-23 compass WAF incident: the gateway answered every shopee/aqp
	// call with the stock nginx denial page over CRLF line endings (146
	// bytes, observed verbatim in the request log) while the deployed build
	// committed 2056 of them as terminal 403s. Both line-ending shapes of the
	// same document must keep classifying as a gateway refusal.
	const nginxCRLF = "<html>\r\n<head><title>403 Forbidden</title></head>\r\n<body>\r\n<center><h1>403 Forbidden</h1></center>\r\n<hr><center>nginx</center>\r\n</body>\r\n</html>\r\n"
	const model = "<html><body>no access to model</body></html>"
	const gatewayDiagnostic = "upstream gateway refused the request"
	cases := []struct {
		name, body, contentType, challenge string
		last, commit                       bool
		accountFailures, modelFailures     int
		evidence                           string
	}{
		{name: "stock gateway", body: nginx, contentType: "text/html", evidence: gatewayDiagnostic},
		{name: "stock gateway CRLF (2026-11-23 compass WAF)", body: nginxCRLF, contentType: "text/html", evidence: gatewayDiagnostic},
		{name: "gateway with wrong MIME", body: nginx, contentType: "application/json", evidence: gatewayDiagnostic},
		{name: "last gateway", body: nginx, last: true, commit: true},
		{name: "explicit challenge", body: "browser check", challenge: "challenge", evidence: gatewayDiagnostic},
		{name: "model denial takes priority", body: model, challenge: "challenge", modelFailures: 1, evidence: model},
		{name: "last model denial", body: model, last: true, commit: true, modelFailures: 1},
		{name: "account disabled", body: `{"error":{"code":"account_disabled"}}`, accountFailures: 1, evidence: "upstream denied the account"},
		{name: "last account suspended", body: `{"error":{"code":"account_suspended"}}`, last: true, accountFailures: 1, evidence: "upstream denied the account"},
		{name: "generic HTML", body: "<html><title>403 Forbidden</title></html>", contentType: "text/html", commit: true},
		{name: "unknown resource", body: "<html>resource does not exist</html>", commit: true},
		{name: "incidental account code", body: `{"metadata":{"code":"account_disabled"}}`, commit: true},
		{name: "endpoint permission", body: `{"error":{"code":"permission_denied","message":"operation not supported by this endpoint"}}`, commit: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := &executorTestProvider{}
			attempt, writer := testAttempt(provider, "{}", Policy{LastTarget: tc.last})
			resp := testResponse(http.StatusForbidden, tc.body)
			resp.Header.Set("Content-Type", tc.contentType)
			resp.Header.Set("Cf-Mitigated", tc.challenge)
			state, effects := &executorState{}, &executorEffects{}
			result := (Executor{Client: &sequenceDoer{responses: []*http.Response{resp}}, State: state, Effects: effects}).Execute(attempt)
			if state.failures != tc.accountFailures || state.modelFailures != tc.modelFailures || state.rateLimits != 0 || effects.failures != 0 || provider.refreshes != 0 {
				t.Fatalf("state=%+v effects=%+v refreshes=%d", state, effects, provider.refreshes)
			}
			if tc.commit {
				if !result.Committed || writer.Code != 403 || writer.Body.String() != tc.body || effects.commits != 1 || effects.failovers != 0 || state.releases != 1 {
					t.Fatalf("commit=%v status=%d body=%q effects=%+v releases=%d", result.Committed, writer.Code, writer.Body.String(), effects, state.releases)
				}
				return
			}
			if result.Committed || result.Outcome != OutcomeFailedHard || result.AttemptStatus != 403 || string(result.AttemptBody) != tc.evidence || effects.failovers != 1 || effects.commits != 0 || writer.Body.Len() != 0 || state.releases != 1-tc.accountFailures {
				t.Fatalf("result=%+v effects=%+v releases=%d", result, effects, state.releases)
			}
		})
	}
}

// TestExecutorLapsedPlanSubscriptionFailsOver pins the 2026-11-25 step-plan
// incident. An account whose Step Plan subscription had lapsed answered every
// request shape with HTTP 400
// {"error":{"message":"you have no active step plan subscription",
// "type":"request_params_invalid"}} — a generic error type, the proof only in
// the message. No attribution matched (the body-proven quota path needs a
// quota marker, the account-disablement path needs error.code), so the 400
// fell through to the ordinary-4xx commit: 11 terminal 400s reached the
// client while both sibling accounts served the same route. A lapsed
// subscription is an ACCOUNT fact: one circuit tick + failover with no failure
// metric, like an exhausted 401 — the last candidate included, the account
// error is never passed through, and the upstream body rides along as the
// leg's evidence. The phrase is the evidence, not the status. A plan that
// merely excludes one model is the adjacent non-match: it stays an ordinary
// 4xx commit with zero accounting.
func TestExecutorLapsedPlanSubscriptionFailsOver(t *testing.T) {
	const lapsed = `{"error":{"message":"you have no active step plan subscription","type":"request_params_invalid"}}`
	cases := []struct {
		name, body string
		status     int
		last       bool
		commit     bool
	}{
		{name: "lapsed subscription", body: lapsed, status: http.StatusBadRequest},
		{name: "last candidate", body: lapsed, status: http.StatusBadRequest, last: true},
		{name: "case-insensitive", body: `{"error":{"message":"You have no active Step Plan subscription","type":"request_params_invalid"}}`, status: http.StatusBadRequest},
		{name: "same phrase on 403", body: lapsed, status: http.StatusForbidden},
		{
			// An account fact outranks the model lock: a body carrying both
			// benches the account, never just the model.
			name:   "account fact outranks the model lock",
			body:   `{"error":{"message":"model not found: you have no active step plan subscription"}}`,
			status: http.StatusBadRequest,
		},
		{
			name:   "plan tier excludes the model",
			body:   `{"error":{"message":"your active step plan subscription does not include model step-5-preview","type":"request_params_invalid"}}`,
			status: http.StatusBadRequest,
			commit: true,
		},
		{name: "ordinary bad request", body: `{"error":{"message":"messages: invalid role","type":"request_params_invalid"}}`, status: http.StatusBadRequest, commit: true},
		{
			// An error body echoing request content is not a statement
			// about the account: only the envelope's own message counts.
			name:   "request content echoing the phrase",
			body:   `{"error":{"code":"request_params_invalid","message":"invalid user field"},"request":{"user":"no active step plan subscription"}}`,
			status: http.StatusBadRequest,
			commit: true,
		},
		{
			// The phrase is the evidence, but only a body the executor reads
			// for evidence can carry it: statuses it never peeks stay an
			// ordinary commit.
			name:   "phrase on a status whose body is not peeked",
			body:   lapsed,
			status: http.StatusMethodNotAllowed,
			commit: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := &executorTestProvider{}
			attempt, writer := testAttempt(provider, "{}", Policy{LastTarget: tc.last})
			// The wire shape observed in the request log: a JSON error body
			// served as text/plain.
			resp := testResponse(tc.status, tc.body)
			resp.Header.Set("Content-Type", "text/plain; charset=utf-8")
			state, effects := &executorState{}, &executorEffects{}
			result := (Executor{Client: &sequenceDoer{responses: []*http.Response{resp}}, State: state, Effects: effects}).Execute(attempt)
			accountFailures := 1
			if tc.commit {
				accountFailures = 0
			}
			if state.failures != accountFailures || state.modelFailures != 0 || state.rateLimits != 0 || state.learned != 0 ||
				effects.failures != 0 || effects.rateLimits != 0 || provider.refreshes != 0 {
				t.Fatalf("state=%+v effects=%+v refreshes=%d", state, effects, provider.refreshes)
			}
			if tc.commit {
				if !result.Committed || writer.Code != tc.status || writer.Body.String() != tc.body || effects.commits != 1 || effects.failovers != 0 || state.releases != 1 {
					t.Fatalf("commit=%v status=%d body=%q effects=%+v releases=%d", result.Committed, writer.Code, writer.Body.String(), effects, state.releases)
				}
				return
			}
			if result.Committed || result.Outcome != OutcomeFailedHard || result.AttemptStatus != tc.status || string(result.AttemptBody) != tc.body ||
				effects.failovers != 1 || effects.commits != 0 || writer.Body.Len() != 0 || state.releases != 0 {
				t.Fatalf("result=%+v effects=%+v releases=%d", result, effects, state.releases)
			}
		})
	}
}

// TestExecutorQuotaProofOutranksSubscriptionAttribution: a denial body that
// carries both a quota marker and the lapsed-subscription statement keeps the
// quota verdict — a rate-limit cooldown with a horizon, not a circuit tick.
// The subscription attribution sits behind the body-proven quota path.
func TestExecutorQuotaProofOutranksSubscriptionAttribution(t *testing.T) {
	provider := &executorTestProvider{}
	attempt, writer := testAttempt(provider, "{}", Policy{})
	body := `{"error":{"code":"quota_exceeded","message":"quota exceeded: you have no active step plan subscription"}}`
	state, effects := &executorState{}, &executorEffects{}
	result := (Executor{Client: &sequenceDoer{responses: []*http.Response{testResponse(http.StatusPaymentRequired, body)}}, State: state, Effects: effects}).Execute(attempt)
	if result.Committed || result.Outcome != OutcomeRateLimited || writer.Body.Len() != 0 {
		t.Fatalf("result=%+v, want the rate-limited outcome", result)
	}
	if state.rateLimits != 1 || state.rateLimitDecision.Kind != RateLimitQuota || state.failures != 0 || state.modelFailures != 0 {
		t.Errorf("state=%+v, want one quota cooldown and no circuit tick", state)
	}
	if effects.rateLimits != 1 || effects.failovers != 1 || effects.failures != 0 || effects.commits != 0 {
		t.Errorf("effects=%+v", effects)
	}
}

// TestExecutorRecordsCredentialDenialOnlyForAccountAttributions: the three
// account attributions (401 after refresh, account-disablement code, lapsed
// plan subscription) each report ONE credential denial to the state port —
// the record the operator surfaces (credential banner, Accounts pill,
// schedule badge) read, so a dead account cannot hide behind its siblings
// carrying the traffic. Every other failure class stays silent: a model
// denial, a quota cooldown, a gateway refusal, a 5xx or a pre-wire
// auth-layer failure is not something the operator must fix on the account
// (auth-layer failures are the quota poll's to report).
func TestExecutorRecordsCredentialDenialOnlyForAccountAttributions(t *testing.T) {
	const lapsed = `{"error":{"message":"you have no active step plan subscription","type":"request_params_invalid"}}`
	const disabled = `{"error":{"code":"account_disabled"}}`
	cases := []struct {
		name         string
		statuses     []int
		bodies       []string
		authErr      error
		noImpl       bool
		wantKind     CredentialDenialKind
		wantEvidence string
	}{
		{name: "lapsed plan subscription", statuses: []int{400}, bodies: []string{lapsed}, wantKind: DenialPlanInactive, wantEvidence: lapsed},
		{name: "account disabled", statuses: []int{403}, bodies: []string{disabled}, wantKind: DenialAccountDisabled, wantEvidence: disabled},
		{name: "401 after refresh", statuses: []int{401, 401}, bodies: []string{"first", "second"}, wantKind: DenialAuth, wantEvidence: "second"},
		{name: "5xx", statuses: []int{500}, bodies: []string{"boom"}},
		{name: "model denied", statuses: []int{404}, bodies: []string{"model not found"}},
		{name: "rate limited", statuses: []int{429}, bodies: []string{"slow down"}},
		{name: "quota denied", statuses: []int{402}, bodies: []string{`{"error":{"code":"quota_exceeded"}}`}},
		{name: "gateway refusal", statuses: []int{403}, bodies: []string{"<html><head><title>403 Forbidden</title></head><body><center><h1>403 Forbidden</h1></center><hr><center>nginx</center></body></html>"}},
		{name: "generic 403", statuses: []int{403}, bodies: []string{`{"error":{"message":"forbidden"}}`}},
		{name: "auth-layer failure", statuses: []int{200}, bodies: []string{"ok"}, authErr: errors.New("mint aqp key: HTTP 401: Session expired")},
		{name: "no provider implementation", statuses: []int{200}, bodies: []string{"ok"}, noImpl: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prov := &executorTestProvider{authErr: tc.authErr}
			var impl provider.Provider = prov
			if tc.noImpl {
				impl = nil
			}
			attempt, _ := testAttempt(impl, "{}", Policy{})
			responses := make([]*http.Response, len(tc.statuses))
			for i := range responses {
				responses[i] = testResponse(tc.statuses[i], tc.bodies[i])
			}
			state, effects := &executorState{}, &executorEffects{}
			(Executor{Client: &sequenceDoer{responses: responses}, State: state, Effects: effects}).Execute(attempt)
			if tc.wantKind == "" {
				if len(state.denials) != 0 {
					t.Fatalf("denials = %+v, want none", state.denials)
				}
				return
			}
			if len(state.denials) != 1 {
				t.Fatalf("denials = %+v, want exactly one", state.denials)
			}
			if got := state.denials[0]; got.provider != "upstream" || got.kind != tc.wantKind || got.evidence != tc.wantEvidence {
				t.Fatalf("denial = %+v, want kind %q with the upstream evidence %q", got, tc.wantKind, tc.wantEvidence)
			}
		})
	}
}

// TestExecutorAuthLayerFailureKeepsObservedStatus pins the 2026-11-23 compass
// WAF behavior: while the gateway blocked it, every aqp call failed in
// AuthHeaders with "mint aqp key: HTTP 403: <html>" (521 legs over ~2h). The
// leg must fail over immediately, carry the auth endpoint's observed 403 (not
// the synthetic 502) with the error text as body evidence, and never dispatch
// an upstream request nor tick the failure circuit.
func TestExecutorAuthLayerFailureKeepsObservedStatus(t *testing.T) {
	provider := &executorTestProvider{authErr: fmt.Errorf("mint aqp key: %w: %s",
		provider.HTTPStatusError{Code: http.StatusForbidden}, "<html>")}
	attempt, writer := testAttempt(provider, "{}", Policy{})
	doer := &sequenceDoer{responses: []*http.Response{testResponse(200, "ok")}}
	state, effects := &executorState{}, &executorEffects{}
	result := (Executor{Client: doer, State: state, Effects: effects}).Execute(attempt)
	if result.Committed || result.Outcome != OutcomeFailedHard || result.AttemptStatus != http.StatusForbidden ||
		!strings.Contains(string(result.AttemptBody), "mint aqp key") || result.Retried != nil {
		t.Fatalf("result=%+v", result)
	}
	if doer.calls != 0 {
		t.Fatalf("auth failure must not dispatch upstream, calls=%d", doer.calls)
	}
	if effects.failovers != 1 || effects.failures != 0 || effects.commits != 0 || provider.refreshes != 0 || writer.Body.Len() != 0 {
		t.Fatalf("effects=%+v refreshes=%d", effects, provider.refreshes)
	}
	if state.releases != 1 || state.failures != 0 || state.rateLimits != 0 {
		t.Fatalf("state=%+v", state)
	}
}

func TestExecutorCommitUsesShapedBody(t *testing.T) {
	provider := &executorTestProvider{}
	attempt, _ := testAttempt(provider, `{"model":"model","store":false}`, Policy{LastTarget: true})
	doer := &sequenceDoer{responses: []*http.Response{testResponse(400, `Unsupported parameter: 'store'`), testResponse(200, "ok")}}
	effects := &executorEffects{}
	result := (Executor{Client: doer, State: &executorState{}, Effects: effects}).Execute(attempt)
	if !result.Committed || effects.commits != 1 || bytes.Contains(effects.lastCommit.Body, []byte(`"store"`)) ||
		!bytes.Equal(doer.bodies[1], effects.lastCommit.Body) {
		t.Fatalf("commit body = %s effects=%+v", effects.lastCommit.Body, effects)
	}
}

func TestExecutorRecordsModeAdaptedResponsesStateOnce(t *testing.T) {
	provider := &executorTestProvider{}
	writer := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "http://proxy.test/v1/responses", nil)
	plan := NewPlan(PlanInput{
		Target: configdomain.RouteTarget{Provider: "upstream", Model: "chat-model"},
		ProviderConfig: configdomain.Provider{
			Provider: "test", OpenAIBaseURL: "https://upstream.test",
		},
		Provider:        provider,
		ClientProtocol:  protocol.Responses,
		BackendProtocol: protocol.OpenAI,
		ClientPath:      "/v1/responses",
	})
	attempt := NewAttempt(
		Runtime{},
		plan,
		Exchange{
			Request: request,
			Writer:  writer,
			Body:    []byte(`{"model":"chat-model","stream":true,"messages":[]}`),
		},
		Scope{
			ResponsesHistory: []any{"history"},
			ResponsesSession: "session",
		},
		Policy{LastTarget: true},
	)
	responses := &executorResponses{}
	upstream := `{"id":"chat-1","object":"chat.completion","model":"chat-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`
	result := (Executor{
		Client:    &sequenceDoer{responses: []*http.Response{testResponse(http.StatusOK, upstream)}},
		State:     &executorState{},
		Responses: responses,
	}).Execute(attempt)
	if !result.Committed || responses.sseRecords != 1 || responses.jsonRecords != 0 {
		t.Fatalf("result=%+v responses=%+v", result, responses)
	}
	if !bytes.Contains(responses.body, []byte("response.completed")) ||
		!bytes.Equal(responses.body, writer.Body.Bytes()) {
		t.Fatalf("recorded=%q client=%q", responses.body, writer.Body.Bytes())
	}
}

// fastFailConnError builds the production error shape: *url.Error wrapping the
// connection-level sentinel, exactly what http.Client.Do surfaces when the
// peer severs an established connection mid-exchange.
func fastFailConnError(unwrapped error) error {
	return &url.Error{Op: "Post", URL: "https://upstream.test/v1/chat/completions", Err: unwrapped}
}

// TestExecutorRedispatchesFastFailConnThen200: one unexpected-EOF on an
// established connection is absorbed by a same-target redispatch — committed
// 200, two Do calls, and NO failure recorded (the breaker must not see the
// blip at all when the fresh connection succeeds).
func TestExecutorRedispatchesFastFailConnThen200(t *testing.T) {
	provider := &executorTestProvider{}
	attempt, writer := testAttempt(provider, `{"model":"model"}`, Policy{LastTarget: true})
	doer := &sequenceDoer{
		errors:    []error{fastFailConnError(io.ErrUnexpectedEOF), nil},
		responses: []*http.Response{nil, testResponse(200, `{"ok":true}`)},
	}
	state := &executorState{}
	effects := &executorEffects{}
	result := (Executor{Client: doer, State: state, Effects: effects}).Execute(attempt)
	if !result.Committed || doer.calls != 2 || provider.refreshes != 0 ||
		state.success != 1 || state.failures != 0 || state.releases != 0 ||
		effects.failures != 0 || effects.failovers != 0 || effects.commits != 1 || writer.Code != 200 {
		t.Fatalf("result=%+v calls=%d state=%+v effects=%+v status=%d",
			result, doer.calls, state, effects, writer.Code)
	}
	if string(doer.bodies[0]) != string(doer.bodies[1]) {
		t.Fatalf("redispatch mutated the body: %q vs %q", doer.bodies[0], doer.bodies[1])
	}
}

// TestExecutorRedispatchesGoAwayThen200: a server-initiated HTTP/2 GOAWAY
// (upstream connection recycle) takes the same fast-fail redispatch path —
// the stream was past LastStreamID, the server never processed the request,
// so one fresh-connection redispatch on the SAME target is safe. Observed
// live as periodic LastStreamID≈2000 ErrCode=NO_ERROR recycles killing every
// in-flight request at once (2026-10-14); without this, GOAWAY burned a
// failover hop plus a circuit tick for a routine connection recycle.
func TestExecutorRedispatchesGoAwayThen200(t *testing.T) {
	provider := &executorTestProvider{}
	attempt, writer := testAttempt(provider, `{"model":"model"}`, Policy{LastTarget: true})
	doer := &sequenceDoer{
		errors: []error{
			fastFailConnError(goAwayTestError()),
			nil,
		},
		responses: []*http.Response{nil, testResponse(200, `{"ok":true}`)},
	}
	state := &executorState{}
	effects := &executorEffects{}
	result := (Executor{Client: doer, State: state, Effects: effects}).Execute(attempt)
	if !result.Committed || doer.calls != 2 || provider.refreshes != 0 ||
		state.success != 1 || state.failures != 0 ||
		effects.failures != 0 || effects.failovers != 0 || effects.commits != 1 || writer.Code != 200 {
		t.Fatalf("result=%+v calls=%d state=%+v effects=%+v status=%d",
			result, doer.calls, state, effects, writer.Code)
	}
}

// goAwayTestError reproduces the production error shape: *url.Error wrapping
// net/http's HTTP/2 goAwayError message verbatim as captured from the live
// daemon log (2026/10/15 00:10:32).
func goAwayTestError() error {
	return errors.New(`http2: server sent GOAWAY and closed the connection; LastStreamID=1999, ErrCode=NO_ERROR, debug=""`)
}

// TestExecutorFastFailConnTwiceCountsOnceAndFailsOver: when the redispatch
// also fast-fails, the whole exchange counts as ONE failure (not two) and the
// request fails over.
func TestExecutorFastFailConnTwiceCountsOnceAndFailsOver(t *testing.T) {
	provider := &executorTestProvider{}
	attempt, _ := testAttempt(provider, `{"model":"model"}`, Policy{LastTarget: true})
	doer := &sequenceDoer{
		errors: []error{
			fastFailConnError(io.ErrUnexpectedEOF),
			fastFailConnError(syscall.ECONNRESET),
		},
		responses: []*http.Response{nil, nil},
	}
	state := &executorState{}
	effects := &executorEffects{}
	result := (Executor{Client: doer, State: state, Effects: effects}).Execute(attempt)
	if result.Committed || result.Outcome != OutcomeFailedHard || doer.calls != 2 ||
		state.failures != 1 || state.success != 0 ||
		effects.failovers != 1 || effects.failures != 1 || effects.commits != 0 {
		t.Fatalf("result=%+v calls=%d state=%+v effects=%+v",
			result, doer.calls, state, effects)
	}
}

// TestExecutorTimeoutErrorNotRedispatched: timeouts never take the fast-fail
// path — their wall-time budget is already spent, retrying would double the
// caller's wait. Single Do call, straight to failure + failover.
func TestExecutorTimeoutErrorNotRedispatched(t *testing.T) {
	provider := &executorTestProvider{}
	attempt, _ := testAttempt(provider, `{"model":"model"}`, Policy{LastTarget: true})
	doer := &sequenceDoer{
		errors:    []error{fastFailConnError(context.DeadlineExceeded)},
		responses: []*http.Response{nil},
	}
	state := &executorState{}
	effects := &executorEffects{}
	result := (Executor{Client: doer, State: state, Effects: effects}).Execute(attempt)
	if result.Committed || result.Outcome != OutcomeFailedHard || doer.calls != 1 ||
		state.failures != 1 || effects.failovers != 1 || effects.failures != 1 {
		t.Fatalf("result=%+v calls=%d state=%+v effects=%+v",
			result, doer.calls, state, effects)
	}
}

// TestExecutorFastFailConnDoesNotConsume401Slot: the redispatch is an
// independent retry slot — a connection blip followed by a stale-key 401 still
// gets its refresh retry, and the request commits.
func TestExecutorFastFailConnDoesNotConsume401Slot(t *testing.T) {
	provider := &executorTestProvider{}
	attempt, writer := testAttempt(provider, `{"model":"model"}`, Policy{LastTarget: true})
	doer := &sequenceDoer{
		errors: []error{
			fastFailConnError(io.ErrUnexpectedEOF),
			nil,
			nil,
		},
		responses: []*http.Response{
			nil,
			testResponse(401, "stale key"),
			testResponse(200, `{"ok":true}`),
		},
	}
	state := &executorState{}
	effects := &executorEffects{}
	result := (Executor{Client: doer, State: state, Effects: effects}).Execute(attempt)
	if !result.Committed || doer.calls != 3 || provider.refreshes != 1 ||
		state.success != 1 || state.failures != 0 || effects.commits != 1 || writer.Code != 200 {
		t.Fatalf("result=%+v calls=%d refresh=%d state=%+v effects=%+v status=%d",
			result, doer.calls, provider.refreshes, state, effects, writer.Code)
	}
}

// 2xx + HTML is a relay/WAW fake success: it must fail over with 5xx-style
// provider accounting (breaker tick, failure metric, failover effect) instead
// of committing markup to the client. Both detection arms — Content-Type
// text/html and a JSON-typed body that opens with an HTML prefix.
func TestExecutorFakeAPIReplyFailsOver(t *testing.T) {
	loginPage := "<!DOCTYPE html><html><head><title>Sign in to relay</title></head><body>login</body></html>"
	cases := []struct {
		name        string
		contentType string
		body        string
	}{
		{"content type html", "text/html; charset=utf-8", loginPage},
		{"html body with json content type", "application/json", loginPage},
	}
	for _, tc := range cases {
		provider := &executorTestProvider{}
		attempt, writer := testAttempt(provider, `{}`, Policy{LastTarget: true})
		response := testResponse(200, tc.body)
		response.Header.Set("Content-Type", tc.contentType)
		state := &executorState{}
		effects := &executorEffects{}
		result := (Executor{Client: &sequenceDoer{responses: []*http.Response{response}}, State: state, Effects: effects}).Execute(attempt)
		if result.Committed || result.Outcome != OutcomeFailedHard || result.AttemptStatus != http.StatusBadGateway ||
			state.failures != 1 || state.modelFailures != 0 || state.success != 0 ||
			effects.failures != 1 || effects.failovers != 1 || effects.commits != 0 ||
			writer.Body.Len() != 0 || !bytes.Contains(result.AttemptBody, []byte("Sign in to relay")) {
			t.Fatalf("%s: result=%+v state=%+v effects=%+v written=%q body=%q",
				tc.name, result, state, effects, writer.Body.String(), result.AttemptBody)
		}
	}
}

// The head peek must not disturb real commits: a plain JSON 200 still commits
// byte-identically, and a JSON body that merely mentions HTML later is not a
// verdict.
func TestExecutorRealAPIReplyCommitsAfterPeek(t *testing.T) {
	provider := &executorTestProvider{}
	attempt, writer := testAttempt(provider, `{}`, Policy{LastTarget: true})
	body := `{"id":"x","choices":[{"message":{"content":"<html> docs"}}]}`
	response := testResponse(200, body)
	response.Header.Set("Content-Type", "application/json")
	state := &executorState{}
	effects := &executorEffects{}
	result := (Executor{Client: &sequenceDoer{responses: []*http.Response{response}}, State: state, Effects: effects}).Execute(attempt)
	if !result.Committed || state.failures != 0 || effects.failovers != 0 || state.success != 1 ||
		writer.Code != 200 || writer.Body.String() != body {
		t.Fatalf("result=%+v state=%+v effects=%+v status=%d body=%q",
			result, state, effects, writer.Code, writer.Body.String())
	}
}

// A 5xx whose body proves a content refusal fails over WITHOUT provider
// failure accounting: no breaker tick, no failure metric — the account is
// healthy, the request content is the problem (magpie content-refusal bucket;
// its misreading once silently benched every account for days).
func TestExecutorContentRefusal5xxFailsOverWithoutCooldown(t *testing.T) {
	provider := &executorTestProvider{}
	attempt, _ := testAttempt(provider, `{}`, Policy{LastTarget: true})
	refusal := `{"error":{"message":"Output blocked by content filtering policy","type":"server_error"}}`
	state := &executorState{}
	effects := &executorEffects{}
	result := (Executor{
		Client:  &sequenceDoer{responses: []*http.Response{testResponse(http.StatusBadGateway, refusal)}},
		State:   state,
		Effects: effects,
	}).Execute(attempt)
	if result.Committed || result.Outcome != OutcomeFailedHard || result.AttemptStatus != http.StatusBadGateway ||
		state.failures != 0 || state.releases != 1 || effects.failures != 0 || effects.failovers != 1 ||
		!bytes.Equal(result.AttemptBody, []byte(refusal)) {
		t.Fatalf("result=%+v state=%+v effects=%+v", result, state, effects)
	}
}
