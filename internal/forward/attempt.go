package forward

import (
	"net/http"
	"time"

	"model-proxy/internal/observe/requestlog"
	"model-proxy/internal/targetexec"
)

// LogCtx is the request-log observation scope for one attempt. It travels from
// the pipeline into targetexec.Scope.Log and back out through the app's
// targetexec.Effects adapter, so the data-plane mapping
// (internal/observe/requestlog) stays decoupled from pipeline vocabulary.
type LogCtx struct {
	RequestID string
	SessionID string
	Attempt   int
	Exposed   string
	Agent     string
	OrigBody  []byte
	// Diagnostics of THIS attempt's request conversion (empty on passthrough)
	Diagnostics []targetexec.ConversionDiagnostic
}

// newTargetAttempt is the single assembly point shared by normal routing and
// Fusion synthesis. Request rewriting, state expansion, and protocol conversion
// deliberately remain outside this factory because those operations have
// caller-specific semantics and may fail before an attempt can be executed.
func newTargetAttempt(
	runtime Snapshot,
	plan targetexec.Plan,
	exchange targetexec.Exchange,
	scope targetexec.Scope,
	policy targetexec.Policy,
) targetexec.Attempt {
	var scheduling Scheduling
	if runtime.Cfg != nil {
		scheduling = runtime.Cfg.Scheduling
	}
	return targetexec.NewAttempt(
		targetexec.Runtime{
			Scheduling: scheduling,
			Generation: runtime.Generation,
			Cache:      runtime.Cache,
		},
		plan,
		exchange,
		scope,
		policy,
	)
}

// BuildRequestLogInput adapts the pipeline's LogCtx vocabulary to
// requestlog.BuildInput.
func BuildRequestLogInput(
	context LogCtx,
	request *http.Request,
	protocol string,
	calledModel string,
	target RouteTarget,
	response *http.Response,
	startedAt time.Time,
	upstreamRequestBody []byte,
) requestlog.Input {
	diags := make([]requestlog.ConversionDiagnostic, 0, len(context.Diagnostics))
	for _, d := range context.Diagnostics {
		diags = append(diags, requestlog.ConversionDiagnostic{Code: d.Code, Detail: d.Detail})
	}
	return requestlog.BuildInput(
		requestlog.LogCtx{
			RequestID:   context.RequestID,
			SessionID:   context.SessionID,
			Attempt:     context.Attempt,
			Exposed:     context.Exposed,
			Agent:       context.Agent,
			OrigBody:    context.OrigBody,
			Diagnostics: diags,
		},
		request,
		protocol,
		calledModel,
		target,
		response,
		startedAt,
		upstreamRequestBody,
	)
}
