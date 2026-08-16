package protocol

import (
	"strings"
	"testing"
)

func TestAggregateAndSynthesizeAllProtocolStreams(t *testing.T) {
	cases := []struct {
		proto string
		json  string
		want  string
	}{
		{"openai", `{"id":"c1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`, `"content":"hello"`},
		{"anthropic", `{"id":"m1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`, `"text":"hello"`},
		{"responses", `{"id":"r1","object":"response","status":"completed","model":"m","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}]}`, `"text":"hello"`},
	}
	for _, tc := range cases {
		t.Run(tc.proto, func(t *testing.T) {
			stream, err := responseToSSE([]byte(tc.json), tc.proto)
			if err != nil {
				t.Fatal(err)
			}
			aggregated, err := aggregateSSEToResponse(stream, tc.proto)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(aggregated), tc.want) {
				t.Fatalf("round trip missing %s: %s", tc.want, aggregated)
			}
		})
	}
}

// requestWantsStream is on the per-request hot path and reads ONLY the
// top-level "stream" key: a literal boolean true counts, coercible lookalikes
// (string/number) and nested occurrences do not.
func TestRequestWantsStreamTopLevelBoolOnly(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"true", `{"model":"m","stream":true,"messages":[]}`, true},
		{"true first key", `{"stream":true}`, true},
		{"true spaced", `{"stream": true}`, true},
		{"false", `{"model":"m","stream":false}`, false},
		{"absent", `{"model":"m"}`, false},
		{"string true", `{"stream":"true"}`, false},
		{"number one", `{"stream":1}`, false},
		{"nested ignored", `{"messages":[{"content":"{\"stream\":true}"}]}`, false},
		{"nested object ignored", `{"a":{"stream":true},"stream":false}`, false},
		{"malformed", `not-json`, false},
		{"empty object", `{}`, false},
		{"array", `[1,2]`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := requestWantsStream([]byte(tc.body)); got != tc.want {
				t.Fatalf("requestWantsStream(%s) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}
