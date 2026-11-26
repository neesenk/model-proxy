package targetexec

import (
	"net/http"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/protocol"
)

// Runtime contains only the reload-scoped facts consumed by one target
// execution. Scheduling and generation are copied from the request's snapshot.
type Runtime struct {
	Scheduling configdomain.Scheduling
	Generation uint64
}

// Exchange is the prepared HTTP exchange for one target. Body has already
// undergone model rewriting, Responses history expansion, and request
// conversion before the attempt is assembled.
type Exchange struct {
	Request *http.Request
	Writer  http.ResponseWriter
	Body    []byte
}

// LogContext carries stable request identity into the application-owned
// request-log adapter without exposing root package types to targetexec.
type LogContext struct {
	RequestID string
	// SessionID is the client session id resolved from the configured header
	// allowlist; carried alongside RequestID for the request log and live events.
	SessionID    string
	Attempt      int
	Exposed      string
	OriginalBody []byte
	// Diagnostics carries the attempt's request-conversion diagnostics
	// (structured lossy-conversion observations) into the request log.
	Diagnostics []ConversionDiagnostic
	// Routing records the route-tier policy decision that selected the target
	// order. It is log metadata only; the executor does not act on it.
	Routing *configdomain.RoutingDecision
	// ScheduleSource names the scheduling engine that produced the try order
	// (quota/sticky/pin/load_balance/force vocabulary). Log metadata only.
	ScheduleSource string
	// Profile is the request-derived routing profile (see RequestProfile).
	// Log metadata only; nil omits the field.
	Profile *RequestProfile
}

// ConversionDiagnostic is the wire-independent projection of a protocol
// conversion diagnostic.
type ConversionDiagnostic struct {
	Code   string
	Detail string
}

// RequestProfile is the log projection of the request's routing profile:
// the body-derived facts the scheduler's request-aware filtering used
// (estimated input tokens, image/tools presence, follow-up turn) plus the
// conversation role composition — one glance answers "what filled this
// context window" without opening the body. Wire-independent vocabulary like
// ConversionDiagnostic; the producing authority is internal/routing and the
// persisted authority is internal/observe/requestlog.
type RequestProfile struct {
	EstimatedTokens int64
	Image           bool
	Tools           bool
	FollowUp        bool
	System          int
	User            int
	Assistant       int
	Tool            int
}

// Scope contains request identity and protocol state that is neither target
// planning nor scheduling policy.
type Scope struct {
	CalledModel      string
	Agent            string
	Log              LogContext
	ResponseContext  protocol.ResponseContext
	ResponsesHistory []any
	ResponsesSession string
}

// Policy contains the few scheduling decisions used during one execution.
// ContextRetry is deliberately typed in terms of the config-domain value,
// rather than a root alias, so the executor can preserve the current rule:
// only abandon the upstream 4xx when a larger compatible target really exists.
type Policy struct {
	Force        bool
	LastTarget   bool
	ContextRetry func() []configdomain.RouteTarget
}

// Attempt is the complete target-execution contract. Its five groups have one
// source each and can only be assembled through NewAttempt.
type Attempt struct {
	runtime  Runtime
	plan     Plan
	exchange Exchange
	scope    Scope
	policy   Policy
}

func NewAttempt(runtime Runtime, plan Plan, exchange Exchange, scope Scope, policy Policy) Attempt {
	return Attempt{
		runtime:  runtime,
		plan:     plan,
		exchange: exchange,
		scope:    scope,
		policy:   policy,
	}
}

func (attempt Attempt) Runtime() Runtime   { return attempt.runtime }
func (attempt Attempt) Plan() Plan         { return attempt.plan }
func (attempt Attempt) Exchange() Exchange { return attempt.exchange }
func (attempt Attempt) Scope() Scope       { return attempt.scope }
func (attempt Attempt) Policy() Policy     { return attempt.policy }

// Outcome classifies an uncommitted attempt for the root terminal-status
// decision: pure rate limits end as 429, while any hard failure yields 502.
type Outcome int

const (
	OutcomeNone Outcome = iota
	OutcomeFailedHard
	OutcomeRateLimited
	// OutcomeClientGone: the caller abandoned the request (disconnect or its
	// own deadline) before the upstream answered. It is not a provider verdict
	// — no circuit-breaker tick, no failover effects — and orchestration stops
	// the failover loop instead of burning the remaining targets on a dead
	// request context.
	OutcomeClientGone
	// OutcomeSkipped: a pre-dispatch gate denied the target (model lock from
	// an earlier zero-byte 200, or an open circuit refusing a half-open probe
	// slot). No network contact happened and no provider verdict exists — no
	// circuit tick, no failure count — so this is routing telemetry, not a
	// failure. SkipReason names the gate for serve.log and the request-log
	// leg row; without it the leg surfaces as a bare status 0 that no log
	// line explains (observed live 2026-11-16: 57 such legs).
	OutcomeSkipped
)

// Result keeps scheduling outside the executor while making the target-level
// outcome explicit.
type Result struct {
	Committed bool
	Retried   []configdomain.RouteTarget
	Outcome   Outcome
	// AttemptStatus/AttemptBody carry the observed status and a bounded body
	// peek of a NON-committed failover leg (429 / >=500 / auth / transport
	// error), so the pipeline can write an attempt_only request-log row for
	// the leg. Zero status = no observation (synthesize upstream-side).
	AttemptStatus int
	AttemptBody   []byte
	// SkipReason names the denying gate for an OutcomeSkipped result ("model
	// locked", "circuit open (no half-open probe slot)"). It rides the leg's
	// serve.log line and request-log evidence body; empty for other outcomes.
	SkipReason string
}
