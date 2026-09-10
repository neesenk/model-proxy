package targetexec

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	responsecache "model-proxy/internal/cache"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/protocol"
)

const (
	aliasTargetModel = "k3"
	aliasCalledModel = "kimi-k3"
)

func modelNormalizeAttempt(clientProto, backendProto protocol.Protocol, targetModel, calledModel, reqBody string) (Attempt, *httptest.ResponseRecorder) {
	writer := httptest.NewRecorder()
	path := "/v1/chat/completions"
	switch clientProto {
	case protocol.Anthropic:
		path = "/v1/messages"
	case protocol.Responses:
		path = "/v1/responses"
	}
	request := httptest.NewRequest(http.MethodPost, "http://proxy.test"+path, nil)
	plan := NewPlan(PlanInput{
		Target: configdomain.RouteTarget{Provider: "upstream", Model: targetModel},
		ProviderConfig: configdomain.Provider{
			Provider:         "test",
			OpenAIBaseURL:    "https://upstream.test",
			AnthropicBaseURL: "https://upstream-anthropic.test",
		},
		Provider:        &executorTestProvider{},
		ClientProtocol:  clientProto,
		BackendProtocol: backendProto,
		ClientPath:      path,
	})
	return NewAttempt(
		Runtime{},
		plan,
		Exchange{Request: request, Writer: writer, Body: []byte(reqBody)},
		Scope{CalledModel: calledModel},
		Policy{LastTarget: true},
	), writer
}

func sseUpstream(body string) *http.Response {
	response := testResponse(http.StatusOK, body)
	response.Header.Set("content-type", "text/event-stream")
	return response
}

const aliasChatCompletion = `{"id":"chatcmpl-1","object":"chat.completion","model":"k3","created":1720000000,` +
	`"choices":[{"index":0,"message":{"role":"assistant","content":"echo \"model\":\"k3\" in text"},"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`

const aliasAnthropicMessage = `{"id":"msg_1","type":"message","role":"assistant","model":"k3",` +
	`"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":2}}`

const aliasAnthropicSSE = "event: message_start\n" +
	"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"k3\",\"content\":[],\"usage\":{\"input_tokens\":5}}}\n\n" +
	"event: content_block_start\n" +
	"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
	"event: content_block_delta\n" +
	"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
	"event: content_block_stop\n" +
	"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
	"event: message_delta\n" +
	"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n" +
	"event: message_stop\n" +
	"data: {\"type\":\"message_stop\"}\n\n"

const aliasChatSSE = "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"k3\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"k3\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"k3\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"k3\",\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1,\"total_tokens\":4}}\n\n" +
	"data: [DONE]\n\n"

// ssePayloads parses the data payloads of every SSE frame in raw.
func ssePayloads(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, frame := range strings.Split(raw, "\n\n") {
		for _, line := range strings.Split(frame, "\n") {
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "" || data == "[DONE]" {
				continue
			}
			var payload map[string]any
			if err := json.Unmarshal([]byte(data), &payload); err != nil {
				t.Fatalf("frame payload does not parse: %v (%q)", err, data)
			}
			out = append(out, payload)
		}
	}
	return out
}

func runAliasAttempt(t *testing.T, attempt Attempt, upstream *http.Response) *httptest.ResponseRecorder {
	t.Helper()
	writer := attempt.Exchange().Writer.(*httptest.ResponseRecorder)
	result := (Executor{
		Client: &sequenceDoer{responses: []*http.Response{upstream}},
		State:  &executorState{},
	}).Execute(attempt)
	if !result.Committed {
		t.Fatalf("result = %+v, want committed (client status %d: %s)", result, writer.Code, writer.Body.String())
	}
	if writer.Code != http.StatusOK {
		t.Fatalf("client status = %d: %s", writer.Code, writer.Body.String())
	}
	return writer
}

