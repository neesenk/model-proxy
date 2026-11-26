package targetexec

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/protocol"
)

// inflightState is the executor state port backed by a real in-flight ledger
// and learned cap, so the executor's slot discipline is observed through the
// same contract the runtime Manager implements.
type inflightState struct {
	executorState
	cap      int
	inflight int
	// released records the outcome of every release, in order.
	released []InflightOutcome
	// notes records every RecordConcurrencyLimit call as {stated limit,
	// other requests in flight at dispatch}.
	notes []concurrencyNote
}

type concurrencyNote struct{ limit, inflight int }

func (s *inflightState) TakeInflight(provider string) (InflightSlot, bool) {
	slot := InflightSlot{Provider: provider, Level: s.inflight}
	s.inflight++
	return slot, s.cap == 0 || slot.Level < s.cap
}

func (s *inflightState) ReleaseInflight(_ InflightSlot, outcome InflightOutcome) (int, bool) {
	s.inflight--
	s.released = append(s.released, outcome)
	return s.cap, false
}

// closedCircuitState is an inflightState whose half-open probe gate refuses.
type closedCircuitState struct{ *inflightState }

func (*closedCircuitState) TakeHalfOpenSlot(string) bool { return false }

func (s *inflightState) RecordConcurrencyLimit(_ string, decision RateLimitDecision) (int, bool) {
	s.notes = append(s.notes, concurrencyNote{limit: decision.ConcurrencyLimit, inflight: decision.ConcurrencyInflight})
	if decision.ConcurrencyLimit > 0 {
		s.cap = decision.ConcurrencyLimit
	} else if s.cap == 0 || decision.ConcurrencyInflight < s.cap {
		s.cap = decision.ConcurrencyInflight
	}
	return s.cap, true
}

// hold occupies n slots of the account the way running requests would.
func (s *inflightState) hold(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, ok := s.TakeInflight("upstream"); !ok {
			t.Fatalf("holder %d refused on an account that must admit it", i)
		}
	}
}

// authFailAttempt is streamingGateAttempt against a provider whose auth
// layer fails, so the executor stops after taking its slot but before
// building the upstream request.
func authFailAttempt(t *testing.T) Attempt {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions", nil)
	plan := NewPlan(PlanInput{
		Target:          configdomain.RouteTarget{Provider: "upstream", Model: "model"},
		ProviderConfig:  configdomain.Provider{Provider: "test", OpenAIBaseURL: "https://upstream.test"},
		Provider:        &executorTestProvider{authErr: fmt.Errorf("mint aqp key: HTTP 401")},
		ClientProtocol:  protocol.OpenAI,
		BackendProtocol: protocol.OpenAI,
		ClientPath:      "/v1/chat/completions",
	})
	return NewAttempt(Runtime{}, plan,
		Exchange{Request: request, Writer: httptest.NewRecorder(), Body: []byte(`{"model":"model","stream":true,"messages":[]}`)},
		Scope{}, Policy{})
}

// TestExecutorSkipsSaturatedAccountBeforeDispatch: once an account's
// concurrency limit is learned, a request that would exceed it is refused
// before any network contact — the same pre-dispatch class as a model lock or
// an open circuit — and the refused take is still released. Before this the
// request was sent, the upstream answered 429, and the account was benched
// for the whole transient cooldown.
func TestExecutorSkipsSaturatedAccountBeforeDispatch(t *testing.T) {
	state := &inflightState{cap: 1}
	state.hold(t, 1)
	doer := &sequenceDoer{responses: []*http.Response{testResponse(http.StatusOK, `{"ok":true}`)}}
	result := (Executor{Client: doer, State: state}).Execute(streamingGateAttempt(t))
	if result.Committed || result.Outcome != OutcomeSkipped {
		t.Fatalf("result = %+v; want OutcomeSkipped, uncommitted", result)
	}
	if !strings.Contains(result.SkipReason, "concurrency saturated") {
		t.Fatalf("SkipReason = %q, want it to name concurrency saturation", result.SkipReason)
	}
	if doer.calls != 0 {
		t.Fatalf("upstream Do calls = %d; a saturated account must not be contacted", doer.calls)
	}
	if state.inflight != 1 || len(state.released) != 1 {
		t.Fatalf("in flight = %d, releases = %v; want only the holder left after one release", state.inflight, state.released)
	}
	// The refused take never reached the upstream: its release carries no
	// verdict, so the account's cap probe does not read it as a failure.
	if state.released[0] != InflightNoVerdict {
		t.Fatalf("refused take released as %v, want InflightNoVerdict", state.released[0])
	}
	// No provider verdict: nothing for the circuit, the failure metric or the
	// half-open probe slot.
	if state.failures != 0 || state.releases != 0 || state.rateLimits != 0 || state.success != 0 {
		t.Fatalf("saturation skip mutated provider health: %+v", &state.executorState)
	}
}

