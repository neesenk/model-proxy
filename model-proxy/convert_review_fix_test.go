package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConvertErrorResponse_AllDirectionsAndStatuses(t *testing.T) {
	protocols := []string{"anthropic", "openai", "responses"}
	statuses := []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound}
	for _, clientProto := range protocols {
		for _, targetProto := range protocols {
			if clientProto == targetProto {
				continue
			}
			for _, status := range statuses {
				t.Run(clientProto+"<-"+targetProto+"/"+http.StatusText(status), func(t *testing.T) {
					body := []byte(`{"error":{"message":"bad input","type":"invalid_request_error","code":"bad_request"},"request_id":"req_123"}`)
					if targetProto == "anthropic" {
						body = []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"bad input"},"request_id":"req_123"}`)
					}
					got, err := convertErrorResponse(body, clientProto, targetProto, status)
					if err != nil {
						t.Fatal(err)
					}
					var out map[string]any
					if err := json.Unmarshal(got, &out); err != nil {
						t.Fatal(err)
					}
					if out["request_id"] != "req_123" {
						t.Fatalf("request_id = %v, want req_123", out["request_id"])
					}
					errObj := asMap(out["error"])
					if errObj == nil || errObj["message"] != "bad input" {
						t.Fatalf("error envelope = %v", out)
					}
					if clientProto == "anthropic" && out["type"] != "error" {
						t.Fatalf("anthropic error type = %v", out["type"])
					}
					if _, success := out["choices"]; success {
						t.Fatalf("error was converted as chat success: %v", out)
					}
					if _, success := out["output"]; success {
						t.Fatalf("error was converted as responses success: %v", out)
					}
				})
			}
		}
	}
}

func TestForward_Converted4xxUsesClientErrorEnvelope(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"unsupported field","type":"invalid_request_error","code":"bad_request"},"request_id":"req_up"}`)
	}))
	defer up.Close()

	cfg := &Config{
		Providers: map[string]Provider{"oai": {OpenAIBaseURL: up.URL, Provider: "static"}},
		Routes:    map[string][]RouteTarget{"claude-x": {{Provider: "oai", Model: "gpt-x", Protocol: "openai"}}},
	}
	p := newTestProxy(t, cfg)
	p.providers["oai"] = &testProv{key: "k"}
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	resp, err := http.Post(px.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-x","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, body)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out["type"] != "error" || strOf(asMap(out["error"])["message"]) != "unsupported field" {
		t.Fatalf("body is not an Anthropic error envelope: %s", body)
	}
}

func TestConvertRequestFor_CodexShapesSameProtocolResponses(t *testing.T) {
	in := []byte(`{"model":"gpt-x","input":"hi","max_output_tokens":10,"temperature":0.2,"top_p":0.9,"store":true}`)
	got, err := convertRequestFor(in, "responses", "responses", convertReqOpts{ProviderID: "codex", ImageOK: true})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(got, &out); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"max_output_tokens", "temperature", "top_p"} {
		if _, ok := out[key]; ok {
			t.Errorf("codex same-protocol request retained %s: %s", key, got)
		}
	}
	if out["input"] != "hi" || out["store"] != true {
		t.Fatalf("unrelated fields changed: %s", got)
	}

	plain, err := convertRequestFor(in, "responses", "responses", convertReqOpts{ProviderID: "static", ImageOK: true})
	if err != nil || !bytes.Equal(plain, in) {
		t.Fatalf("non-codex same-protocol request must stay byte-identical: %s, %v", plain, err)
	}
}

func TestConvertOpenAIRequestToResponses_FoldsAllInstructions(t *testing.T) {
	in := []byte(`{"model":"gpt-x","messages":[` +
		`{"role":"system","content":""},` +
		`{"role":"user","content":"first"},` +
		`{"role":"developer","content":"rule one"},` +
		`{"role":"system","content":[{"type":"text","text":"rule two"}]},` +
		`{"role":"user","content":"second"}]}`)
	got, err := convertOpenAIRequestToResponses(in)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	json.Unmarshal(got, &out)
	if out["instructions"] != "rule one\n\nrule two" {
		t.Fatalf("instructions = %q", out["instructions"])
	}
	for _, item := range responsesInputItems(out["input"]) {
		if role := strOf(item["role"]); role == "system" || role == "developer" {
			t.Fatalf("instruction role leaked into input: %v", item)
		}
	}
}