func TestExecutorNormalizesPassthroughBufferedModel(t *testing.T) {
	t.Run("openai", func(t *testing.T) {
		attempt, _ := modelNormalizeAttempt(protocol.OpenAI, protocol.OpenAI, aliasTargetModel, aliasCalledModel, `{"model":"kimi-k3"}`)
		writer := runAliasAttempt(t, attempt, testResponse(http.StatusOK, aliasChatCompletion))
		var body map[string]any
		if err := json.Unmarshal(writer.Body.Bytes(), &body); err != nil {
			t.Fatalf("client body does not parse: %v (%s)", err, writer.Body.String())
		}
		if body["model"] != aliasCalledModel {
			t.Fatalf("client model = %v, want %s (%s)", body["model"], aliasCalledModel, writer.Body.String())
		}
		if body["id"] != "chatcmpl-1" {
			t.Fatalf("unrelated field lost: %s", writer.Body.String())
		}
		// The model literal inside generated content must survive untouched.
		if !strings.Contains(writer.Body.String(), `echo \"model\":\"k3\" in text`) {
			t.Fatalf("nested model literal corrupted: %s", writer.Body.String())
		}
	})
	t.Run("anthropic", func(t *testing.T) {
		attempt, _ := modelNormalizeAttempt(protocol.Anthropic, protocol.Anthropic, aliasTargetModel, aliasCalledModel, `{"model":"kimi-k3"}`)
		writer := runAliasAttempt(t, attempt, testResponse(http.StatusOK, aliasAnthropicMessage))
		var body map[string]any
		if err := json.Unmarshal(writer.Body.Bytes(), &body); err != nil {
			t.Fatalf("client body does not parse: %v (%s)", err, writer.Body.String())
		}
		if body["model"] != aliasCalledModel {
			t.Fatalf("client model = %v, want %s (%s)", body["model"], aliasCalledModel, writer.Body.String())
		}
	})
	t.Run("responses", func(t *testing.T) {
		upstream := `{"id":"resp_1","object":"response","model":"k3","status":"completed","created_at":1720000000,` +
			`"output":[{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hi","annotations":[]}]}],` +
			`"usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4}}`
		attempt, _ := modelNormalizeAttempt(protocol.Responses, protocol.Responses, aliasTargetModel, aliasCalledModel, `{"model":"kimi-k3"}`)
		writer := runAliasAttempt(t, attempt, testResponse(http.StatusOK, upstream))
		var body map[string]any
		if err := json.Unmarshal(writer.Body.Bytes(), &body); err != nil {
			t.Fatalf("client body does not parse: %v (%s)", err, writer.Body.String())
		}
		if body["model"] != aliasCalledModel {
			t.Fatalf("client model = %v, want %s (%s)", body["model"], aliasCalledModel, writer.Body.String())
		}
	})
}

func TestExecutorNormalizesPassthroughSSEModel(t *testing.T) {
	t.Run("openai chunks", func(t *testing.T) {
		attempt, _ := modelNormalizeAttempt(protocol.OpenAI, protocol.OpenAI, aliasTargetModel, aliasCalledModel, `{"model":"kimi-k3","stream":true}`)
		writer := runAliasAttempt(t, attempt, sseUpstream(aliasChatSSE))
		payloads := ssePayloads(t, writer.Body.String())
		if len(payloads) != 4 {
			t.Fatalf("chunks = %d, want 4: %s", len(payloads), writer.Body.String())
		}
		for _, payload := range payloads {
			if payload["model"] != aliasCalledModel {
				t.Fatalf("chunk model = %v, want %s (%s)", payload["model"], aliasCalledModel, writer.Body.String())
			}
		}
		if !strings.HasSuffix(writer.Body.String(), "data: [DONE]\n\n") {
			t.Fatalf("[DONE] terminator damaged: %s", writer.Body.String())
		}
	})
	t.Run("anthropic message_start nested model", func(t *testing.T) {
		attempt, _ := modelNormalizeAttempt(protocol.Anthropic, protocol.Anthropic, aliasTargetModel, aliasCalledModel, `{"model":"kimi-k3","stream":true}`)
		writer := runAliasAttempt(t, attempt, sseUpstream(aliasAnthropicSSE))
		payloads := ssePayloads(t, writer.Body.String())
		if len(payloads) == 0 || payloads[0]["type"] != "message_start" {
			t.Fatalf("first frame is not message_start: %s", writer.Body.String())
		}
		message, _ := payloads[0]["message"].(map[string]any)
		if message["model"] != aliasCalledModel {
			t.Fatalf("message_start model = %v, want %s (%s)", message["model"], aliasCalledModel, writer.Body.String())
		}
		// The text delta must be byte-preserved (raw fast-forward after
		// message_start).
		if !strings.Contains(writer.Body.String(), `"delta":{"type":"text_delta","text":"hi"}`) {
			t.Fatalf("delta frame damaged: %s", writer.Body.String())
		}
	})
}