// TestExecutorForceDispatchesSaturatedAccount: a pin is a hard choice — the
// request goes out even when the account's slots are full, and it still
// occupies one while it runs.
func TestExecutorForceDispatchesSaturatedAccount(t *testing.T) {
	state := &inflightState{cap: 1}
	state.hold(t, 1)
	doer := &sequenceDoer{responses: []*http.Response{testResponse(http.StatusOK, `{"ok":true}`)}}
	attempt := streamingGateAttempt(t)
	attempt = NewAttempt(attempt.Runtime(), attempt.Plan(), attempt.Exchange(), attempt.Scope(), Policy{Force: true, LastTarget: true})
	result := (Executor{Client: doer, State: state}).Execute(attempt)
	if !result.Committed || doer.calls != 1 {
		t.Fatalf("result = %+v calls = %d; want the pinned request dispatched and committed", result, doer.calls)
	}
	if state.inflight != 1 || len(state.released) != 1 || state.released[0] != InflightSucceeded {
		t.Fatalf("in flight = %d, releases = %v; want the forced request counted, then released as a success", state.inflight, state.released)
	}
}

// panicDoer makes the transport blow up inside Execute.
type panicDoer struct{}

func (panicDoer) Do(*http.Request) (*http.Response, error) { panic("transport exploded") }

// TestExecutorReleasesInflightOnEveryExit: whatever way an attempt ends, the
// slot it took goes back — a leaked slot would read as permanent saturation.
// The release reports how the request ended, and only the upstream's own
// answer is a verdict: a 2xx is the probe-up success, anything else the
// upstream answered (or a transport that never answered) is a failure, and an
// attempt the gates or the auth layer stopped before dispatch — or the caller
// abandoned — carries no verdict at all.
func TestExecutorReleasesInflightOnEveryExit(t *testing.T) {
	// clientGone builds an attempt whose caller already left: the transport
	// fails while the request context is canceled.
	clientGone := func(t *testing.T) (Attempt, *sequenceDoer, *inflightState) {
		doer := &sequenceDoer{responses: []*http.Response{nil}, errors: []error{context.Canceled}}
		request := httptest.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions", nil)
		ctx, cancel := context.WithCancel(request.Context())
		cancel()
		return streamingAttempt(t, request.WithContext(ctx), httptest.NewRecorder()), doer, &inflightState{}
	}
	cases := []struct {
		name      string
		responses []*http.Response
		errors    []error
		wantDo    int
		want      InflightOutcome
	}{
		{"transport error", nil, []error{errors.New("boom")}, 1, InflightFailed},
		{"401 after refresh", []*http.Response{testResponse(http.StatusUnauthorized, "no"), testResponse(http.StatusUnauthorized, "no")}, nil, 2, InflightFailed},
		{"429", []*http.Response{testResponse(http.StatusTooManyRequests, `{"error":"slow down"}`)}, nil, 1, InflightFailed},
		{"5xx", []*http.Response{testResponse(http.StatusInternalServerError, `{"error":"down"}`)}, nil, 1, InflightFailed},
		{"2xx", []*http.Response{testResponse(http.StatusOK, `{"ok":true}`)}, nil, 1, InflightSucceeded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doer := &sequenceDoer{responses: tc.responses, errors: tc.errors}
			if len(tc.responses) == 0 {
				doer.responses = []*http.Response{nil}
			}
			state := &inflightState{}
			_ = (Executor{Client: doer, State: state}).Execute(streamingGateAttempt(t))
			if doer.calls != tc.wantDo || state.inflight != 0 || len(state.released) != 1 || state.released[0] != tc.want {
				t.Fatalf("calls = %d, in flight = %d, releases = %v; want %d calls, no leak, released as %v", doer.calls, state.inflight, state.released, tc.wantDo, tc.want)
			}
		})
	}
	t.Run("client gone", func(t *testing.T) {
		attempt, doer, state := clientGone(t)
		_ = (Executor{Client: doer, State: state}).Execute(attempt)
		// The caller left: the upstream was asked but its answer is nobody's
		// to read — not a verdict on the account (the client-cancel rule the
		// circuit already follows).
		if state.inflight != 0 || len(state.released) != 1 || state.released[0] != InflightNoVerdict {
			t.Fatalf("in flight = %d, releases = %v; want the slot released with no verdict", state.inflight, state.released)
		}
	})
	t.Run("circuit open", func(t *testing.T) {
		state := &closedCircuitState{&inflightState{}}
		doer := &sequenceDoer{responses: []*http.Response{testResponse(http.StatusOK, `{"ok":true}`)}}
		_ = (Executor{Client: doer, State: state}).Execute(streamingGateAttempt(t))
		if doer.calls != 0 || state.inflight != 0 || len(state.released) != 1 || state.released[0] != InflightNoVerdict {
			t.Fatalf("calls = %d, in flight = %d, releases = %v; want no dispatch and the slot released with no verdict", doer.calls, state.inflight, state.released)
		}
	})
	t.Run("auth error before dispatch", func(t *testing.T) {
		state := &inflightState{}
		doer := &sequenceDoer{responses: []*http.Response{testResponse(http.StatusOK, `{"ok":true}`)}}
		_ = (Executor{Client: doer, State: state}).Execute(authFailAttempt(t))
		if doer.calls != 0 || state.inflight != 0 || len(state.released) != 1 || state.released[0] != InflightNoVerdict {
			t.Fatalf("calls = %d, in flight = %d, releases = %v; want no dispatch and the slot released with no verdict", doer.calls, state.inflight, state.released)
		}
	})
	t.Run("panic", func(t *testing.T) {
		state := &inflightState{}
		defer func() {
			if recover() == nil {
				t.Fatal("the panicking transport did not panic")
			}
			if state.inflight != 0 || len(state.released) != 1 {
				t.Fatalf("in flight = %d, releases = %v after a panic; want the slot released", state.inflight, state.released)
			}
		}()
		_ = (Executor{Client: panicDoer{}, State: state}).Execute(streamingGateAttempt(t))
	})
}

