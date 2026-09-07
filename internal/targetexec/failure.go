package targetexec

import (
	"bytes"
	"encoding/json"
	"net/http"
	"regexp"
)

var contextOverflowMarkers = [][]byte{
	[]byte("context_length_exceeded"),
	[]byte("maximum context length"),
	[]byte("context window"),
	[]byte("context length"),
	[]byte("prompt is too long"),
	[]byte("reduce the length"),
	[]byte("too many tokens"),
}

func IsContextOverflow(status int, peek []byte) bool {
	if status < 400 || status >= 500 || len(peek) == 0 {
		return false
	}
	lower := bytes.ToLower(peek)
	for _, marker := range contextOverflowMarkers {
		if bytes.Contains(lower, marker) {
			return true
		}
	}
	return false
}

var modelDeniedMarkers = [][]byte{
	[]byte("model not found"),
	// shopee gateway: {"retcode":40403,"message":"Model not supported by this endpoint"}
	[]byte("not supported by this endpoint"),
	[]byte("model_not_found"),
	[]byte("model does not exist"),
	[]byte("does not exist"),
	[]byte("no such model"),
	[]byte("model is not available"),
	[]byte("model unavailable"),
	[]byte("do not have access to model"),
	[]byte("not have access to the model"),
	[]byte("no access to model"),
	[]byte("not entitled to access model"),
	[]byte("invalid model"),
	[]byte("unknown model"),
	[]byte("模型不存在"),
	[]byte("模型已下线"),
	[]byte("模型不可用"),
	[]byte("无权限访问模型"),
	[]byte("没有该模型的访问权限"),
}

func IsModelDenied(status int, peek []byte) bool {
	if status != 400 && status != 403 || len(peek) == 0 {
		return false
	}
	lower := bytes.ToLower(peek)
	for _, marker := range modelDeniedMarkers {
		if bytes.Contains(lower, marker) {
			return true
		}
	}
	return false
}

var (
	unsupportedParamRE  = regexp.MustCompile(`(?i)unsupported[ _]parameter[^a-z0-9]{0,12}["'` + "`" + `]?([a-zA-Z0-9_.\-]{1,64})`)
	unknownParamRE      = regexp.MustCompile(`(?i)unknown[ _](?:parameter|param|field|argument)[^a-z0-9]{0,12}["'` + "`" + `]?([a-zA-Z0-9_.\-]{1,64})`)
	unrecognizedParamRE = regexp.MustCompile(`(?i)unrecognized[ _](?:parameter|param|field)[^a-z0-9]{0,12}["'` + "`" + `]?([a-zA-Z0-9_.\-]{1,64})`)
	notSupportedRE      = regexp.MustCompile(`(?i)(?:parameter|param|field)[^a-z0-9]{0,12}["'` + "`" + `]?([a-zA-Z0-9_.\-]{1,64})["'` + "`" + `]?[^a-z0-9]{0,20}(?:is\s+)?not\s+(?:supported|allowed|recognized)`)
	jsonParamRE         = regexp.MustCompile(`"param"\s*:\s*"([a-zA-Z0-9_.\-]{1,64})"`)
)

var neverStripParams = map[string]bool{
	"model": true, "messages": true, "input": true, "prompt": true, "system": true,
	"stream": true, "tools": true, "tool_choice": true, "response_format": true,
}

func ParseUnsupportedParam(peek []byte) (string, bool) {
	for _, re := range []*regexp.Regexp{unsupportedParamRE, unknownParamRE, unrecognizedParamRE, notSupportedRE} {
		if match := re.FindSubmatch(peek); match != nil && !neverStripParams[string(match[1])] {
			return string(match[1]), true
		}
	}
	if bytes.Contains(bytes.ToLower(peek), []byte("unsupported")) {
		if match := jsonParamRE.FindSubmatch(peek); match != nil && !neverStripParams[string(match[1])] {
			return string(match[1]), true
		}
	}
	return "", false
}

func StripTopLevelParam(body []byte, param string) ([]byte, bool) {
	var object map[string]json.RawMessage
	if json.Unmarshal(body, &object) != nil {
		return body, false
	}
	if _, ok := object[param]; !ok {
		return body, false
	}
	delete(object, param)
	out, err := json.Marshal(object)
	if err != nil {
		return body, false
	}
	return out, true
}

// ParamDeveloperRole is the paramBlock lesson for chat upstreams that reject
// the OpenAI "developer" message role (volcengine: "invalid value: `developer`",
// deepseek: "unknown variant `developer`", kimi-code: "role 'developer' is not
// a valid role", zhipu: "角色信息不正确"). Applying it renames developer→system
// instead of stripping anything — system is the universally accepted spelling.
const ParamDeveloperRole = "developer_role"

