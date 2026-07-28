package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestForward_ResponsesPreviousIDRestoresChatToolHistory(t *testing.T) {
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch hits.Add(1) {
		case 1:
			if !strings.Contains(string(body), `"content":"weather?"`) {
				t.Errorf("first request = %s", body)
			}
			w.Header().Set("content-type", "application/json")
			io.WriteString(w, `{"id":"chat_1","model":"g","choices":[{"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"weather","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
		case 2:
			var req map[string]any
			json.Unmarshal(body, &req)
			msgs, _ := req["messages"].([]any)
			if len(msgs) != 3 {
				t.Fatalf("restored chat messages = %d, want 3: %s", len(msgs), body)
			}
			if len(asMap(msgs[1])["tool_calls"].([]any)) != 1 {
				t.Fatalf("assistant tool call missing: %s", body)
			}
			if asMap(msgs[2])["role"] != "tool" || asMap(msgs[2])["tool_call_id"] != "c1" {
				t.Fatalf("tool output missing: %s", body)
			}
			w.Header().Set("content-type", "application/json")
			io.WriteString(w, `{"id":"chat_2","model":"g","choices":[{"message":{"role":"assistant","content":"sunny"},"finish_reason":"stop"}]}`)
		default:
			t.Fatalf("unexpected upstream request %d", hits.Load())
		}
	}))
	defer up.Close()

	cfg := &Config{
		Providers: map[string]Provider{"p": {OpenAIBaseURL: up.URL, Provider: "static"}},
		Routes:    map[string][]RouteTarget{"g": {{Provider: "p", Model: "g", Protocol: "openai"}}},
	}
	p := newTestProxy(t, cfg)
	p.providers["p"] = &testProv{key: "k"}
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	post := func(body string) []byte {
		req, _ := http.NewRequest(http.MethodPost, px.URL+"/v1/responses", strings.NewReader(body))
		req.Header.Set("content-type", "application/json")
		req.Header.Set("x-claude-code-session-id", "sess")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("status=%d body=%s", resp.StatusCode, out)
		}
		return out
	}
	first := post(`{"model":"g","input":"weather?","tools":[{"type":"function","name":"weather","parameters":{"type":"object"}}]}`)
	var firstResp map[string]any
	json.Unmarshal(first, &firstResp)
	if firstResp["id"] != "chat_1" {
		t.Fatalf("first response = %s", first)
	}
	second := post(`{"model":"g","previous_response_id":"chat_1","input":[{"type":"function_call_output","call_id":"c1","output":"sunny"}]}`)
	if !strings.Contains(string(second), "sunny") || hits.Load() != 2 {
		t.Fatalf("second response=%s hits=%d", second, hits.Load())
	}
}
