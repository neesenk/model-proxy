package protocol

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func topLevelModel(t *testing.T, body []byte) string {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, body)
	}
	model, _ := value["model"].(string)
	return model
}

func TestNormalizeResponseModel(t *testing.T) {
	t.Run("openai model not first key, bytes around value preserved", func(t *testing.T) {
		body := []byte(`{"id":"chatcmpl-1", "object":"chat.completion", "model" : "k3" , "choices":[{"index":0,"message":{"role":"assistant","content":"echo \"model\":\"k3\" inside text"}}]}`)
		out := NormalizeResponseModel(body, OpenAI, "kimi-k3")
		if got := topLevelModel(t, out); got != "kimi-k3" {
			t.Fatalf("model = %q, want kimi-k3 (%s)", got, out)
		}
		// Structural splice: the nested literal inside generated content must
		// survive byte-identical, and only the top-level value changed.
		if !strings.Contains(string(out), `echo \"model\":\"k3\" inside text`) {
			t.Fatalf("nested model literal corrupted: %s", out)
		}
		if !strings.Contains(string(out), `"model" : "kimi-k3" ,`) {
			t.Fatalf("splice did not preserve surrounding whitespace: %s", out)
		}
	})
	t.Run("anthropic top-level model", func(t *testing.T) {
		body := []byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-x","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":3,"output_tokens":1}}`)
		out := NormalizeResponseModel(body, Anthropic, "claude-alias")
		if got := topLevelModel(t, out); got != "claude-alias" {
			t.Fatalf("model = %q (%s)", got, out)
		}
	})
	t.Run("responses top-level model", func(t *testing.T) {
		body := []byte(`{"id":"resp_1","object":"response","model":"gpt-x","created_at":1720000000,"output":[]}`)
		out := NormalizeResponseModel(body, Responses, "gpt-alias")
		if got := topLevelModel(t, out); got != "gpt-alias" {
			t.Fatalf("model = %q (%s)", got, out)
		}
	})
	t.Run("missing model key passes through unchanged (no fabrication)", func(t *testing.T) {
		body := []byte(`{"id":"chatcmpl-1","choices":[]}`)
		out := NormalizeResponseModel(body, OpenAI, "kimi-k3")
		if string(out) != string(body) {
			t.Fatalf("out = %s, want unchanged %s", out, body)
		}
	})
	t.Run("empty model is a no-op", func(t *testing.T) {
		body := []byte(`{"model":"k3"}`)
		out := NormalizeResponseModel(body, OpenAI, "")
		if string(out) != string(body) {
			t.Fatalf("out=%s, want unchanged", out)
		}
	})
	t.Run("unknown protocol is a no-op", func(t *testing.T) {
		body := []byte(`{"model":"k3"}`)
		out := NormalizeResponseModel(body, Protocol("mystery"), "kimi-k3")
		if string(out) != string(body) {
			t.Fatalf("out=%s, want unchanged", out)
		}
	})
	t.Run("non-object and malformed bodies pass through unchanged", func(t *testing.T) {
		for _, raw := range []string{`["not","an","object"]`, `{broken`, `"just a string"`, `{"model":123}`} {
			if out := NormalizeResponseModel([]byte(raw), OpenAI, "kimi-k3"); string(out) != raw {
				t.Fatalf("out = %s, want unchanged %s", out, raw)
			}
		}
	})
}

func readAllStream(t *testing.T, source io.ReadCloser, proto Protocol, model string) string {
	t.Helper()
	out, err := io.ReadAll(NormalizeSSEModelStream(source, proto, model))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(out)
}

func TestNormalizeSSEModelStreamOpenAI(t *testing.T) {
	raw := "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"k3\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"k3\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"literal \\\"model\\\":\\\"k3\\\" in text\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"k3\",\"choices\":[],\"usage\":{\"prompt_tokens\":1}}\n\n" +
		"data: [DONE]\n\n"
	out := readAllStream(t, io.NopCloser(strings.NewReader(raw)), OpenAI, "kimi-k3")
	if strings.Contains(out, `"model":"k3","choices"`) {
		t.Fatalf("upstream model leaked: %s", out)
	}
	if got := strings.Count(out, `"model":"kimi-k3"`); got != 3 {
		t.Fatalf("normalized chunks = %d, want 3: %s", got, out)
	}
	if !strings.Contains(out, "literal \\\"model\\\":\\\"k3\\\" in text") {
		t.Fatalf("nested model literal corrupted: %s", out)
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Fatalf("[DONE] terminator damaged: %q", out)
	}
}

