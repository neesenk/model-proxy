package targetexec

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	configdomain "model-proxy/internal/config"
)

// Regression (observed live 2026-11-16, 57 status-0 legs): a target denied by
// a PRE-DISPATCH gate (model lock from an earlier zero-byte 200, or an open
// circuit refusing a half-open probe slot) returned a bare Result{} — no
// outcome, no status, no log. forward then logged "failed (status 0); trying
// next" whose comment promised an "upstream error:" line that never exists for
// this path (transport errors carry status 502 and DO log that line), and the
// request-log attempt row serialized a synthetic empty 502 indistinguishable
// from a real network fault.

// gateDenyState overrides the two pre-dispatch gates.
type gateDenyState struct {
	*executorState
	locked   bool
	halfOpen bool
}

func (s *gateDenyState) ModelLocked(configdomain.RouteTarget, time.Time) bool { return s.locked }
func (s *gateDenyState) TakeHalfOpenSlot(string) bool                         { return s.halfOpen }

func TestExecutorGateDenialReportsSkipReason(t *testing.T) {
	cases := []struct {
		name       string
		state      *gateDenyState
		wantReason string
	}{
		{"model locked", &gateDenyState{executorState: &executorState{}, locked: true, halfOpen: true}, "model locked"},
		{"circuit open", &gateDenyState{executorState: &executorState{}, locked: false, halfOpen: false}, "circuit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doer := &transportErrorDoer{}
			attempt := streamingGateAttempt(t)
			result := (Executor{Client: doer, State: tc.state}).Execute(attempt)
			if result.Committed || result.Outcome != OutcomeSkipped {
				t.Fatalf("result = %+v; want OutcomeSkipped, uncommitted", result)
			}
			if !strings.Contains(result.SkipReason, tc.wantReason) {
				t.Fatalf("SkipReason = %q; want it to contain %q", result.SkipReason, tc.wantReason)
			}
			if doer.calls != 0 {
				t.Fatalf("upstream Do calls = %d; a gate denial must never touch the network", doer.calls)
			}
			// No provider verdict: the denied gate must not tick the circuit,
			// count a failure or consume a half-open slot.
			if tc.state.failures != 0 || tc.state.modelFailures != 0 || tc.state.releases != 0 || tc.state.rateLimits != 0 {
				t.Fatalf("gate denial mutated provider state: %+v", tc.state.executorState)
			}
		})
	}
}

func streamingGateAttempt(t *testing.T) Attempt {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions", nil)
	return streamingAttempt(t, request, httptest.NewRecorder())
}
