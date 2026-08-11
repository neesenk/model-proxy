package targetexec

import (
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
