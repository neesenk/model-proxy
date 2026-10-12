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
	base := Key(request("/v1/responses", "", nil), body, "")
	if got := Key(request("/v1/responses", "", nil), body, ""); got != base {
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
			if got := Key(test.request, test.body, ""); got == base {
				t.Errorf("response-affecting %s produced base key %q", test.name, got)
			}
		})
	}
	if got := Key(request("/v1/responses", "", map[string]string{"x-custom": "ignored"}), body, ""); got != base {
		t.Errorf("non-response-affecting header changed key: got %q, want %q", got, base)
	}
}

// TestKeyEncodingIsStable pins the exact key encoding (length-prefixed
// method/path/query/key-headers/body) with a golden value: a silent change to
// the hash layout would invalidate every cached entry across upgrades.
func TestKeyEncodingIsStable(t *testing.T) {
	request, err := http.NewRequest(http.MethodPost, "http://example.invalid/v1/responses?beta=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("anthropic-beta", "output-128k")
	got := Key(request, []byte(`{"model":"glm","stream":false}`), "")
	const want = "8706438aff82a079d4a41b968dcbb5438cbaa520548a28a068d0396cfe1b880b"
	if got != want {
		t.Errorf("Key = %q, want %q", got, want)
	}
}

// TestKeySeparatesCallers pins per-caller bucketing: with api-keys enabled,
// an entry cached under one key id must never replay to another caller with
// an identical body. The empty caller (loopback default) keeps the legacy
// composition and equals the golden encoding above.
func TestKeySeparatesCallers(t *testing.T) {
	body := []byte(`{"model":"glm","input":[]}`)
	request, err := http.NewRequest(http.MethodPost, "http://example.invalid/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	base := Key(request, body, "")
	keyA := Key(request, body, "a1b2c3d4")
	keyB := Key(request, body, "e5f6a7b8")
	if keyA == base || keyB == base || keyA == keyB {
		t.Fatalf("caller bucketing collapsed: base=%s a=%s b=%s", base, keyA, keyB)
	}
	if Key(request, body, "a1b2c3d4") != keyA {
		t.Error("same caller must be stable across calls")
	}
}
