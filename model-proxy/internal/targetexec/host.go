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
	RecordModelFailure(provider, model string, scheduling configdomain.Scheduling, generation uint64)
	RecordRateLimit(provider string, until time.Time, kind string, generation uint64)
	LearnParamBlock(provider, model, parameter string, generation uint64) bool
	ApplyParamBlock(provider, model string, body []byte) []byte
	NoteWireResponsesMiss(provider string)
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

func (state GateState) LearnParamBlock(target configdomain.RouteTarget, parameter string) bool {
	return state.Gate.LearnParamBlock(target.Provider, target.Model, parameter, state.Runtime.Generation)
}

func (state GateState) ApplyParamBlock(target configdomain.RouteTarget, body []byte) []byte {
	return state.Gate.ApplyParamBlock(target.Provider, target.Model, body)
}

func (state GateState) NoteWireResponsesMiss(provider string) {
	state.Gate.NoteWireResponsesMiss(provider)
}
