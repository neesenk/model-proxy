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
		Providers: map[string]Provider{"p": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
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

// TestForward_ResponsesStreamPreviousIDRestoresChatToolHistory exercises the
// continuation state on the actual streaming conversion path.  The state must
// contain the client-facing Responses terminal (not the upstream Chat bytes),
// so a later previous_response_id request can rebuild the Chat tool turn.
func TestForward_ResponsesStreamPreviousIDRestoresChatToolHistory(t *testing.T) {
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch hits.Add(1) {
		case 1:
			request := unmarshalMap(t, body)
			if request["stream"] != true {
				t.Fatalf("first upstream stream = %v, want true: %s", request["stream"], body)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `data: {"id":"chat_stream_1","object":"chat.completion.chunk","model":"g","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"weather","arguments":"{}"}}]},"finish_reason":null}]}`+"\n\n")
			_, _ = io.WriteString(w, `data: {"id":"chat_stream_1","object":"chat.completion.chunk","model":"g","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		case 2:
			request := unmarshalMap(t, body)
			messages, _ := request["messages"].([]any)
			if len(messages) != 3 {
				t.Fatalf("restored Chat messages = %d, want user + tool call + tool output: %s", len(messages), body)
			}
			if messages[0] == nil || asMap(messages[0])["role"] != "user" {
				t.Fatalf("restored first message = %#v, want original user: %s", messages[0], body)
			}
			assistant := asMap(messages[1])
			calls, _ := assistant["tool_calls"].([]any)
			if assistant["role"] != "assistant" || len(calls) != 1 || asMap(calls[0])["id"] != "c1" {
				t.Fatalf("restored assistant tool call = %#v: %s", assistant, body)
			}
			tool := asMap(messages[2])
			if tool["role"] != "tool" || tool["tool_call_id"] != "c1" || tool["content"] != "sunny" {
				t.Fatalf("restored tool output = %#v: %s", tool, body)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"chat_stream_2","model":"g","choices":[{"message":{"role":"assistant","content":"sunny"},"finish_reason":"stop"}]}`)
		default:
			t.Fatalf("unexpected upstream request %d", hits.Load())
		}
	}))
	defer up.Close()

	cfg := &Config{
		Providers: map[string]Provider{"p": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"g": {{Provider: "p", Model: "g", Protocol: "openai"}}},
	}
	p := newTestProxy(t, cfg)
	p.providers["p"] = &testProv{key: "k"}
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	post := func(body string) []byte {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, px.URL+"/v1/responses", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Claude-Code-Session-Id", "stream-session")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, body = %s", resp.StatusCode, out)
		}
		return out
	}

	first := post(`{"model":"g","stream":true,"input":"weather?","tools":[{"type":"function","name":"weather","parameters":{"type":"object"}}]}`)
	firstEvents := parseSSE(string(first))
	if got := sseCount(firstEvents, "response.completed"); got != 1 {
		t.Fatalf("completed terminal count = %d, want exactly one: %s", got, first)
	}
	completed := sseDataMap(t, sseFilter(firstEvents, "response.completed")[0])
	if response := asMap(completed["response"]); response["id"] != "chat_stream_1" || response["status"] != "completed" {
		t.Fatalf("completed response = %#v, want clean chat_stream_1 terminal", response)
	}

	continuation := []byte(`{"model":"g","previous_response_id":"chat_stream_1","input":[{"type":"function_call_output","call_id":"c1","output":"sunny"}]}`)
	expanded, history, hit, err := p.responsesState.Expand(continuation, "stream-session")
	if err != nil || !hit {
		t.Fatalf("stream response state hit=%v err=%v, want hit", hit, err)
	}
	if len(history) != 3 {
		t.Fatalf("expanded history items = %d, want original input + one function call + current tool output: %#v", len(history), history)
	}
	if asMap(history[0])["role"] != "user" || asMap(history[1])["type"] != "function_call" || asMap(history[1])["call_id"] != "c1" || asMap(history[2])["type"] != "function_call_output" {
		t.Fatalf("expanded history lost or duplicated the streamed tool turn: %#v", history)
	}
	if strings.Contains(string(expanded), `"previous_response_id"`) {
		t.Fatalf("expanded continuation kept previous_response_id: %s", expanded)
	}

	second := post(string(continuation))
	if !strings.Contains(string(second), `"text":"sunny"`) || hits.Load() != 2 {
		t.Fatalf("second response=%s upstream requests=%d", second, hits.Load())
	}
}

// A stream without a clean Responses terminal is observable by the client but
// must never become continuation history.  This keeps a broken or cancelled
// delivery from replaying an assistant tool call on the next turn.
func TestForward_ResponsesTruncatedStreamDoesNotRecordContinuationState(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"chat_truncated_1","object":"chat.completion.chunk","model":"g","choices":[{"index":0,"delta":{"role":"assistant","content":"partial"},"finish_reason":null}]}`+"\n\n")
		// Deliberately omit the Chat finish chunk and [DONE].  The converted
		// Responses stream has no response.completed terminal to record.
	}))
	defer up.Close()

	cfg := &Config{
		Providers: map[string]Provider{"p": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"g": {{Provider: "p", Model: "g", Protocol: "openai"}}},
	}
	p := newTestProxy(t, cfg)
	p.providers["p"] = &testProv{key: "k"}
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	req, err := http.NewRequest(http.MethodPost, px.URL+"/v1/responses", strings.NewReader(`{"model":"g","stream":true,"input":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Claude-Code-Session-Id", "truncated-session")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if got := sseCount(parseSSE(string(body)), "response.completed"); got != 0 {
		t.Fatalf("truncated stream completed terminals = %d, want 0: %s", got, body)
	}

	_, _, hit, err := p.responsesState.Expand([]byte(`{"model":"g","previous_response_id":"chat_truncated_1","input":"continue"}`), "truncated-session")
	if err != nil {
		t.Fatal(err)
	}
	if hit {
		t.Fatal("truncated stream unexpectedly created continuation state")
	}
}
