package requestlog

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// Record is one line in the JSONL request log.
type Record struct {
	Ts              string `json:"ts"`
	Shadow          bool   `json:"shadow,omitempty"`
	RequestID       string `json:"request_id"`
	SessionID       string `json:"session_id"`
	Protocol        string `json:"protocol"`
	Method          string `json:"method"`
	Path            string `json:"path"`
	CalledModel     string `json:"called_model"`
	UpstreamModel   string `json:"upstream_model"`
	Exposed         string `json:"exposed"`
	Provider        string `json:"provider"`
	Attempt         int    `json:"attempt"`
	Status          int    `json:"status"`
	LatencyMs       int64  `json:"latency_ms"`
	RequestSize     int    `json:"request_size"`
	ResponseSize    int64  `json:"response_size"`
	RequestBody     string `json:"request_body"`
	ResponseBody    string `json:"response_body"`
	ResponseHeaders string `json:"response_headers,omitempty"`
}

// Input contains detached request/response facts used to build a Record.
type Input struct {
	Timestamp         time.Time
	StartedAt         time.Time
	RequestID         string
	SessionID         string
	Protocol          string
	Method            string
	Path              string
	CalledModel       string
	UpstreamModel     string
	Exposed           string
	Provider          string
	Attempt           int
	Status            int
	RequestBody       []byte
	ResponseBody      []byte
	ResponseSize      int64
	ResponseTruncated bool
	ResponseHeader    http.Header
}

const truncationMarker = "\n...[truncated by model-proxy request_log max_body_bytes]"

// RequestBodyTruncated reports whether this record is unsafe to replay.
func (r Record) RequestBodyTruncated() bool {
	return strings.HasSuffix(r.RequestBody, truncationMarker)
}

// BuildRecord creates one JSONL record from detached input. Selection of the
// original client body versus a rewritten upstream body belongs to the
// application adapter.
func (l *Logger) BuildRecord(in Input) *Record {
	now := in.Timestamp
	if now.IsZero() {
		now = time.Now()
	}
	latency := int64(0)
	if !in.StartedAt.IsZero() {
		latency = now.Sub(in.StartedAt).Milliseconds()
	}
	rec := &Record{
		Ts:              now.UTC().Format(time.RFC3339),
		Shadow:          strings.HasPrefix(in.RequestID, "shadow-"),
		RequestID:       in.RequestID,
		SessionID:       in.SessionID,
		Protocol:        in.Protocol,
		Method:          in.Method,
		Path:            in.Path,
		CalledModel:     in.CalledModel,
		UpstreamModel:   in.UpstreamModel,
		Exposed:         in.Exposed,
		Provider:        in.Provider,
		Attempt:         in.Attempt,
		Status:          in.Status,
		LatencyMs:       latency,
		ResponseSize:    in.ResponseSize,
		ResponseHeaders: responseHeaders(in.ResponseHeader),
	}

	rec.RequestSize = len(in.RequestBody)
	if len(in.RequestBody) > l.maxBodyBytes {
		rec.RequestBody = string(in.RequestBody[:l.maxBodyBytes]) + truncationMarker
	} else {
		rec.RequestBody = string(in.RequestBody)
	}
	rec.ResponseBody = string(in.ResponseBody)
	if in.ResponseTruncated {
		rec.ResponseBody += truncationMarker
	}
	return rec
}

func responseHeaders(header http.Header) string {
	if len(header) == 0 {
		return ""
	}
	allowed := map[string]string{}
	for _, name := range []string{"content-type", "x-request-id", "retry-after"} {
		if value := header.Get(name); value != "" {
			allowed[name] = value
		}
	}
	if len(allowed) == 0 {
		return ""
	}
	data, err := json.Marshal(allowed)
	if err != nil {
		return ""
	}
	return string(data)
}
