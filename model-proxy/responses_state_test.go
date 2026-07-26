package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestResponsesState_RecordExpandAndPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "responses_state.json")
	s := newResponsesStateStore(path)
	history := []any{map[string]any{
		"type": "message", "role": "user",
		"content": []any{map[string]any{"type": "input_text", "text": "weather?"}},
	}}
	resp := []byte(`{"id":"resp_1","status":"completed","output":[{"type":"function_call","call_id":"c1","name":"weather","arguments":"{}"}]}`)
	if !s.recordJSON("sess", history, resp) {
		t.Fatal("recordJSON returned false")
	}
	s.close()

	s2 := newResponsesStateStore(path)
	defer s2.close()
	in := []byte(`{"model":"g","previous_response_id":"resp_1","input":[{"type":"function_call_output","call_id":"c1","output":"sunny"}]}`)
	got, expanded, hit, err := s2.expand(in, "sess")
	if err != nil {
		t.Fatal(err)
	}
	if !hit || len(expanded) != 3 {
		t.Fatalf("hit=%v expanded=%v", hit, expanded)
	}
	var body map[string]any
	json.Unmarshal(got, &body)
	if _, ok := body["previous_response_id"]; ok {
		t.Fatalf("previous_response_id not stripped: %s", got)
	}
	if typ := strOf(asMap(expanded[1])["type"]); typ != "function_call" {
		t.Fatalf("restored item type = %q", typ)
	}
	if typ := strOf(asMap(expanded[2])["type"]); typ != "function_call_output" {
		t.Fatalf("continuation item type = %q", typ)
	}
}

func TestResponsesState_MissRepairsOrphanedOutput(t *testing.T) {
	s := newResponsesStateStore("")
	in := []byte(`{"model":"g","previous_response_id":"missing","input":[` +
		`{"type":"reasoning","summary":[{"type":"summary_text","text":"secret"}]},` +
		`{"type":"function_call_output","call_id":"c9","output":"result"}]}`)
	_, expanded, hit, err := s.expand(in, "sess")
	if err != nil {
		t.Fatal(err)
	}
	if hit || len(expanded) != 1 {
		t.Fatalf("hit=%v expanded=%v", hit, expanded)
	}
	msg := asMap(expanded[0])
	if msg["type"] != "message" || msg["role"] != "user" {
		t.Fatalf("orphan was not repaired as user message: %v", msg)
	}
	if text := strOf(asMap(asSlice(msg["content"], 0))["text"]); !strings.Contains(text, "result") || !strings.Contains(text, "c9") {
		t.Fatalf("repair text = %q", text)
	}
}

func TestResponsesState_TTLExpiry(t *testing.T) {
	s := newResponsesStateStore("")
	now := time.Unix(1000, 0)
	s.now = func() time.Time { return now }
	if !s.recordJSON("", []any{map[string]any{"type": "message"}}, []byte(`{"id":"r1","status":"completed","output":[]}`)) {
		t.Fatal("record failed")
	}
	now = now.Add(responsesStateTTL + time.Second)
	_, _, hit, err := s.expand([]byte(`{"model":"g","previous_response_id":"r1","input":"next"}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if hit {
		t.Fatal("expired state was returned")
	}
}

func TestResponsesState_RecordsOnlyReplaySafeIncomplete(t *testing.T) {
	s := newResponsesStateStore("")
	history := []any{map[string]any{"type": "message", "role": "user"}}
	if s.recordJSON("sess", history, []byte(`{
		"id":"filtered","status":"incomplete",
		"incomplete_details":{"reason":"content_filter"},
		"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"partial"}]}]
	}`)) {
		t.Fatal("content-filtered incomplete response was cached for replay")
	}
	if !s.recordJSON("sess", history, []byte(`{
		"id":"limited","status":"incomplete",
		"incomplete_details":{"reason":"max_output_tokens"},
		"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"partial"}]}]
	}`)) {
		t.Fatal("max_output_tokens incomplete response was not cached")
	}
	_, expanded, hit, err := s.expand([]byte(`{
		"model":"g","previous_response_id":"limited","input":"continue"
	}`), "sess")
	if err != nil {
		t.Fatal(err)
	}
	if !hit || len(expanded) != 3 {
		t.Fatalf("replay-safe incomplete expansion hit=%v history=%#v", hit, expanded)
	}
}

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