func TestExecutorNormalizesConvertedResponseModel(t *testing.T) {
	t.Run("openai to anthropic buffered", func(t *testing.T) {
		attempt, _ := modelNormalizeAttempt(protocol.Anthropic, protocol.OpenAI, aliasTargetModel, aliasCalledModel, `{"model":"kimi-k3"}`)
		writer := runAliasAttempt(t, attempt, testResponse(http.StatusOK, aliasChatCompletion))
		var body map[string]any
		if err := json.Unmarshal(writer.Body.Bytes(), &body); err != nil {
			t.Fatalf("client body does not parse: %v (%s)", err, writer.Body.String())
		}
		if body["model"] != aliasCalledModel {
			t.Fatalf("converted model = %v, want %s (%s)", body["model"], aliasCalledModel, writer.Body.String())
		}
		if body["type"] != "message" {
			t.Fatalf("not an anthropic message: %s", writer.Body.String())
		}
	})
	t.Run("anthropic to openai buffered", func(t *testing.T) {
		attempt, _ := modelNormalizeAttempt(protocol.OpenAI, protocol.Anthropic, aliasTargetModel, aliasCalledModel, `{"model":"kimi-k3"}`)
		writer := runAliasAttempt(t, attempt, testResponse(http.StatusOK, aliasAnthropicMessage))
		var body map[string]any
		if err := json.Unmarshal(writer.Body.Bytes(), &body); err != nil {
			t.Fatalf("client body does not parse: %v (%s)", err, writer.Body.String())
		}
		if body["model"] != aliasCalledModel {
			t.Fatalf("converted model = %v, want %s (%s)", body["model"], aliasCalledModel, writer.Body.String())
		}
	})
	t.Run("anthropic to openai SSE", func(t *testing.T) {
		attempt, _ := modelNormalizeAttempt(protocol.OpenAI, protocol.Anthropic, aliasTargetModel, aliasCalledModel, `{"model":"kimi-k3","stream":true}`)
		writer := runAliasAttempt(t, attempt, sseUpstream(aliasAnthropicSSE))
		payloads := ssePayloads(t, writer.Body.String())
		if len(payloads) == 0 {
			t.Fatalf("no chunks: %s", writer.Body.String())
		}
		for _, payload := range payloads {
			if payload["model"] != aliasCalledModel {
				t.Fatalf("converted chunk model = %v, want %s (%s)", payload["model"], aliasCalledModel, writer.Body.String())
			}
		}
	})
}

