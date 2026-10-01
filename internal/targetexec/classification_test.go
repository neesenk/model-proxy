package targetexec

import (
	"encoding/json"
	"testing"
)

func TestIsModelDenied(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{400, `{"error":{"message":"The model 'gpt-x' does not exist"}}`, true},
		{404, `{"error":"model not found"}`, false},
		{403, `{"error":"You do not have access to model glm-x"}`, true},
		{400, `{"error":"model is not available in your region"}`, true},
		{400, `{"msg":"模型不存在"}`, true},
		{400, `{"error":"The Model 'gpt-x' Does Not Exist"}`, true},
		// v3 addition — shopee's retcode 40403 envelope. This marker triggers
		// model lock + failover, so the boundary matters:
		{400, `{"retcode":40403,"message":"Model not supported by this endpoint"}`, true},
		// "supported" substrings that are NOT model denials must not fire.
		{400, `{"error":"Streaming is not supported for this plan tier"}`, false},
		{400, `{"error":"Unsupported parameter: 'temperature' is not supported"}`, false},
		{400, `{"error":"invalid api key"}`, false},
		{400, `{"error":"max_tokens is too large"}`, false},
		{400, ``, false},
		{500, `{"error":"model not found"}`, false},
	}
	for _, test := range cases {
		if got := IsModelDenied(test.status, []byte(test.body)); got != test.want {
			t.Errorf("IsModelDenied(%d, %q) = %v, want %v", test.status, test.body, got, test.want)
		}
	}
}

func TestParseUnsupportedParam(t *testing.T) {
	cases := []struct {
		body string
		want string
		ok   bool
	}{
		{`{"error":{"message":"Unsupported parameter: 'max_tokens' is not supported with this model."}}`, "max_tokens", true},
		{`{"error":{"type":"invalid_request_error","param":"store","code":"unsupported_parameter"}}`, "store", true},
		{`{"error":"Unknown parameter: 'session_id'"}`, "session_id", true},
		{`{"error":"Unrecognized field: "foo""}`, "foo", true},
		{`{"error":"parameter 'topp' is not supported"}`, "topp", true},
		{`{"error":{"message":"Unsupported parameter: 'model'"}}`, "", false},
		{`{"error":{"message":"Unsupported parameter: 'messages'"}}`, "", false},
		{`{"error":"invalid api key"}`, "", false},
		{`{"error":"context length exceeded"}`, "", false},
		{`{"param":"store"}`, "", false},
		{``, "", false},
	}
	for _, test := range cases {
		got, ok := ParseUnsupportedParam([]byte(test.body))
		if got != test.want || ok != test.ok {
			t.Errorf("ParseUnsupportedParam(%q) = %q,%v, want %q,%v", test.body, got, ok, test.want, test.ok)
		}
	}
}

// TestIsContextOverflow: the 4xx body classifier — hits the context-overflow
// shapes of the known backends, never an ordinary client error. (Migrated from
// internal/app: pure classifier semantics belong to the owner package; the
// cross-route retry ORCHESTRATION stays in app's forward_retry tests.)
func TestIsContextOverflow(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"openai context_length_exceeded", 400,
			`{"error":{"message":"This model's maximum context length is 128000 tokens. However, your messages resulted in 200001 tokens. Please reduce the length of the messages.","type":"invalid_request_error","param":"messages","code":"context_length_exceeded"}}`, true},
		{"deepseek maximum context length", 400,
			`{"error":{"message":"This model's maximum context length is 65536 tokens. However, you requested 100000 tokens (100000 in the messages, 0 in the completion). Please reduce the length of the messages.","type":"invalid_request_error","param":null,"code":"invalid_request_error"}}`, true},
		{"anthropic prompt is too long", 400,
			`{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 213432 tokens > 200000 maximum"}}`, true},
		{"zhipu-style context length", 400,
			`{"error":{"code":"1308","message":"prompt tokens exceed the model context length limit"}}`, true},
		{"too many tokens", 400,
			`{"error":{"message":"Request contains too many tokens: 300000"}}`, true},
		{"case-insensitive", 400,
			`{"error":{"code":"CONTEXT_LENGTH_EXCEEDED"}}`, true},
		{"invalid api key", 401,
			`{"error":{"message":"Incorrect API key provided","type":"invalid_request_error"}}`, false},
		{"missing model field", 400,
			`{"error":{"message":"Missing required field: model"}}`, false},
		{"model not found", 400,
			`{"error":{"message":"model 'foo' does not exist"}}`, false},
		{"2xx never an overflow", 200,
			`{"error":{"code":"context_length_exceeded"}}`, false},
		{"5xx never an overflow", 500,
			`context_length_exceeded`, false},
		{"empty body", 400, ``, false},
	}
	for _, c := range cases {
		if got := IsContextOverflow(c.status, []byte(c.body)); got != c.want {
			t.Errorf("%s: isContextOverflow(%d, body) = %v, want %v", c.name, c.status, got, c.want)
		}
	}
}

// TestStripTopLevelParam: unit semantics of the best-effort stripper.
func TestStripTopLevelParam(t *testing.T) {
	out, did := StripTopLevelParam([]byte(`{"model":"m","max_tokens":5,"messages":[]}`), "max_tokens")
	if !did {
		t.Fatal("expected did=true")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if _, ok := obj["max_tokens"]; ok {
		t.Error("max_tokens not stripped")
	}
	if _, ok := obj["model"]; !ok {
		t.Error("model must be preserved")
	}
	if _, did := StripTopLevelParam([]byte(`{"model":"m"}`), "max_tokens"); did {
		t.Error("absent key: did should be false")
	}
	if _, did := StripTopLevelParam([]byte(`not-json`), "max_tokens"); did {
		t.Error("non-JSON: did should be false")
	}
}
