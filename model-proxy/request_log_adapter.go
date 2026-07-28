package main

import (
	"log"
	"net/http"
	"time"

	"model-proxy/internal/observe/requestlog"
)

// requestLogInput maps application-owned request/route state to the detached
// value contract consumed by the request-log data plane.
func requestLogInput(
	context forwardLogCtx,
	request *http.Request,
	protocol string,
	calledModel string,
	target RouteTarget,
	response *http.Response,
	startedAt time.Time,
	upstreamRequestBody []byte,
) requestlog.Input {
	requestBody := context.origBody
	if len(requestBody) == 0 {
		requestBody = upstreamRequestBody
	}
	return requestlog.Input{
		StartedAt:      startedAt,
		RequestID:      context.requestID,
		SessionID:      request.Header.Get("x-claude-code-session-id"),
		Protocol:       protocol,
		Method:         request.Method,
		Path:           request.URL.Path,
		CalledModel:    calledModel,
		UpstreamModel:  target.Model,
		Exposed:        context.exposed,
		Provider:       target.Provider,
		Attempt:        context.attempt,
		Status:         response.StatusCode,
		RequestBody:    requestBody,
		ResponseHeader: response.Header.Clone(),
	}
}

func completeRequestLog(
	logger *requestlog.Logger,
	input requestlog.Input,
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

// initRequestLog adapts resolved configuration values into the process-owned
// logger. The goroutine itself remains owned by proxyLifecycle.
func (p *Proxy) initRequestLog(config RequestLogConfig) {
	if !config.Enabled {
		return
	}
	p.reqLog = requestlog.New(requestlog.Options{
		Directory:    config.ResolvedDir(),
		MaxFileSize:  config.MaxFileSizeBytes(),
		MaxBodyBytes: config.MaxBodyBytesValue(),
		Retention:    config.RetentionDuration(),
	})
	log.Printf(
		"[request_log] enabled -> %s (max_file_size %d bytes, max_body %d bytes, retention %s)",
		config.ResolvedDir(),
		config.MaxFileSizeBytes(),
		config.MaxBodyBytesValue(),
		config.RetentionDuration(),
	)
}
