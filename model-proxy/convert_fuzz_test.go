package main

// convert_fuzz_test.go — Go native fuzzing for the protocol converters.
// Seed corpus: inline f.Add plus testdata/fuzz/<Target>/ files. Run real
// fuzzing with:
//
//	go test -fuzz=FuzzConvertRequest -fuzztime=20s .
//	go test -fuzz=FuzzConvertSSE -fuzztime=20s .
//	go test -fuzz=FuzzParseToolArgs -fuzztime=15s .
//	go test -fuzz=FuzzSanitizeToolUseID -fuzztime=15s .
//
// Contracts under fuzz: no panics; bounded output (no unbounded loops);
// the pure helpers are deterministic; sanitizeToolUseID always satisfies
// anthropic's tool_use id charset.

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// requestSeedBodies covers valid bodies for all six request directions plus
// malformed/truncated shapes.
var requestSeedBodies = []string{
	`{"model":"c","max_tokens":100,"system":"s","messages":[{"role":"user","content":"hi"}],"stream":true}`,
	`{"model":"g","messages":[{"role":"system","content":"s"},{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c1","content":"r"}]}`,
	`{"model":"c","max_tokens":10,"thinking":{"type":"enabled","budget_tokens":8000},"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"t","signature":"sig"},{"type":"redacted_thinking","data":"d"}]}]}`,
	`{"model":"g","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"},{"type":"input_image","image_url":"data:image/png;base64,aGk="}]},{"type":"function_call","call_id":"c1","name":"f","arguments":"{\"q\":1}"},{"type":"reasoning","summary":[],"encrypted_content":"enc"}],"reasoning":{"effort":"high"},"text":{"format":{"type":"json_object"}}}`,
	`{"model":"g","messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"data:image/png;base64,aGk="}}]}],"response_format":{"type":"json_schema","json_schema":{"name":"S","schema":{"type":"object"}}}}`,
	`{"id":"resp_1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]},{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"}],"usage":{"input_tokens":3,"output_tokens":2}}`,
	`{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"hi","reasoning_content":"t"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`,
	`{"id":"msg_1","stop_reason":"refusal","content":[{"type":"thinking","thinking":"t","signature":"s"},{"type":"text","text":"x"}],"usage":{"input_tokens":1,"output_tokens":1}}`,
	`{`, `not json at all`, `[]`, `null`, `{"model":`, `"just a string"`, `{"input":"shortcut"}`,
	`{"messages":[{"role":"tool","content":"orphan"}]}`,
}

// FuzzConvertRequest feeds arbitrary bytes as request AND response bodies
// through all pairwise converters. Errors are fine; panics are not.
func FuzzConvertRequest(f *testing.F) {
	for _, s := range requestSeedBodies {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		// request direction
		convertAnthropicRequestToOpenAI(data)
		convertOpenAIRequestToAnthropic(data)
		convertAnthropicRequestToResponses(data)
		convertOpenAIRequestToResponses(data)
		convertResponsesRequestToAnthropic(data)
		convertResponsesRequestToOpenAI(data)
		// response direction
		convertOpenAIResponseToAnthropic(data)
		convertAnthropicResponseToOpenAI(data)
		convertResponsesToAnthropic(data)
		convertResponsesToOpenAI(data)
		convertAnthropicResponseToResponses(data)
		convertOpenAIResponseToResponses(data)
	})
}

// sseSeedStreams covers each source protocol's frame shapes.
var sseSeedStreams = []string{
	"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"usage\":{\"input_tokens\":3}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"t1\",\"name\":\"f\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{}\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"r\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"name\":\"f\",\"arguments\":\"{}\"}}]}}]}\n\ndata: [DONE]\n\n",
	"event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\"}}\n\nevent: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"hi\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n",
	"data: {broken\n\nevent: bogus_event\ndata: {\"type\":\"bogus_event\"}\n\ndata: [DONE]",
	"",
	"data: {\"error\":{\"message\":\"x\",\"type\":\"api_error\"}}\n\n",
}

