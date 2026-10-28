package requestlog

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	configdomain "model-proxy/internal/config"
)

// Record is one line in the JSONL request log.
type Record struct {
	Ts     string `json:"ts"`
	Shadow bool   `json:"shadow,omitempty"`
	// AttemptOnly marks a non-committed failover leg (429/5xx/auth/transport):
	// same request id + attempt number as the committed row; excluded from
	// usage and session aggregates.
	AttemptOnly bool `json:"attempt_only,omitempty"`
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
	APIKeyID      string `json:"api_key,omitempty"`
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
	// Routing records the route-tier policy decision that produced the target
	// order for this request. Omitted when no route_policy applied, when a
	// hard selection (pin/force-provider) skipped the policy, or when no
	// policy step (latch/selector/band) matched.
	Routing *configdomain.RoutingDecision `json:"routing,omitempty"`
	// Schedule records which scheduling engine produced this pass's try order
	// (schedule.source: quota/sticky/pin/load_balance/force). Orthogonal to
	// Routing — the policy layer re-orders what the scheduler ordered.
	// Omitted when no target was schedulable.
	Schedule *ScheduleDecision `json:"schedule,omitempty"`
	// Profile is the request-derived routing profile (see RequestProfile).
	// Omitted from the JSONL line when nil (attempt legs, pre-field records).
	Profile *RequestProfile `json:"profile,omitempty"`
	// ParsedUsage is a query-time projection of ResponseBody (ExtractUsage)
	// for aggregate views that strip bodies as they read (Filter.UsageOnly).
	// Never persisted: the JSONL encoder does not write it and json:"-"
	// keeps decoded lines from populating it.
	ParsedUsage Usage `json:"-"`
	// FailedHops is the fold projection of summary list queries: this
	// request's failover legs, attached by the scan engine (or set directly
	// on the Summary by the index path) so paired legs stop appearing as
	// standalone rows. Never persisted; json:"-" keeps decoded lines clean.
	FailedHops []FailedHop `json:"-"`
}

// ConversionDiagnostic is the log projection of one conversion diagnostic.
type ConversionDiagnostic struct {
	Code   string `json:"code"`
	Detail string `json:"detail,omitempty"`
}

// ScheduleDecision captures which scheduling engine produced a pass's try
// order. Source vocabulary: "quota" (tier→priority→surplus ranking),
// "sticky" (an existing (session, route) binding held), "pin" (operator pin
// narrowed the chain), "load_balance" (rotation slot), "force" (a
// force-provider override collapsed the chain before ranking — stamped by
// the forward layer). The runtime constants are the producing authority;
// this struct is the persisted projection.
type ScheduleDecision struct {
	Source string `json:"source"`
	// Note explains a request-aware reroute: the per-target fit verdicts that
	// emptied (or narrowed) the route and the cross-route replacement chosen.
	// Empty on ordinary scheduling. Surfaced on the record's `schedule` field
	// and (as x-mp-reroute) on the response header.
	Note string `json:"note,omitempty"`
}

// RoleCounts is the persisted projection of the request's message-role
// composition (see internal/routing.CountRoles — the producing authority).
// Tool counts the tool-position forms of all three client protocols: openai
// tool/function roles and anthropic tool_result blocks.
type RoleCounts struct {
	System    int `json:"system,omitempty"`
	User      int `json:"user,omitempty"`
	Assistant int `json:"assistant,omitempty"`
	Tool      int `json:"tool,omitempty"`
}

