package providerbuild

import (
	"testing"

	configdomain "model-proxy/internal/config"
)

func TestProtocolConfigFingerprint(t *testing.T) {
	base := configdomain.Provider{
		Provider:         "aqp",
		OpenAIBaseURL:    "https://o",
		AnthropicBaseURL: "https://a",
		Headers:          map[string]string{"x-one": "1", "x-two": "2"},
	}
	fp := ProtocolConfigFingerprint(base)
	if len(fp) != 16 {
		t.Fatalf("fingerprint = %q, want 16 hex chars", fp)
	}

	// Header map order must not matter.
	reordered := configdomain.Provider{
		Provider:         "aqp",
		OpenAIBaseURL:    "https://o",
		AnthropicBaseURL: "https://a",
		Headers:          map[string]string{"x-two": "2", "x-one": "1"},
	}
	if ProtocolConfigFingerprint(reordered) != fp {
		t.Error("header insertion order changed the fingerprint")
	}

	// Every protocol-relevant field is fingerprint-sensitive.
	variants := map[string]configdomain.Provider{
		"provider id":    {Provider: "other", OpenAIBaseURL: "https://o", AnthropicBaseURL: "https://a"},
		"openai base":    {Provider: "aqp", OpenAIBaseURL: "https://o2", AnthropicBaseURL: "https://a"},
		"anthropic base": {Provider: "aqp", OpenAIBaseURL: "https://o", AnthropicBaseURL: ""},
		"header value":   {Provider: "aqp", OpenAIBaseURL: "https://o", AnthropicBaseURL: "https://a", Headers: map[string]string{"x-one": "9"}},
		"no headers":     {Provider: "aqp", OpenAIBaseURL: "https://o", AnthropicBaseURL: "https://a"},
	}
	for name, v := range variants {
		if ProtocolConfigFingerprint(v) == fp {
			t.Errorf("%s variant kept the same fingerprint", name)
		}
	}
}