func TestResponsesContentToAnthropicBlocks_PreservesURLImage(t *testing.T) {
	blocks := responsesContentToAnthropicBlocks([]any{
		map[string]any{"type": "input_image", "image_url": "https://example.test/image.png"},
	})
	if len(blocks) != 1 {
		t.Fatalf("blocks = %v", blocks)
	}
	source := asMap(blocks[0]["source"])
	if source["type"] != "url" || source["url"] != "https://example.test/image.png" {
		t.Fatalf("url image source = %v", source)
	}
}

func TestResponsesNonStream_StatusOnlyFailureFailsClosed(t *testing.T) {
	for _, status := range []string{"failed", "cancelled"} {
		body := []byte(`{"id":"resp_1","status":"` + status + `","output":[]}`)
		if _, err := convertResponsesToAnthropic(body); err == nil {
			t.Errorf("responses→anthropic status %s returned success", status)
		}
		if _, err := convertResponsesToOpenAI(body); err == nil {
			t.Errorf("responses→chat status %s returned success", status)
		}
	}
}

func TestStreamEOFWithoutTerminalFailsClosed_AllDirections(t *testing.T) {
	tests := []struct {
		name   string
		reader io.Reader
		want   string
		forbid string
	}{
		{
			name:   "chat-to-anthropic",
			reader: newOpenAIToAnthropicSSE(strings.NewReader(`data: {"choices":[{"delta":{"content":"partial"}}]}`+"\n\n"), "gpt-x"),
			want:   "event: error",
			forbid: "event: message_stop",
		},
		{
			name:   "anthropic-to-chat",
			reader: newAnthropicToOpenAISSE(strings.NewReader(`event: message_start`+"\n"+`data: {"type":"message_start","message":{"id":"m1"}}`+"\n\n"), "claude-x"),
			want:   `"error"`,
			forbid: `"finish_reason":"stop"`,
		},
		{
			name:   "responses-to-anthropic",
			reader: newResponsesToAnthropicSSE(strings.NewReader(`event: response.created`+"\n"+`data: {"type":"response.created","response":{"id":"r1"}}`+"\n\n"), "gpt-x"),
			want:   "event: error",
			forbid: "event: message_stop",
		},
		{
			name:   "responses-to-chat",
			reader: newResponsesToOpenAISSE(strings.NewReader(`event: response.created`+"\n"+`data: {"type":"response.created","response":{"id":"r1"}}`+"\n\n"), "gpt-x"),
			want:   `"error"`,
			forbid: `"finish_reason":"stop"`,
		},
		{
			name:   "anthropic-to-responses",
			reader: newAnthropicToResponsesSSE(strings.NewReader(`event: message_start`+"\n"+`data: {"type":"message_start","message":{"id":"m1"}}`+"\n\n"), "claude-x"),
			want:   "event: response.failed",
			forbid: "event: response.completed",
		},
		{
			name:   "chat-to-responses",
			reader: newOpenAIToResponsesSSE(strings.NewReader(`data: {"choices":[{"delta":{"content":"partial"}}]}`+"\n\n"), "gpt-x"),
			want:   "event: response.failed",
			forbid: "event: response.completed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := io.ReadAll(tt.reader)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(got, []byte(tt.want)) || bytes.Contains(got, []byte(tt.forbid)) {
				t.Fatalf("unexpected stream:\n%s", got)
			}
		})
	}
}

func TestOpenAIStream_IncompleteToolArgumentsFailsClosed(t *testing.T) {
	in := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"name\":\"f\",\"arguments\":\"{\\\"x\\\":\"}}]}}]}\n\n" +
		"data: [DONE]\n\n"
	outA, _ := io.ReadAll(newOpenAIToAnthropicSSE(strings.NewReader(in), "gpt-x"))
	if !bytes.Contains(outA, []byte("event: error")) || bytes.Contains(outA, []byte("event: message_stop")) {
		t.Fatalf("chat→anthropic accepted incomplete tool JSON:\n%s", outA)
	}
	outR, _ := io.ReadAll(newOpenAIToResponsesSSE(strings.NewReader(in), "gpt-x"))
	if !bytes.Contains(outR, []byte("event: response.failed")) || bytes.Contains(outR, []byte("event: response.completed")) {
		t.Fatalf("chat→responses accepted incomplete tool JSON:\n%s", outR)
	}
}