// TestExecutorConcurrency429TeachesLimitInsteadOfCooldown: a 429 that proves
// a concurrency limit updates the account's cap and starts no time cooldown —
// the cap plus the in-flight count are what keep the next request off the
// account. The proof is the upstream's own statement, or the proxy's own
// observation that other requests were running when the 429 came; a 429 with
// nothing else in flight is not about concurrency and keeps the ordinary
// transient cooldown.
func TestExecutorConcurrency429TeachesLimitInsteadOfCooldown(t *testing.T) {
	stated := `{"error":{"message":"concurrency reached, current: 3, limit: 2","type":"rate_limited"}}`
	markerless := `{"type":"error","error":{"type":"rate_limit_error","code":"1302","message":"[1302][您的账户已达到速率限制，请您控制请求频率][20261126001642]"}}`
	cases := []struct {
		name      string
		body      string
		held      int
		wantNote  concurrencyNote
		cooldowns int
	}{
		{"upstream states the limit", stated, 2, concurrencyNote{limit: 2, inflight: 2}, 0},
		{"inferred from requests in flight", markerless, 4, concurrencyNote{limit: 0, inflight: 4}, 0},
		{"nothing else in flight is not concurrency", markerless, 0, concurrencyNote{}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := &inflightState{}
			state.hold(t, tc.held)
			doer := &sequenceDoer{responses: []*http.Response{testResponse(http.StatusTooManyRequests, tc.body)}}
			result := (Executor{Client: doer, State: state}).Execute(streamingGateAttempt(t))
			if result.Committed || result.Outcome != OutcomeRateLimited || result.AttemptStatus != http.StatusTooManyRequests {
				t.Fatalf("result = %+v; want an uncommitted rate-limited leg carrying the 429", result)
			}
			if tc.cooldowns == 0 {
				if len(state.notes) != 1 || state.notes[0] != tc.wantNote {
					t.Fatalf("concurrency notes = %+v; want exactly %+v", state.notes, tc.wantNote)
				}
				if state.rateLimits != 0 {
					t.Fatalf("time cooldowns recorded = %d; a concurrency limit is not a time horizon", state.rateLimits)
				}
			} else if len(state.notes) != 0 || state.rateLimits != 1 || state.rateLimitDecision.Kind != RateLimitTransient {
				t.Fatalf("notes = %+v, cooldowns = %d (%+v); want the plain transient cooldown and no cap", state.notes, state.rateLimits, state.rateLimitDecision)
			}
			if state.inflight != tc.held {
				t.Fatalf("in flight = %d after the leg; want the %d holders only", state.inflight, tc.held)
			}
		})
	}
}