func TestExecutorNormalizesModeMismatchResponseModel(t *testing.T) {
	t.Run("stream client over buffered upstream synthesizes normalized SSE", func(t *testing.T) {
		attempt, _ := modelNormalizeAttempt(protocol.Anthropic, protocol.OpenAI, aliasTargetModel, aliasCalledModel, `{"model":"kimi-k3","stream":true}`)
		writer := runAliasAttempt(t, attempt, testResponse(http.StatusOK, aliasChatCompletion))
		payloads := ssePayloads(t, writer.Body.String())
		if len(payloads) == 0 || payloads[0]["type"] != "message_start" {
			t.Fatalf("first frame is not message_start: %s", writer.Body.String())
		}
		message, _ := payloads[0]["message"].(map[string]any)
		if message["model"] != aliasCalledModel {
			t.Fatalf("message_start model = %v, want %s (%s)", message["model"], aliasCalledModel, writer.Body.String())
		}
	})
	t.Run("buffered client over SSE upstream aggregates normalized JSON", func(t *testing.T) {
		attempt, _ := modelNormalizeAttempt(protocol.OpenAI, protocol.Anthropic, aliasTargetModel, aliasCalledModel, `{"model":"kimi-k3"}`)
		writer := runAliasAttempt(t, attempt, sseUpstream(aliasAnthropicSSE))
		var body map[string]any
		if err := json.Unmarshal(writer.Body.Bytes(), &body); err != nil {
			t.Fatalf("client body does not parse: %v (%s)", err, writer.Body.String())
		}
		if body["model"] != aliasCalledModel {
			t.Fatalf("aggregated model = %v, want %s (%s)", body["model"], aliasCalledModel, writer.Body.String())
		}
	})
}

// TestExecutorSameModelKeepsByteTransparentPassthrough pins the zero-copy
// contract: when the target model equals the called model the response bytes
// reach the client untouched (no parsing, no re-encoding).
func TestExecutorSameModelKeepsByteTransparentPassthrough(t *testing.T) {
	t.Run("buffered", func(t *testing.T) {
		upstream := "{  \"id\":\"chatcmpl-1\",\n \"model\" : \"k3\", \"choices\": [ ] }"
		attempt, _ := modelNormalizeAttempt(protocol.OpenAI, protocol.OpenAI, aliasTargetModel, aliasTargetModel, `{"model":"k3"}`)
		writer := runAliasAttempt(t, attempt, testResponse(http.StatusOK, upstream))
		if writer.Body.String() != upstream {
			t.Fatalf("bytes changed:\nupstream: %q\nclient:   %q", upstream, writer.Body.String())
		}
	})
	t.Run("sse", func(t *testing.T) {
		attempt, _ := modelNormalizeAttempt(protocol.OpenAI, protocol.OpenAI, aliasTargetModel, aliasTargetModel, `{"model":"k3","stream":true}`)
		writer := runAliasAttempt(t, attempt, sseUpstream(aliasChatSSE))
		if writer.Body.String() != aliasChatSSE {
			t.Fatalf("stream bytes changed:\nupstream: %q\nclient:   %q", aliasChatSSE, writer.Body.String())
		}
	})
}

// TestExecutorCachesNormalizedModel: the cache recorder sits downstream of
// normalization, so a cache hit replays the same called-model bytes without
// any upstream call.
func TestExecutorCachesNormalizedModel(t *testing.T) {
	attempt, _ := modelNormalizeAttempt(protocol.OpenAI, protocol.OpenAI, aliasTargetModel, aliasCalledModel, `{"model":"kimi-k3"}`)
	cache := responsecache.New(responsecache.Options{TTL: time.Minute, MaxEntries: 4, MaxBodyBytes: 1 << 20})
	runtime := attempt.Runtime()
	runtime.Cache = cache
	scope := attempt.Scope()
	scope.CacheKey = "cache-key"
	attempt = NewAttempt(runtime, attempt.Plan(), attempt.Exchange(), scope, attempt.Policy())
	writer := runAliasAttempt(t, attempt, testResponse(http.StatusOK, aliasChatCompletion))
	if writer.Body.String() == "" {
		t.Fatal("empty client body")
	}
	entry, ok := cache.Lookup("cache-key", "m", time.Now())
	if !ok {
		t.Fatal("response not cached")
	}
	replay := httptest.NewRecorder()
	if err := responsecache.Replay(replay, entry); err != nil {
		t.Fatalf("replay: %v", err)
	}
	var stored map[string]any
	if err := json.Unmarshal(replay.Body.Bytes(), &stored); err != nil {
		t.Fatalf("stored body does not parse: %v", err)
	}
	if stored["model"] != aliasCalledModel {
		t.Fatalf("cached model = %v, want %s", stored["model"], aliasCalledModel)
	}
}