// maxFuzzSSEOutput caps total converter output as a multiple of input size —
// a converter looping unboundedly blows past it immediately.
func maxFuzzSSEOutput(inputLen int) int { return 128*inputLen + 16384 }

// FuzzConvertSSE feeds arbitrary bytes as an SSE stream through every
// streaming transformer to EOF. No panics, bounded output. Seeds include
// every testdata/wire/*.sse ≤64KiB — real recorded upstream streams (via
// `model-proxy wire record`) automatically join the corpus.
func FuzzConvertSSE(f *testing.F) {
	for _, s := range sseSeedStreams {
		f.Add([]byte(s))
	}
	if entries, err := os.ReadDir("testdata/wire"); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".sse") {
				continue
			}
			if info, err := e.Info(); err != nil || info.Size() > 64*1024 {
				continue
			}
			if data, err := os.ReadFile(filepath.Join("testdata/wire", e.Name())); err == nil {
				f.Add(data)
			}
		}
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		outCap := maxFuzzSSEOutput(len(data))
		readers := map[string]io.Reader{
			"o→a": newOpenAIToAnthropicSSE(strings.NewReader(string(data)), "m"),
			"a→o": newAnthropicToOpenAISSE(strings.NewReader(string(data)), "m"),
			"r→a": newResponsesToAnthropicSSE(strings.NewReader(string(data)), "m"),
			"r→o": newResponsesToOpenAISSE(strings.NewReader(string(data)), "m"),
			"a→r": newAnthropicToResponsesSSE(strings.NewReader(string(data)), "m"),
			"o→r": newOpenAIToResponsesSSE(strings.NewReader(string(data)), "m"),
		}
		for name, r := range readers {
			out, err := io.ReadAll(io.LimitReader(r, int64(outCap)+1))
			if err != nil {
				t.Fatalf("%s: read error: %v", name, err)
			}
			if len(out) > outCap {
				t.Fatalf("%s: output %d bytes exceeds bound %d for %d input bytes (unbounded loop?)", name, len(out), outCap, len(data))
			}
		}
	})
}

// FuzzParseToolArgs: never panics, deterministic.
func FuzzParseToolArgs(f *testing.F) {
	f.Add(`{"q":"x"}`)
	f.Add(`{"nested":{"a":[1,2,{"b":null}]}}`)
	f.Add(`not json{`)
	f.Add(`  `)
	f.Add(`[1,2,3]`)
	f.Add(`"string"`)
	f.Fuzz(func(t *testing.T, data string) {
		a := parseToolArgs(data)
		b := parseToolArgs(data)
		if strOf(a) != strOf(b) {
			t.Fatalf("parseToolArgs(%q) not deterministic: %v vs %v", data, a, b)
		}
	})
}

var toolUseIDCharset = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// FuzzSanitizeToolUseID: never panics, deterministic, output always in the
// anthropic tool_use id charset.
func FuzzSanitizeToolUseID(f *testing.F) {
	f.Add("call_abc-DEF_123")
	f.Add("functions.Bash:0")
	f.Add("a b/c.d:e")
	f.Add("")
	f.Add("日本語-id")
	f.Fuzz(func(t *testing.T, data string) {
		a := sanitizeToolUseID(data)
		if !toolUseIDCharset.MatchString(a) {
			t.Fatalf("sanitizeToolUseID(%q) = %q, outside ^[a-zA-Z0-9_-]+$", data, a)
		}
		// Determinism: empty ids get a unique counter placeholder BY DESIGN
		// (emptyToolIDCounter) — only non-empty ids are pure functions.
		if data != "" {
			if b := sanitizeToolUseID(data); b != a {
				t.Fatalf("sanitizeToolUseID(%q) not deterministic: %q vs %q", data, a, b)
			}
		}
	})
}
