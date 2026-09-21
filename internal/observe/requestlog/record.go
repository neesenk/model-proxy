package requestlog

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Record is one line in the JSONL request log.
type Record struct {
	Ts     string `json:"ts"`
	Shadow bool   `json:"shadow,omitempty"`
	// Kind separates traffic classes sharing the log: empty = LLM forward
	// traffic (the historical default, kept empty for back-compat), "mcp" =
	// MCP gateway exchanges (/mcp/<name>).
	Kind      string `json:"kind,omitempty"`
	RequestID string `json:"request_id"`
	SessionID string `json:"session_id"`
	Protocol  string `json:"protocol"`
	Method    string `json:"method"`
	Path      string `json:"path"`
	// Tool is the client-facing tool name of an MCP tools/call exchange
	// (the JSON-RPC params.name; route rows carry the canonical route name,
	// not the backend's rewritten one). Empty for every other method and on
	// records written before the field existed.
	Tool          string `json:"tool,omitempty"`
	CalledModel   string `json:"called_model"`
	UpstreamModel string `json:"upstream_model"`
	Exposed       string `json:"exposed"`
	Provider      string `json:"provider"`
	Agent         string `json:"agent"`
	Attempt       int    `json:"attempt"`
	Status        int    `json:"status"`
	LatencyMs     int64  `json:"latency_ms"`
	// TTFTMs is time-to-first-byte of the committed response body (proxy
	// pipeline's first read). Streams ≈ first token; buffered conversions ≈
	// total. 0 = unknown (records written before the field existed).
	TTFTMs          int64  `json:"ttft_ms,omitempty"`
	RequestSize     int    `json:"request_size"`
	ResponseSize    int64  `json:"response_size"`
	RequestBody     string `json:"request_body"`
	ResponseBody    string `json:"response_body"`
	ResponseHeaders string `json:"response_headers,omitempty"`
	// TurnKey is a fingerprint of this request's conversational turn. Empty on
	// records written before the field existed or when the body carries no user
	// text; omitted from the JSONL line when empty.
	TurnKey string `json:"turn_key,omitempty"`
	// Diagnostics lists the attempt's protocol-conversion diagnostics
	// (structured lossy-conversion observations, stable codes).
	Diagnostics []ConversionDiagnostic `json:"diagnostics,omitempty"`
	// ParsedUsage is a query-time projection of ResponseBody (ExtractUsage)
	// for aggregate views that strip bodies as they read (Filter.UsageOnly).
	// Never persisted: the JSONL encoder does not write it and json:"-"
	// keeps decoded lines from populating it.
	ParsedUsage Usage `json:"-"`
}

// ConversionDiagnostic is the log projection of one conversion diagnostic.
type ConversionDiagnostic struct {
	Code   string `json:"code"`
	Detail string `json:"detail,omitempty"`
}