func TestNormalizeSSEModelStreamAnthropic(t *testing.T) {
	raw := "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"k3\",\"usage\":{\"input_tokens\":5}}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"mentions \\\"model\\\":\\\"k3\\\" verbatim\"}}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"message\":{\"model\":\"k3\"}}\n\n" +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	out := readAllStream(t, io.NopCloser(strings.NewReader(raw)), Anthropic, "kimi-k3")
	if !strings.Contains(out, `"message":{"id":"msg_1","model":"kimi-k3"`) {
		t.Fatalf("message_start model not normalized: %s", out)
	}
	if !strings.Contains(out, "event: message_start\n") {
		t.Fatalf("event line lost: %s", out)
	}
	// Generated content mentioning a model literal is untouched.
	if !strings.Contains(out, "mentions \\\"model\\\":\\\"k3\\\" verbatim") {
		t.Fatalf("delta content corrupted: %s", out)
	}
	// After message_start the stream fast-forwards raw: even a (pathological)
	// later frame with a nested message.model must pass through unchanged.
	if !strings.Contains(out, `"message_delta","delta":{"stop_reason":"end_turn"},"message":{"model":"k3"}`) {
		t.Fatalf("post-message_start frame was rescanned: %s", out)
	}
}

func TestNormalizeSSEModelStreamResponses(t *testing.T) {
	raw := "event: response.created\n" +
		"data: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\",\"model\":\"gpt-x\",\"status\":\"in_progress\"}}\n\n" +
		"event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\",\"item_id\":\"m1\"}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"model\":\"gpt-x\",\"status\":\"completed\",\"output\":[]}}\n\n"
	out := readAllStream(t, io.NopCloser(strings.NewReader(raw)), Responses, "gpt-alias")
	if got := strings.Count(out, `"model":"gpt-alias"`); got != 2 {
		t.Fatalf("normalized snapshots = %d, want 2: %s", got, out)
	}
	if !strings.Contains(out, `"delta":"hi","item_id":"m1"`) {
		t.Fatalf("delta frame damaged: %s", out)
	}
}

func TestNormalizeSSEModelStreamPassthroughCases(t *testing.T) {
	t.Run("malformed frame passes through unchanged", func(t *testing.T) {
		raw := "data: {broken json\n\ndata: {\"model\":\"k3\"}\n\n"
		out := readAllStream(t, io.NopCloser(strings.NewReader(raw)), OpenAI, "kimi-k3")
		if !strings.HasPrefix(out, "data: {broken json\n\n") {
			t.Fatalf("malformed frame damaged: %q", out)
		}
		if !strings.Contains(out, `"model":"kimi-k3"`) {
			t.Fatalf("valid frame after malformed one not normalized: %q", out)
		}
	})
	t.Run("heartbeat and event-only frames unchanged", func(t *testing.T) {
		raw := ": ping\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
		out := readAllStream(t, io.NopCloser(strings.NewReader(raw)), Anthropic, "kimi-k3")
		if out != raw {
			t.Fatalf("out = %q, want byte-identical %q", out, raw)
		}
	})
	t.Run("final frame without trailing blank line", func(t *testing.T) {
		raw := "data: {\"model\":\"k3\"}\n\n" + "data: [DONE]"
		out := readAllStream(t, io.NopCloser(strings.NewReader(raw)), OpenAI, "kimi-k3")
		if !strings.Contains(out, `"model":"kimi-k3"`) || !strings.HasSuffix(out, "data: [DONE]") {
			t.Fatalf("out = %q", out)
		}
	})
	t.Run("empty model returns source untouched", func(t *testing.T) {
		source := io.NopCloser(strings.NewReader("data: {\"model\":\"k3\"}\n\n"))
		if got := NormalizeSSEModelStream(source, OpenAI, ""); got != source {
			t.Fatal("empty model must return the source reader")
		}
	})
}

func TestSpliceTopLevelStringValue(t *testing.T) {
	body := []byte(`{"a":1,"model":"old","b":{"model":"inner"}}`)
	out, ok := spliceTopLevelStringValue(body, "model", "new")
	if !ok {
		t.Fatal("splice failed")
	}
	if string(out) != `{"a":1,"model":"new","b":{"model":"inner"}}` {
		t.Fatalf("out = %s", out)
	}
	if _, ok := spliceTopLevelStringValue([]byte(`{"model":123}`), "model", "new"); ok {
		t.Fatal("non-string model must not splice")
	}
	if _, ok := spliceTopLevelStringValue([]byte(`{"other":"x"}`), "model", "new"); ok {
		t.Fatal("missing key must not splice")
	}
}

// TestNormalizeSSEModelStreamFoldedDataLinesStayValidSSE: a JSON payload may
// span several data: lines (legal SSE — the lines fold with '\n'). The
// rewrite must keep every line a data: line; folding the rewritten payload
// into one data: line left its continuation lines without the field name, so
// the client saw a truncated (invalid) JSON payload.
func TestNormalizeSSEModelStreamFoldedDataLinesStayValidSSE(t *testing.T) {
	raw := "data: {\"id\":\"c1\",\n" + "data: \"model\":\"k3\",\"choices\":[]}\n\n"
	out := readAllStream(t, io.NopCloser(strings.NewReader(raw)), OpenAI, "kimi-k3")
	want := "data: {\"id\":\"c1\",\n" + "data: \"model\":\"kimi-k3\",\"choices\":[]}\n\n"
	if out != want {
		t.Fatalf("out = %q, want %q", out, want)
	}
}

