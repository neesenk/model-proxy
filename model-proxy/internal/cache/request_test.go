package cache

import (
	"net/http"
	"testing"
)

func TestKeySeparatesResponseAffectingInputs(t *testing.T) {
	body := []byte(`{"model":"glm","input":[]}`)
	request := func(path, query string, headers map[string]string) *http.Request {
		url := "http://example.invalid" + path
		if query != "" {
			url += "?" + query
		}
		result, err := http.NewRequest(http.MethodPost, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		for name, value := range headers {
			result.Header.Set(name, value)
		}
		return result
	}
	base := Key(request("/v1/responses", "", nil), body)
	if got := Key(request("/v1/responses", "", nil), body); got != base {
		t.Errorf("identical request key = %q, want %q", got, base)
	}
	cases := []struct {
		name    string
		request *http.Request
		body    []byte
	}{
		{"body", request("/v1/responses", "", nil), []byte(`{"model":"other"}`)},
		{"path", request("/v1/messages", "", nil), body},
		{"query", request("/v1/responses", "version=2", nil), body},
		{"anthropic beta", request("/v1/responses", "", map[string]string{"anthropic-beta": "output-128k"}), body},
		{"language", request("/v1/responses", "", map[string]string{"accept-language": "zh-CN"}), body},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := Key(test.request, test.body); got == base {
				t.Errorf("response-affecting %s produced base key %q", test.name, got)
			}
		})
	}
	if got := Key(request("/v1/responses", "", map[string]string{"x-custom": "ignored"}), body); got != base {
		t.Errorf("non-response-affecting header changed key: got %q, want %q", got, base)
	}
}
