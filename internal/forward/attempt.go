package forward

import (
	"net/http"
	"time"

	configdomain "model-proxy/internal/config"
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
	// Routing records the route-tier policy decision for this request.
	Routing *configdomain.RoutingDecision
	// Schedule names the engine that produced this pass's try order
	// (requestlog.ScheduleDecision; runtime.ScheduleSource* vocabulary plus
	// forward's force override). Nil omits the field.
	Schedule *requestlog.ScheduleDecision
	// Profile is the request-derived routing profile for the request log
	// (targetexec vocabulary; BuildRequestLogInput projects it onto the
	// persisted requestlog shape). Nil omits the field.
	Profile *targetexec.RequestProfile
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

// scheduleDecisionSource projects the optional ScheduleDecision onto the
// executor's plain-string log vocabulary (targetexec must not import the
// observation package).
func scheduleDecisionSource(d *requestlog.ScheduleDecision) string {
	if d == nil {
		return ""
	}
	return d.Source
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
	var schedule *requestlog.ScheduleDecision
	if context.Schedule != nil && context.Schedule.Source != "" {
		schedule = &requestlog.ScheduleDecision{Source: context.Schedule.Source}
	}
	var profile *requestlog.RequestProfile
	if context.Profile != nil {
		profile = &requestlog.RequestProfile{
			EstimatedTokens: context.Profile.EstimatedTokens,
			Image:           context.Profile.Image,
			Tools:           context.Profile.Tools,
			FollowUp:        context.Profile.FollowUp,
			Roles: requestlog.RoleCounts{
				System:    context.Profile.System,
				User:      context.Profile.User,
				Assistant: context.Profile.Assistant,
				Tool:      context.Profile.Tool,
			},
		}
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
			Routing:     context.Routing,
			Schedule:    schedule,
			Profile:     profile,
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