// Input contains detached request/response facts used to build a Record.
type Input struct {
	Timestamp         time.Time
	StartedAt         time.Time
	Kind              string
	RequestID         string
	SessionID         string
	Protocol          string
	Method            string
	Path              string
	Tool              string
	CalledModel       string
	UpstreamModel     string
	Exposed           string
	Provider          string
	Agent             string
	Attempt           int
	Status            int
	TTFTMilliseconds  int64
	RequestBody       []byte
	ResponseBody      []byte
	ResponseSize      int64
	ResponseTruncated bool
	ResponseHeader    http.Header
	Diagnostics       []ConversionDiagnostic
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
		Kind:            in.Kind,
		RequestID:       in.RequestID,
		SessionID:       in.SessionID,
		Protocol:        in.Protocol,
		Method:          in.Method,
		Path:            in.Path,
		Tool:            in.Tool,
		CalledModel:     in.CalledModel,
		UpstreamModel:   in.UpstreamModel,
		Exposed:         in.Exposed,
		Provider:        in.Provider,
		Agent:           in.Agent,
		Attempt:         in.Attempt,
		Status:          in.Status,
		LatencyMs:       latency,
		TTFTMs:          in.TTFTMilliseconds,
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
	rec.TurnKey = computeTurnKey(in.RequestBody)
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

// appendRecordLine appends the JSONL encoding of rec to dst. The output is
// byte-identical to json.Marshal(rec) followed by a newline; encoding by hand
// lets the single writer encode onto its retained line buffer instead of
// paying json.Marshal's per-record buffer allocation and copy for large
// bodies. Field order and omitempty rules mirror the Record struct tags.
func appendRecordLine(dst []byte, rec *Record) []byte {
	dst = append(dst, `{"ts":`...)
	dst = appendJSONString(dst, rec.Ts)
	if rec.Shadow {
		dst = append(dst, `,"shadow":true`...)
	}
	if rec.Kind != "" {
		dst = append(dst, `,"kind":`...)
		dst = appendJSONString(dst, rec.Kind)
	}
	dst = append(dst, `,"request_id":`...)
	dst = appendJSONString(dst, rec.RequestID)
	dst = append(dst, `,"session_id":`...)
	dst = appendJSONString(dst, rec.SessionID)
	dst = append(dst, `,"protocol":`...)
	dst = appendJSONString(dst, rec.Protocol)
	dst = append(dst, `,"method":`...)
	dst = appendJSONString(dst, rec.Method)
	dst = append(dst, `,"path":`...)
	dst = appendJSONString(dst, rec.Path)
	if rec.Tool != "" {
		dst = append(dst, `,"tool":`...)
		dst = appendJSONString(dst, rec.Tool)
	}
	dst = append(dst, `,"called_model":`...)
	dst = appendJSONString(dst, rec.CalledModel)
	dst = append(dst, `,"upstream_model":`...)
	dst = appendJSONString(dst, rec.UpstreamModel)
	dst = append(dst, `,"exposed":`...)
	dst = appendJSONString(dst, rec.Exposed)
	dst = append(dst, `,"provider":`...)
	dst = appendJSONString(dst, rec.Provider)
	dst = append(dst, `,"agent":`...)
	dst = appendJSONString(dst, rec.Agent)
	dst = append(dst, `,"attempt":`...)
	dst = strconv.AppendInt(dst, int64(rec.Attempt), 10)
	dst = append(dst, `,"status":`...)
	dst = strconv.AppendInt(dst, int64(rec.Status), 10)
	dst = append(dst, `,"latency_ms":`...)
	dst = strconv.AppendInt(dst, rec.LatencyMs, 10)
	if rec.TTFTMs > 0 {
		dst = append(dst, `,"ttft_ms":`...)
		dst = strconv.AppendInt(dst, rec.TTFTMs, 10)
	}
	dst = append(dst, `,"request_size":`...)
	dst = strconv.AppendInt(dst, int64(rec.RequestSize), 10)
	dst = append(dst, `,"response_size":`...)
	dst = strconv.AppendInt(dst, rec.ResponseSize, 10)
	dst = append(dst, `,"request_body":`...)
	dst = appendJSONString(dst, rec.RequestBody)
	dst = append(dst, `,"response_body":`...)
	dst = appendJSONString(dst, rec.ResponseBody)
	if rec.ResponseHeaders != "" {
		dst = append(dst, `,"response_headers":`...)
		dst = appendJSONString(dst, rec.ResponseHeaders)
	}
	if rec.TurnKey != "" {
		dst = append(dst, `,"turn_key":`...)
		dst = appendJSONString(dst, rec.TurnKey)
	}
	if len(rec.Diagnostics) > 0 {
		dst = append(dst, `,"diagnostics":[`...)
		for i, d := range rec.Diagnostics {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = append(dst, `{"code":`...)
			dst = appendJSONString(dst, d.Code)
			if d.Detail != "" {
				dst = append(dst, `,"detail":`...)
				dst = appendJSONString(dst, d.Detail)
			}
			dst = append(dst, '}')
		}
		dst = append(dst, ']')
	}
	return append(dst, '}', '\n')
}

const hexDigits = "0123456789abcdef"

// appendJSONString appends s as a JSON string with the exact escaping rules
// of encoding/json (HTML escaping on): control characters, quote, backslash,
// '<', '>' and '&' are escaped, invalid UTF-8 becomes U+FFFD, and
// U+2028/U+2029 are escaped.
func appendJSONString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); {
		if b := s[i]; b < utf8.RuneSelf {
			if b >= 0x20 && b != '"' && b != '\\' && b != '<' && b != '>' && b != '&' {
				i++
				continue
			}
			dst = append(dst, s[start:i]...)
			switch b {
			case '"', '\\':
				dst = append(dst, '\\', b)
			case '\n':
				dst = append(dst, '\\', 'n')
			case '\r':
				dst = append(dst, '\\', 'r')
			case '\t':
				dst = append(dst, '\\', 't')
			default:
				dst = append(dst, '\\', 'u', '0', '0', hexDigits[b>>4], hexDigits[b&0xF])
			}
			i++
			start = i
			continue
		}
		c, size := utf8.DecodeRuneInString(s[i:])
		if c == utf8.RuneError && size == 1 {
			dst = append(dst, s[start:i]...)
			dst = append(dst, `\ufffd`...)
			i += size
			start = i
			continue
		}
		if c == '\u2028' || c == '\u2029' {
			dst = append(dst, s[start:i]...)
			dst = append(dst, '\\', 'u', '2', '0', '2', hexDigits[c&0xF])
			i += size
			start = i
			continue
		}
		i += size
	}
	dst = append(dst, s[start:]...)
	return append(dst, '"')
}
