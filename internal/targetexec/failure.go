package targetexec

import (
	"bytes"
	"encoding/json"
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
