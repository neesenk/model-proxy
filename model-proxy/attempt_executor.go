package main

import (
	"net/http"
	"time"
)

// attemptState is the narrow mutable-runtime port required by target execution.
// Keeping it separate from Proxy prevents the target pipeline from acquiring
// new scheduler, reload, or Web dependencies by reaching into the god object.
type attemptState interface {
	modelLocked(provider, model string, now time.Time) bool
	takeHalfOpenSlot(name string, generations ...uint64) bool
	releaseHalfOpenSlot(name string, generations ...uint64)
	recordSuccess(name, model string, generations ...uint64)
	recordFailure(name string, sched Scheduling, generations ...uint64)
	recordModelFailure(provider, model string, sched Scheduling, generations ...uint64)
	recordRateLimit(name string, until time.Time, kind rateLimitKind, generations ...uint64)
	parseRateLimit(resp *http.Response, bodyPeek []byte, now time.Time, sched Scheduling) (time.Time, rateLimitKind)
	learnParamBlock(provider, model, param string, generations ...uint64) bool
	applyParamBlock(provider, model string, body []byte) []byte
	noteWireResponsesMiss(name string)
	runShadow(proto, bodyProto, calledModel, exposed string, shadow ShadowTarget, reqBody []byte, primaryReqID string)
	currentShadowRuntime() *shadowRuntime
}

var _ attemptState = (*Proxy)(nil)

// attemptExecutor owns the complete one-target I/O pipeline. Its collaborators
// are explicit so request execution no longer has unrestricted access to Proxy.
// The executor is a cheap request-local value; all pointers reference
// concurrency-safe, process-owned components.
type attemptExecutor struct {
	attemptState

	client         *http.Client
	metrics        *metricsStore
	tokens         *tokenCounter
	agents         *agentCounter
	reqLog         *requestLogger
	responsesState *responsesStateStore
	events         *eventHub
}

func (p *Proxy) targetExecutor() attemptExecutor {
	return attemptExecutor{
		attemptState:   p,
		client:         p.client,
		metrics:        p.metrics,
		tokens:         p.tokens,
		agents:         p.agents,
		reqLog:         p.reqLog,
		responsesState: p.responsesState,
		events:         p.events,
	}
}

func (p *Proxy) currentShadowRuntime() *shadowRuntime {
	return p.shadow.Load()
}