// TestNormalizeResponseModelDecoderSemantics pins the splice's JSON semantics
// — those of a decoder walk of the top-level object, which a fast byte-level
// path must reproduce exactly: the FIRST member named exactly "model" (key
// escapes decoded, no case folding) whose value is a JSON string is spliced;
// a malformed member BEFORE it fails the splice, bytes after it are never
// examined, and members whose values are containers are walked over.
func TestNormalizeResponseModelDecoderSemantics(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"model after nested containers", `{"a":{"b":[1,{"c":"model"}]},"model":"k3","z":1}`, `{"a":{"b":[1,{"c":"model"}]},"model":"kimi","z":1}`},
		{"escaped key", `{"model":"k3"}`, `{"model":"kimi"}`},
		{"escaped value", `{"model":"k3"}`, `{"model":"kimi"}`},
		{"first of duplicate keys", `{"model":"k3","model":"k4"}`, `{"model":"kimi","model":"k4"}`},
		{"numbers and literals before", `{"n":-1.5e+3,"t":true,"f":false,"z":null,"model":"k3"}`, `{"n":-1.5e+3,"t":true,"f":false,"z":null,"model":"kimi"}`},
		{"garbage after the value is not examined", `{"model":"k3",}`, `{"model":"kimi",}`},
		{"case-different key is not the model", `{"Model":"k3"}`, `{"Model":"k3"}`},
		{"invalid number before", `{"created":01,"model":"k3"}`, `{"created":01,"model":"k3"}`},
		{"invalid literal before", `{"x":truex,"model":"k3"}`, `{"x":truex,"model":"k3"}`},
		{"raw control byte in an earlier value", "{\"x\":\"a\x01b\",\"model\":\"k3\"}", "{\"x\":\"a\x01b\",\"model\":\"k3\"}"},
		{"non-string model", `{"model":["k3"]}`, `{"model":["k3"]}`},
		{"leading whitespace", " \n{\"model\" :\t\"k3\"}", " \n{\"model\" :\t\"kimi\"}"},
	}
	for _, tc := range cases {
		if got := string(NormalizeResponseModel([]byte(tc.body), OpenAI, "kimi")); got != tc.want {
			t.Errorf("%s: NormalizeResponseModel(%q) = %q, want %q", tc.name, tc.body, got, tc.want)
		}
	}
}

// TestNormalizeSSEModelStreamAllocationsDoNotScaleWithFrames: the openai
// normalizer runs on every chunk of every aliased stream (thousands of chunks
// per long answer, tens of concurrent streams). Frame assembly, the rewrite
// and the model encoding must reuse per-stream buffers: doubling a stream
// past its buffer warm-up (1500 → 3000 chunks) adds no per-chunk allocation.
func TestNormalizeSSEModelStreamAllocationsDoNotScaleWithFrames(t *testing.T) {
	stream := func(chunks int) string {
		var b strings.Builder
		for i := 0; i < chunks; i++ {
			b.WriteString("data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"up-1017\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"tok\"},\"finish_reason\":null}]}\n\n")
		}
		b.WriteString("data: [DONE]\n\n")
		return b.String()
	}
	// A caller-owned copy buffer, and wrappers hiding ReaderFrom/WriterTo:
	// io.Discard's ReadFrom draws pooled buffers, and the race detector
	// randomly drops sync.Pool items — an allocation outside the normalizer.
	buf := make([]byte, 8<<10)
	allocs := func(raw string) float64 {
		return testing.AllocsPerRun(10, func() {
			normalized := NormalizeSSEModelStream(io.NopCloser(strings.NewReader(raw)), OpenAI, "alias")
			if _, err := io.CopyBuffer(struct{ io.Writer }{io.Discard}, struct{ io.Reader }{normalized}, buf); err != nil {
				t.Fatal(err)
			}
		})
	}
	long := stream(3000)
	out := readAllStream(t, io.NopCloser(strings.NewReader(long)), OpenAI, "alias")
	if got := strings.Count(out, `"model":"alias"`); got != 3000 {
		t.Fatalf("normalized chunks = %d, want 3000", got)
	}
	// The rate, not exact equality: the race detector's runtime allocates
	// sporadically (±1 per run); a per-chunk allocation shows as >= 1500.
	if half, full := allocs(stream(1500)), allocs(long); (full-half)/1500 >= 0.01 {
		t.Errorf("allocations: 3000 chunks = %v, 1500 chunks = %v; want no per-chunk allocation", full, half)
	}
}
