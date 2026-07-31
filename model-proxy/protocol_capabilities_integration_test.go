package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"model-proxy/internal/protocol"
)

func TestWriteUnsupportedConversionError_ProtocolEnvelopes(t *testing.T) {
	err := &protocol.UnsupportedError{
		ClientProto: "openai", TargetProto: "anthropic",
		Feature: "audio", Detail: "Chat Completions input_audio content",
	}
	for _, proto := range []protocol.Protocol{protocol.OpenAI, protocol.Responses, protocol.Anthropic} {
		t.Run(string(proto), func(t *testing.T) {
			rec := httptest.NewRecorder()
			protocol.WriteUnsupportedConversionError(rec, proto, err)
			if rec.Code != http.StatusBadRequest || rec.Header().Get("content-type") != "application/json" {
				t.Fatalf("status=%d headers=%v body=%s", rec.Code, rec.Header(), rec.Body.String())
			}
			body := unmarshalMap(t, rec.Body.Bytes())
			if proto == protocol.Anthropic {
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
			Provider: testProviderID, AnthropicBaseURL: up.URL, OpenAIBaseURL: up.URL,
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
			"ant":  {Provider: testProviderID, AnthropicBaseURL: anthropic.URL, OpenAIBaseURL: anthropic.URL},
			"chat": {Provider: testProviderID, OpenAIBaseURL: chat.URL},
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