// TestParseRateLimitConcurrency: a 429 whose body states a concurrency limit
// is read as such by every caller; a markerless 429 is concurrency only from
// the vantage point of a request that had company, and a quota or daily proof
// always outranks that inference.
func TestParseRateLimitConcurrency(t *testing.T) {
	now := time.Now()
	scheduling := configdomain.Scheduling{}
	stated := []byte(`{"error":{"message":"concurrency reached, current: 11, limit: 10","type":"rate_limited"}}`)
	if got := (RateLimitDecision{Kind: RateLimitConcurrency, ConcurrencyLimit: 10, Until: now}); ParseRateLimitFor(nil, stated, now, scheduling, 0) != got {
		t.Fatalf("stated limit (with vantage point) = %+v, want %+v", ParseRateLimitFor(nil, stated, now, scheduling, 0), got)
	}
	if got := ParseRateLimit(nil, stated, now, scheduling); got.Kind != RateLimitConcurrency || got.ConcurrencyLimit != 10 || got.ConcurrencyInflight != 0 {
		t.Fatalf("stated limit (no vantage point) = %+v, want concurrency / 10 / 0", got)
	}

	markerless := []byte(`{"type":"error","error":{"type":"rate_limit_error","code":"1302","message":"[1302][您的账户已达到速率限制，请您控制请求频率][20261126001642]"}}`)
	if got := ParseRateLimitFor(nil, markerless, now, scheduling, 4); got != (RateLimitDecision{Kind: RateLimitConcurrency, ConcurrencyInflight: 4, Until: now}) {
		t.Fatalf("markerless with company = %+v, want concurrency inferred at 4", ParseRateLimitFor(nil, markerless, now, scheduling, 4))
	}
	if got := ParseRateLimitFor(nil, markerless, now, scheduling, 0); got.Kind != RateLimitTransient || got.Until != now.Add(scheduling.RateBackoff()) {
		t.Fatalf("markerless alone = %+v, want the unchanged transient cooldown", ParseRateLimitFor(nil, markerless, now, scheduling, 0))
	}
	if got := ParseRateLimitFor(nil, []byte(`{"error":{"message":"Too many concurrent requests"}}`), now, scheduling, 3); got.Kind != RateLimitConcurrency || got.ConcurrencyLimit != 0 || got.ConcurrencyInflight != 3 {
		t.Fatalf("concurrency in words without a number = %+v, want concurrency inferred at 3", got)
	}

	quota := []byte(`{"error":{"message":"You exceeded your current quota","code":"insufficient_quota"}}`)
	if got := ParseRateLimitFor(nil, quota, now, scheduling, 5); got.Kind != RateLimitQuota || got.ConcurrencyInflight != 0 {
		t.Fatalf("quota proof with company = %+v, want quota with no concurrency inference", got)
	}
	// A reset hint is the upstream dating its own limit: time-based, so the
	// hint wins over what the request's company would suggest.
	if got := ParseRateLimitFor(nil, []byte(`rate limit exceeded, retry after 30s`), now, scheduling, 5); got.Kind != RateLimitTransient || got.Until != now.Add(30*time.Second) {
		t.Fatalf("markerless 429 with a reset hint = %+v, want the transient horizon from the hint", got)
	}
	if got := ParseRateLimitFor(&http.Response{Header: http.Header{"Retry-After": []string{"7"}}}, markerless, now, scheduling, 5); got.Kind != RateLimitTransient || got.Until != now.Add(7*time.Second) {
		t.Fatalf("markerless 429 with Retry-After = %+v, want the transient horizon from the header", got)
	}
}
