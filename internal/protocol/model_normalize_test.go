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
