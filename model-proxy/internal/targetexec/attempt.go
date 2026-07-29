package targetexec

import (
	"net/http"

	responsecache "model-proxy/internal/cache"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/protocol"
)

// Runtime contains only the reload-scoped facts consumed by one target
// execution. Scheduling is copied from the request's runtime snapshot; Cache
// remains the store captured by that same generation.
type Runtime struct {
	Scheduling configdomain.Scheduling
	Generation uint64
	Cache      *responsecache.Store
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
	RequestID    string
	Attempt      int
	Exposed      string
	OriginalBody []byte
}

// Scope contains request identity and protocol state that is neither target
// planning nor scheduling policy.
type Scope struct {
	CalledModel      string
	Agent            string
	CacheKey         string
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
)

// Commit is the only post-commit datum orchestration needs for Shadow: the
// exact upstream request bytes after provider rewrite and parameter shaping.
type Commit struct {
	requestBody []byte
}

func NewCommit(requestBody []byte) *Commit {
	return &Commit{requestBody: requestBody}
}

func (commit *Commit) RequestBody() []byte {
	if commit == nil {
		return nil
	}
	return commit.requestBody
}

// Result keeps scheduling and Shadow orchestration outside the executor while
// making the target-level outcome explicit.
type Result struct {
	Committed bool
	Retried   []configdomain.RouteTarget
	Outcome   Outcome
	Commit    *Commit
}