// developerRoleRejectionREs match the observed 400 bodies of upstreams whose
// chat dialects predate the developer role. zhipu's wording is generic
// ("角色信息不正确" = role information incorrect), so it is gated on the error
// mentioning a role at all; the rename is idempotent for bodies that don't
// carry a developer message (RenameDeveloperRole reports changed=false).
var developerRoleRejectionREs = []*regexp.Regexp{
	regexp.MustCompile(`(?i)invalid value: ?["'` + "`" + `]?developer`),
	regexp.MustCompile(`(?i)unknown variant ?["'` + "`" + `]?developer`),
	regexp.MustCompile(`(?i)role ?["'` + "`" + `]?developer["'` + "`" + `]? .{0,24}(?:is not|not a valid|not valid|not supported|are not valid)`),
	regexp.MustCompile(`(?i)messages\.role.{0,80}developer`),
	regexp.MustCompile(`角色信息不正确`),
}

// IsDeveloperRoleRejection reports whether a 400 body is a chat-dialect
// rejection of the developer role specifically (mentions developer, or zhipu's
// generic role wording).
func IsDeveloperRoleRejection(status int, peek []byte) bool {
	if status != http.StatusBadRequest || len(peek) == 0 {
		return false
	}
	for _, re := range developerRoleRejectionREs {
		if re.Match(peek) {
			return true
		}
	}
	return false
}

// RenameDeveloperRole rewrites messages[i].role == "developer" to "system".
// Best-effort like StripTopLevelParam: a non-JSON body (or one without a
// developer message) passes through unchanged (changed=false).
func RenameDeveloperRole(body []byte) ([]byte, bool) {
	var object struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &object) != nil {
		return body, false
	}
	changed := false
	for i := range object.Messages {
		if object.Messages[i].Role == "developer" {
			changed = true
		}
	}
	if !changed {
		return body, false
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return body, false
	}
	var messages []json.RawMessage
	if json.Unmarshal(root["messages"], &messages) != nil {
		return body, false
	}
	for i, raw := range messages {
		var m map[string]json.RawMessage
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		if role, ok := m["role"]; ok && string(role) == `"developer"` {
			m["role"] = []byte(`"system"`)
			if out, err := json.Marshal(m); err == nil {
				messages[i] = out
			}
		}
	}
	marshaled := mustMarshalRaw(messages)
	if marshaled == nil {
		return body, false
	}
	root["messages"] = marshaled
	full, err := json.Marshal(root)
	if err != nil {
		return body, false
	}
	return full, true
}

func mustMarshalRaw(v any) json.RawMessage {
	out, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return out
}

// ParamThinkingAdaptive is the paramBlock lesson for anthropic upstreams that
// reject budget-based thinking (shopee's claude gateway: "\"thinking.type.enabled\"
// is not supported for this model. Use \"thinking.type.adaptive\""). Applying it
// rewrites the request's thinking object to {"type":"adaptive"}.
const ParamThinkingAdaptive = "thinking_adaptive"

// IsThinkingDialectRejection reports whether a 400 body is an anthropic
// upstream demanding the adaptive thinking spelling over budget-based
// enabled thinking.
func IsThinkingDialectRejection(status int, peek []byte) bool {
	if status != http.StatusBadRequest || len(peek) == 0 {
		return false
	}
	lower := bytes.ToLower(peek)
	return bytes.Contains(lower, []byte("thinking.type")) &&
		(bytes.Contains(lower, []byte("not supported")) || bytes.Contains(lower, []byte("unsupported"))) &&
		bytes.Contains(lower, []byte("adaptive"))
}

// RewriteThinkingAdaptive replaces a budget-based thinking config with the
// adaptive spelling. Best-effort: bodies without a thinking object (or with
// one already adaptive) pass through unchanged.
func RewriteThinkingAdaptive(body []byte) ([]byte, bool) {
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return body, false
	}
	raw, ok := root["thinking"]
	if !ok {
		return body, false
	}
	var thinking map[string]json.RawMessage
	if json.Unmarshal(raw, &thinking) != nil {
		return body, false
	}
	if t, ok := thinking["type"]; ok && string(t) == `"adaptive"` {
		return body, false
	}
	root["thinking"] = []byte(`{"type":"adaptive"}`)
	out, err := json.Marshal(root)
	if err != nil {
		return body, false
	}
	return out, true
}
