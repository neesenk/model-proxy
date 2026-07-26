package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestConversionCapabilities_RejectKnownLosses(t *testing.T) {
	tests := []struct {
		name    string
		client  string
		target  string
		body    string
		feature string
	}{
		{"chat multi choice to anthropic", "openai", "anthropic", `{"n":2,"messages":[]}`, "multi_choice"},
		{"chat multi choice to responses", "openai", "responses", `{"n":3,"messages":[]}`, "multi_choice"},
		{"chat logprobs to anthropic", "openai", "anthropic", `{"logprobs":true,"messages":[]}`, "logprobs"},
		{"chat logprobs to responses", "openai", "responses", `{"top_logprobs":5,"messages":[]}`, "logprobs"},
		{"chat audio to anthropic", "openai", "anthropic", `{"messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"eA==","format":"wav"}}]}]}`, "audio"},
		{"chat audio to responses", "openai", "responses", `{"modalities":["text","audio"],"messages":[]}`, "audio"},
		{"responses file search to anthropic", "responses", "anthropic", `{"input":[],"tools":[{"type":"file_search","vector_store_ids":["v"]}]}`, "hosted_tool"},
		{"responses file search to chat", "responses", "openai", `{"input":[],"tools":[{"type":"file_search","vector_store_ids":["v"]}]}`, "hosted_tool"},
		{"responses computer history to anthropic", "responses", "anthropic", `{"input":[{"type":"computer_call","id":"c"}]}`, "input_item"},
		{"responses audio to chat", "responses", "openai", `{"input":[{"type":"message","role":"user","content":[{"type":"input_audio"}]}]}`, "audio"},
		{"anthropic computer to chat", "anthropic", "openai", `{"messages":[],"tools":[{"type":"computer_20250124","name":"computer"}]}`, "hosted_tool"},
		{"anthropic computer to responses", "anthropic", "responses", `{"messages":[],"tools":[{"type":"computer_20250124","name":"computer"}]}`, "hosted_tool"},
		{"anthropic mcp to chat", "anthropic", "openai", `{"messages":[],"mcp_servers":[{"url":"https://mcp.test"}]}`, "mcp"},
		{"anthropic search result to responses", "anthropic", "responses", `{"messages":[{"role":"user","content":[{"type":"search_result","url":"https://example.test"}]}]}`, "content_block"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := convertRequestFor([]byte(tc.body), tc.client, tc.target, convertReqOpts{ImageOK: true})
			unsupported, ok := asUnsupportedConversion(err)
			if !ok {
				t.Fatalf("error = %v, want unsupportedConversionError", err)
			}
			if unsupported.Feature != tc.feature || unsupported.ClientProto != tc.client || unsupported.TargetProto != tc.target {
				t.Fatalf("unsupported = %#v", unsupported)
			}
		})
	}
}

func TestConversionCapabilities_RejectResponsesCustomToolToAnthropic(t *testing.T) {
	body := []byte(`{"model":"m","input":"hi","tools":[{"type":"custom","name":"shell"}]}`)
	err := validateConversionCapabilities(body, "responses", "anthropic")
	var unsupported *unsupportedConversionError
	if !errors.As(err, &unsupported) || unsupported.Feature != "custom_tool" {
		t.Fatalf("err = %v, want custom_tool unsupported conversion", err)
	}
	if err := validateConversionCapabilities(body, "responses", "openai"); err != nil {
		t.Fatalf("custom tool should remain supported through chat: %v", err)
	}
}

func TestConversionCapabilities_AllowSupportedAndSameProtocol(t *testing.T) {
	tests := []struct {
		client string
		target string
		body   string
	}{
		{"responses", "anthropic", `{"input":[],"tools":[{"type":"web_search"},{"type":"tool_search"}]}`},
		{"responses", "openai", `{"input":[],"tools":[{"type":"custom","name":"shell"}]}`},
		{"anthropic", "responses", `{"messages":[],"tools":[{"type":"web_search_20250305","name":"web_search"}]}`},
		{"openai", "anthropic", `{"messages":[{"role":"developer","content":"rule"}],"tools":[{"type":"function","function":{"name":"f"}}]}`},
		// Same-protocol byte passthrough remains outside the conversion guard.
		{"openai", "openai", `{"n":4,"logprobs":true,"messages":[]}`},
	}
	for _, tc := range tests {
		if _, err := convertRequestFor([]byte(tc.body), tc.client, tc.target, convertReqOpts{ImageOK: true}); err != nil {
			t.Errorf("%s→%s: %v", tc.client, tc.target, err)
		}
	}
}

