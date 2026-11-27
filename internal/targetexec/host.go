package targetexec

import (
	"time"

	configdomain "model-proxy/internal/config"
)

// HealthGate is the application-owned health/circuit capability the executor
// state delegates to. It mirrors the root Proxy's scheduling-adapter methods,
// bound to one captured runtime generation by the caller.
type HealthGate interface {
	ModelLocked(provider, model string, now time.Time) bool
	TakeHalfOpenSlot(provider string, generation uint64) bool
	ReleaseHalfOpenSlot(provider string, generation uint64)
	RecordSuccess(provider, model string, generation uint64)
	RecordFailure(provider string, scheduling configdomain.Scheduling, generation uint64)
	// RecordGatewayRefusal counts an edge/WAF 403 refusal towards the
	// account's circuit and nothing else (see State).
	RecordGatewayRefusal(provider string, scheduling configdomain.Scheduling, generation uint64)
	RecordModelFailure(provider, model string, scheduling configdomain.Scheduling, generation uint64)
	RecordRateLimit(provider string, until time.Time, kind string, generation uint64)
	RecordCredentialDenied(provider string, kind CredentialDenialKind, evidence []byte, generation uint64)
	LearnParamBlock(provider, model, parameter string, generation uint64) bool
	ApplyParamBlock(provider, model string, body []byte) []byte
	NoteWireResponsesMiss(provider, model string)
	// TakeInflight / ReleaseInflight are the account's in-flight ledger (see
	// State). Not generation-bound on purpose: a running request is a fact
	// about the process whatever generation captured it.
	TakeInflight(provider string) (InflightSlot, bool)
	ReleaseInflight(slot InflightSlot, outcome InflightOutcome) (raisedTo int, raised bool)
	// RecordConcurrencyLimit reports the concurrency limit a 429 proved:
	// limit as the upstream stated it (0 = unstated), inflight the other
	// requests running at dispatch. Returns the cap now in force and whether
	// this report moved it.
	RecordConcurrencyLimit(provider string, limit, inflight int, generation uint64) (cap int, changed bool)
}

// GateState adapts a HealthGate to the executor State port, freezing the
// scheduling config and runtime generation captured by one Attempt.
type GateState struct {
	Gate       HealthGate
	Runtime    Runtime
	Scheduling configdomain.Scheduling
}

var _ State = GateState{}

func (state GateState) ModelLocked(target configdomain.RouteTarget, now time.Time) bool {
	return state.Gate.ModelLocked(target.Provider, target.Model, now)
}

func (state GateState) TakeHalfOpenSlot(provider string) bool {
	return state.Gate.TakeHalfOpenSlot(provider, state.Runtime.Generation)
}

func (state GateState) ReleaseHalfOpenSlot(provider string) {
	state.Gate.ReleaseHalfOpenSlot(provider, state.Runtime.Generation)
}

func (state GateState) RecordSuccess(target configdomain.RouteTarget) {
	state.Gate.RecordSuccess(target.Provider, target.Model, state.Runtime.Generation)
}

func (state GateState) RecordFailure(provider string) {
	state.Gate.RecordFailure(provider, state.Scheduling, state.Runtime.Generation)
}

func (state GateState) RecordModelFailure(target configdomain.RouteTarget) {
	state.Gate.RecordModelFailure(target.Provider, target.Model, state.Scheduling, state.Runtime.Generation)
}

func (state GateState) RecordRateLimit(provider string, decision RateLimitDecision) {
	state.Gate.RecordRateLimit(provider, decision.Until, string(decision.Kind), state.Runtime.Generation)
}

func (state GateState) RecordCredentialDenied(provider string, kind CredentialDenialKind, evidence []byte) {
	state.Gate.RecordCredentialDenied(provider, kind, evidence, state.Runtime.Generation)
}

func (state GateState) RecordGatewayRefusal(provider string) {
	state.Gate.RecordGatewayRefusal(provider, state.Scheduling, state.Runtime.Generation)
}

func (state GateState) LearnParamBlock(target configdomain.RouteTarget, parameter string) bool {
	return state.Gate.LearnParamBlock(target.Provider, target.Model, parameter, state.Runtime.Generation)
}

func (state GateState) ApplyParamBlock(target configdomain.RouteTarget, body []byte) []byte {
	return state.Gate.ApplyParamBlock(target.Provider, target.Model, body)
}

func (state GateState) NoteWireResponsesMiss(target configdomain.RouteTarget) {
	state.Gate.NoteWireResponsesMiss(target.Provider, target.Model)
}

func (state GateState) TakeInflight(provider string) (InflightSlot, bool) {
	return state.Gate.TakeInflight(provider)
}

func (state GateState) ReleaseInflight(slot InflightSlot, outcome InflightOutcome) (int, bool) {
	return state.Gate.ReleaseInflight(slot, outcome)
}

func (state GateState) RecordConcurrencyLimit(provider string, decision RateLimitDecision) (int, bool) {
	return state.Gate.RecordConcurrencyLimit(provider, decision.ConcurrencyLimit, decision.ConcurrencyInflight, state.Runtime.Generation)
}
