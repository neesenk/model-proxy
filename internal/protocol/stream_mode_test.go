package protocol

import (
	"encoding/json"
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

// TestAggregateChatSSEStringShapedError: the SSE→JSON aggregation bridge must
// treat a string-form error payload ({"error":"rate limited"}) as a terminal
// error like the object form — not aggregate a "successful" response around it.
func TestAggregateChatSSEStringShapedError(t *testing.T) {
	raw := "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: {\"error\":\"rate limited\"}\n\n"
	if _, err := aggregateSSEToResponse([]byte(raw), "openai"); err == nil {
		t.Fatal("string-form error chunk aggregated as a clean response, want an error")
	}
}

// The non-stream chat JSON → SSE bridge must emit spec-shaped chunks: every
// chat.completion.chunk carries the required created field (strict SDKs
// validate it per chunk), and usage rides its own empty-choices chunk before
// [DONE] — the same shape the streaming converters emit — instead of riding
// the finish_reason chunk.
func TestChatJSONToSSEChunkContract(t *testing.T) {
	body := `{"id":"c1","object":"chat.completion","created":1750000000,"model":"m",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`
	stream, err := responseToSSE([]byte(body), "openai")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(stream)), "\n\n")
	if len(lines) < 2 || lines[len(lines)-1] != "data: [DONE]" {
		t.Fatalf("stream does not end with [DONE]:\n%s", stream)
	}
	frames := lines[:len(lines)-1]
	var finishChunk, usageChunk map[string]any
	for _, ln := range frames {
		if !strings.HasPrefix(ln, "data: ") {
			t.Fatalf("unexpected non-data line %q", ln)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(ln, "data: ")), &m); err != nil {
			t.Fatalf("chunk %q is not JSON: %v", ln, err)
		}
		created, _ := m["created"].(float64)
		if created == 0 {
			t.Errorf("chunk without required created field: %s", ln)
		}
		if m["id"] != "c1" || m["object"] != "chat.completion.chunk" || m["model"] != "m" {
			t.Errorf("chunk missing identity fields: %s", ln)
		}
		choices, _ := m["choices"].([]any)
		if len(choices) == 0 {
			usageChunk = m
			continue
		}
		first, _ := choices[0].(map[string]any)
		if fr, ok := first["finish_reason"]; ok && fr != nil {
			finishChunk = m
			if _, hasUsage := m["usage"]; hasUsage {
				t.Errorf("finish chunk must not carry usage: %s", ln)
			}
		}
	}
	if finishChunk == nil {
		t.Fatal("no finish_reason chunk in the bridged stream")
	}
	if usageChunk == nil {
		t.Fatal("no empty-choices usage chunk before [DONE]")
	}
	usage, _ := usageChunk["usage"].(map[string]any)
	if usage == nil || usage["total_tokens"] != float64(7) {
		t.Errorf("usage chunk payload wrong: %v", usageChunk["usage"])
	}
}

// A chat response without a created field still bridges: every synthesized
// chunk falls back to the stream-start timestamp.
func TestChatJSONToSSEBackfillsCreated(t *testing.T) {
	body := `{"id":"c2","object":"chat.completion","model":"m",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}]}`
	stream, err := responseToSSE([]byte(body), "openai")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stream), `"created":`) {
		t.Fatalf("created was not backfilled:\n%s", stream)
	}
}