func TestWriteUnsupportedConversionError_ProtocolEnvelopes(t *testing.T) {
	err := &unsupportedConversionError{
		ClientProto: "openai", TargetProto: "anthropic",
		Feature: "audio", Detail: "Chat Completions input_audio content",
	}
	for _, proto := range []string{"openai", "responses", "anthropic"} {
		t.Run(proto, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeUnsupportedConversionError(rec, proto, err)
			if rec.Code != http.StatusBadRequest || rec.Header().Get("content-type") != "application/json" {
				t.Fatalf("status=%d headers=%v body=%s", rec.Code, rec.Header(), rec.Body.String())
			}
			body := unmarshalMap(t, rec.Body.Bytes())
			if proto == "anthropic" {
				if body["type"] != "error" || asMap(body["error"])["type"] != "invalid_request_error" {
					t.Fatalf("anthropic envelope = %v", body)
				}
			} else if asMap(body["error"])["code"] != "unsupported_protocol_conversion" {
				t.Fatalf("openai envelope = %v", body)
			}
		})
	}
}

func TestForward_UnsupportedConversionReturns400WithoutUpstream(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		t.Error("unsupported request reached upstream")
	}))
	defer up.Close()
	cfg := &Config{
		Providers: map[string]Provider{"ant": {
			Provider: "static", AnthropicBaseURL: up.URL, OpenAIBaseURL: up.URL,
		}},
		Routes: map[string][]RouteTarget{"m": {{
			Provider: "ant", Model: "claude", Protocol: "anthropic",
		}}},
	}
	p := newTestProxy(t, cfg)
	p.providers["ant"] = &testProv{key: "k"}
	server := httptest.NewServer(http.HandlerFunc(p.handler))
	defer server.Close()

	resp, err := http.Post(server.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"m","n":2,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest || calls.Load() != 0 {
		t.Fatalf("status=%d calls=%d body=%s", resp.StatusCode, calls.Load(), body)
	}
	if !strings.Contains(string(body), `"code":"unsupported_protocol_conversion"`) ||
		!strings.Contains(string(body), "n=2") {
		t.Fatalf("body=%s", body)
	}
}

func TestForward_UnsupportedTargetFallsThroughToCompatibleProtocol(t *testing.T) {
	var anthropicCalls atomic.Int32
	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		anthropicCalls.Add(1)
		t.Error("unsupported converted target was called")
	}))
	defer anthropic.Close()
	chat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, `{"id":"c","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer chat.Close()

	cfg := &Config{
		Providers: map[string]Provider{
			"ant":  {Provider: "static", AnthropicBaseURL: anthropic.URL, OpenAIBaseURL: anthropic.URL},
			"chat": {Provider: "static", OpenAIBaseURL: chat.URL},
		},
		Routes: map[string][]RouteTarget{"m": {
			{Provider: "ant", Model: "claude", Protocol: "anthropic", Priority: 0},
			{Provider: "chat", Model: "gpt", Protocol: "openai", Priority: 1},
		}},
	}
	p := newTestProxy(t, cfg)
	p.providers["ant"] = &testProv{key: "k"}
	p.providers["chat"] = &testProv{key: "k"}
	server := httptest.NewServer(http.HandlerFunc(p.handler))
	defer server.Close()

	resp, err := http.Post(server.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"m","n":2,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || anthropicCalls.Load() != 0 || !strings.Contains(string(body), `"content":"ok"`) {
		t.Fatalf("status=%d anthropic_calls=%d body=%s", resp.StatusCode, anthropicCalls.Load(), body)
	}
}
