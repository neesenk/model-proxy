// loginput.go owns the application→data-plane input mapping for request
// logging: building one Input from request/route state and completing the
// record with its captured response.
package requestlog

import (
	"net/http"
	"time"

	configdomain "model-proxy/internal/config"
)

// LogCtx carries stable request identity into target execution logging.
// Reload generation remains owned by targetexec.Attempt.Runtime and is not
// duplicated in this observation scope.
type LogCtx struct {
	RequestID string
	Attempt   int
	Exposed   string
	OrigBody  []byte
}

// BuildInput maps application-owned request/route state to the detached
// value contract consumed by the request-log data plane.
func BuildInput(
	context LogCtx,
	request *http.Request,
	protocol string,
	calledModel string,
	target configdomain.RouteTarget,
	response *http.Response,
	startedAt time.Time,
	upstreamRequestBody []byte,
) Input {
	requestBody := context.OrigBody
	if len(requestBody) == 0 {
		requestBody = upstreamRequestBody
	}
	return Input{
		StartedAt:      startedAt,
		RequestID:      context.RequestID,
		SessionID:      request.Header.Get("x-claude-code-session-id"),
		Protocol:       protocol,
		Method:         request.Method,
		Path:           request.URL.Path,
		CalledModel:    calledModel,
		UpstreamModel:  target.Model,
		Exposed:        context.Exposed,
		Provider:       target.Provider,
		Attempt:        context.Attempt,
		Status:         response.StatusCode,
		RequestBody:    requestBody,
		ResponseHeader: response.Header.Clone(),
	}
}

// Complete enqueues one finished request record.
func Complete(
	logger *Logger,
	input Input,
	responseBody []byte,
	responseSize int64,
	truncated bool,
) {
	input.Timestamp = time.Now()
	input.ResponseBody = responseBody
	input.ResponseSize = responseSize
	input.ResponseTruncated = truncated
	logger.Enqueue(logger.BuildRecord(input))
}
