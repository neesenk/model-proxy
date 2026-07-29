package targetexec

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	responsecache "model-proxy/internal/cache"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/protocol"
	"model-proxy/provider"
)

type executorTestProvider struct{ refreshes int }

func (p *executorTestProvider) AuthHeaders(*http.Request) error { return nil }
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
func (*executorTestProvider) ExtraHeaders(*http.Request, string)               {}
func (*executorTestProvider) FilterModelIDs(ids []string) ([]string, []string) { return ids, nil }

type sequenceDoer struct {
	responses []*http.Response
	bodies    [][]byte
	calls     int
}

func (d *sequenceDoer) Do(request *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(request.Body)
	d.bodies = append(d.bodies, body)
	response := d.responses[d.calls]
	d.calls++
	return response, nil
}

type executorState struct {
	failures, modelFailures, releases, success, rateLimits, learned int
	wireMisses                                                      int
}

func (*executorState) ModelLocked(configdomain.RouteTarget, time.Time) bool { return false }
func (*executorState) TakeHalfOpenSlot(string) bool                         { return true }
func (s *executorState) ReleaseHalfOpenSlot(string)                         { s.releases++ }
func (s *executorState) RecordSuccess(configdomain.RouteTarget)             { s.success++ }
func (s *executorState) RecordFailure(string)                               { s.failures++ }
func (s *executorState) RecordModelFailure(configdomain.RouteTarget)        { s.modelFailures++ }
func (s *executorState) RecordRateLimit(string, *http.Response, []byte, time.Time) RateLimit {
	s.rateLimits++
	return RateLimit{}
}
func (s *executorState) LearnParamBlock(configdomain.RouteTarget, string) bool {
	s.learned++
	return true
}
func (*executorState) ApplyParamBlock(_ configdomain.RouteTarget, body []byte) []byte { return body }
func (s *executorState) NoteWireResponsesMiss(string)                                 { s.wireMisses++ }

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
		result := (Executor{
			Client: &sequenceDoer{responses: []*http.Response{testResponse(status, "err")}}, State: state, Effects: effects,
		}).Execute(attempt)
		if result.Committed || effects.failovers != 1 ||
			(status == 429 && (result.Outcome != OutcomeRateLimited || state.rateLimits != 1 ||
				effects.rateLimits != 1 || effects.failures != 0)) ||
			(status == 503 && (state.failures != 1 || effects.failures != 1 || effects.rateLimits != 0)) {
			t.Fatalf("status %d result=%+v state=%+v effects=%+v", status, result, state, effects)
		}
	}
}

func TestExecutorCommitUsesShapedBody(t *testing.T) {
	provider := &executorTestProvider{}
	attempt, _ := testAttempt(provider, `{"model":"model","store":false}`, Policy{LastTarget: true})
	doer := &sequenceDoer{responses: []*http.Response{testResponse(400, `Unsupported parameter: 'store'`), testResponse(200, "ok")}}
	effects := &executorEffects{}
	result := (Executor{Client: doer, State: &executorState{}, Effects: effects}).Execute(attempt)
	if !result.Committed || bytes.Contains(result.Commit.RequestBody(), []byte(`"store"`)) ||
		effects.commits != 1 || !bytes.Equal(effects.lastCommit.Body, result.Commit.RequestBody()) {
		t.Fatalf("commit body = %s effects=%+v", result.Commit.RequestBody(), effects)
	}
}

func TestExecutorCachesOnlyCleanEOF(t *testing.T) {
	tests := []struct {
		name              string
		body              io.ReadCloser
		writer            http.ResponseWriter
		wantEntries       uint64
		wantModelFailures int
	}{
		{name: "clean eof", body: io.NopCloser(bytes.NewBufferString("complete")), wantEntries: 1},
		{
			name:              "clean empty eof locks model",
			body:              io.NopCloser(bytes.NewReader(nil)),
			wantModelFailures: 1,
		},
		{
			name:   "client write error",
			body:   io.NopCloser(bytes.NewBufferString("partial")),
			writer: failingResponseWriter{},
		},
		{
			name: "upstream read error",
			body: &errorReadCloser{err: io.ErrUnexpectedEOF},
		},
		{
			name: "partial upstream read error",
			body: &dataThenErrorReadCloser{data: []byte("partial"), err: io.ErrUnexpectedEOF},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := &executorTestProvider{}
			attempt, writer := testAttempt(provider, `{}`, Policy{LastTarget: true})
			cache := responsecache.New(responsecache.Options{
				TTL: time.Minute, MaxEntries: 4, MaxBodyBytes: 1024,
			})
			runtime := attempt.Runtime()
			runtime.Cache = cache
			scope := attempt.Scope()
			scope.CacheKey = "cache-key"
			exchange := attempt.Exchange()
			if test.writer != nil {
				exchange.Writer = test.writer
			}
			attempt = NewAttempt(runtime, attempt.Plan(), exchange, scope, attempt.Policy())
			response := testResponse(http.StatusOK, "")
			response.Body = test.body
			state := &executorState{}
			result := (Executor{
				Client: &sequenceDoer{responses: []*http.Response{response}},
				State:  state,
			}).Execute(attempt)
			if !result.Committed {
				t.Fatalf("result = %+v", result)
			}
			if got := cache.Stats().Entries; got != test.wantEntries {
				t.Fatalf("cache entries = %d, want %d; client body=%q", got, test.wantEntries, writer.Body.String())
			}
			if state.modelFailures != test.wantModelFailures {
				t.Fatalf("model failures = %d, want %d; client body=%q", state.modelFailures, test.wantModelFailures, writer.Body.String())
			}
		})
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

type dataThenErrorReadCloser struct {
	data []byte
	err  error
	done bool
}

func (reader *dataThenErrorReadCloser) Read(buffer []byte) (int, error) {
	if reader.done {
		return 0, reader.err
	}
	reader.done = true
	return copy(buffer, reader.data), reader.err
}

func (*dataThenErrorReadCloser) Close() error { return nil }
