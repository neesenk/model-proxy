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