// RequestProfile is the request-derived routing profile persisted with the
// record: the same facts the scheduler's request-aware filtering used
// (estimated input tokens, image/tools presence, follow-up turn), plus the
// conversation composition — one glance answers "what filled this context
// window" without opening the body (the magpie context-grid insight, landed
// as a log field). Omitted on records written before the field existed, on
// attempt-only failover legs, and when request logging is disabled.
type RequestProfile struct {
	EstimatedTokens int64      `json:"est_tokens"`
	Image           bool       `json:"image,omitempty"`
	Tools           bool       `json:"tools,omitempty"`
	FollowUp        bool       `json:"follow_up,omitempty"`
	Roles           RoleCounts `json:"roles"`
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
	APIKeyID          string
	Attempt           int
	Status            int
	TTFTMilliseconds  int64
	AttemptOnly       bool
	RequestBody       []byte
	ResponseBody      []byte
	ResponseSize      int64
	ResponseTruncated bool
	ResponseHeader    http.Header
	Diagnostics       []ConversionDiagnostic
	Routing           *configdomain.RoutingDecision
	Schedule          *ScheduleDecision
	Profile           *RequestProfile
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
		APIKeyID:        in.APIKeyID,
		Attempt:         in.Attempt,
		AttemptOnly:     in.AttemptOnly,
		Status:          in.Status,
		LatencyMs:       latency,
		TTFTMs:          in.TTFTMilliseconds,
		ResponseSize:    in.ResponseSize,
		ResponseHeaders: responseHeaders(in.ResponseHeader),
		Diagnostics:     in.Diagnostics,
		Routing:         in.Routing,
		Schedule:        in.Schedule,
		Profile:         in.Profile,
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
	rec.TurnKey = ComputeTurnKey(in.RequestBody)
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
	if rec.AttemptOnly {
		dst = append(dst, `,"attempt_only":true`...)
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
	if rec.APIKeyID != "" {
		dst = append(dst, `,"api_key":`...)
		dst = appendJSONString(dst, rec.APIKeyID)
	}
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
	if rec.Routing != nil {
		dst = append(dst, `,"routing":`...)
		dst = appendRoutingDecision(dst, rec.Routing)
	}
	if rec.Schedule != nil && (rec.Schedule.Source != "" || rec.Schedule.Note != "") {
		dst = append(dst, `,"schedule":{"source":`...)
		dst = appendJSONString(dst, rec.Schedule.Source)
		if rec.Schedule.Note != "" {
			dst = append(dst, `,"note":`...)
			dst = appendJSONString(dst, rec.Schedule.Note)
		}
		dst = append(dst, '}')
	}
	if rec.Profile != nil {
		dst = append(dst, `,"profile":{"est_tokens":`...)
		dst = strconv.AppendInt(dst, rec.Profile.EstimatedTokens, 10)
		if rec.Profile.Image {
			dst = append(dst, `,"image":true`...)
		}
		if rec.Profile.Tools {
			dst = append(dst, `,"tools":true`...)
		}
		if rec.Profile.FollowUp {
			dst = append(dst, `,"follow_up":true`...)
		}
		dst = append(dst, `,"roles":{`...)
		first := true
		appendRole := func(name string, n int) {
			if n == 0 {
				return
			}
			if !first {
				dst = append(dst, ',')
			}
			first = false
			dst = append(dst, '"')
			dst = append(dst, name...)
			dst = append(dst, `":`...)
			dst = strconv.AppendInt(dst, int64(n), 10)
		}
		appendRole("system", rec.Profile.Roles.System)
		appendRole("user", rec.Profile.Roles.User)
		appendRole("assistant", rec.Profile.Roles.Assistant)
		appendRole("tool", rec.Profile.Roles.Tool)
		dst = append(dst, '}', '}')
	}
	return append(dst, '}', '\n')
}

// appendRoutingDecision appends a JSON object for RoutingDecision.
func appendRoutingDecision(dst []byte, r *configdomain.RoutingDecision) []byte {
	dst = append(dst, '{')
	dst = append(dst, `"source":`...)
	dst = appendJSONString(dst, r.Source)
	if r.Grade != "" {
		dst = append(dst, `,"grade":`...)
		dst = appendJSONString(dst, r.Grade)
	}
	if r.Target != "" {
		dst = append(dst, `,"target":`...)
		dst = appendJSONString(dst, r.Target)
	}
	if r.Selector != nil {
		dst = append(dst, `,"selector":`...)
		dst = appendSelectorChoice(dst, r.Selector)
	}
	if r.Latch != "" {
		dst = append(dst, `,"latch":`...)
		dst = appendJSONString(dst, r.Latch)
	}
	dst = append(dst, '}')
	return dst
}

// appendSelectorChoice appends a JSON object for SelectorChoice.
func appendSelectorChoice(dst []byte, s *configdomain.SelectorChoice) []byte {
	dst = append(dst, '{')
	dst = append(dst, `"choice":`...)
	dst = appendJSONString(dst, s.Choice)
	dst = append(dst, `,"confidence":`...)
	dst = strconv.AppendFloat(dst, s.Confidence, 'f', -1, 64)
	dst = append(dst, `,"difficulty":`...)
	dst = strconv.AppendFloat(dst, s.Difficulty, 'f', -1, 64)
	dst = append(dst, `,"enforced":`...)
	if s.Enforced {
		dst = append(dst, "true"...)
	} else {
		dst = append(dst, "false"...)
	}
	if s.Err != "" {
		dst = append(dst, `,"err":`...)
		dst = appendJSONString(dst, s.Err)
	}
	dst = append(dst, '}')
	return dst
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
