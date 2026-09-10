package app

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	observeevents "model-proxy/internal/observe/events"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---- live_codex_test.go ----

// live_codex_test.go — LIVE tests for the two heaviest conversion paths, all
// gated on MODEL_PROXY_LIVE (see live_e2e_test.go for the helpers):
//
//	A. codex provider (real chatgpt.com/backend-api/codex): anthropic/chat →
//	   Responses. codex is the strictest backend we convert to (input must be
//	   a list; max_output_tokens/temperature/top_p rejected — our converters
//	   strip them; these tests prove it end-to-end).
//	B. r→chat reasoning: effort dialect mapping + reasoning return path on
//	   deepseek / zhipu / kimi-code.
//
// Cost discipline: minimal prompts, small max_tokens. Failure excerpts ≤500
// chars, never tokens.

// liveCodexSkip skips codex tests when the OAuth credential is rejected
// (401/403 = login expired, not a conversion defect).
func liveCodexSkip(t *testing.T, status int, body string) {
	t.Helper()
	if status == 401 || status == 403 {
		t.Skipf("live: codex OAuth rejected (status %d) — re-login needed, not a conversion defect", status)
	}
	liveStatusOK(t, status, body)
}

// liveCodexProxy builds the codex-routed live proxy (explicit protocol:
// responses for determinism).
func liveCodexProxy(t *testing.T) *httptest.Server {
	t.Helper()
	cfg := liveConfig(t)
	model := liveModel(t, cfg, "codex", "gpt-5.5")
	srv, _ := liveProxy(t, cfg, map[string][]RouteTarget{
		"live-cx": {{Provider: "codex", Model: model, Protocol: "responses"}},
	}, "codex")
	return srv
}

// A1: anthropic client → codex. Full anthropic SSE shape with usage.
func TestLive_AnthropicToCodex_Text(t *testing.T) {
	srv := liveCodexProxy(t)
	defer srv.Close()
	status, raw := livePost(t, srv, "/v1/messages",
		`{"model":"live-cx","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"reply with exactly: pong"}]}`)
	liveCodexSkip(t, status, raw)

	events := drainSSE(t, strings.NewReader(raw))
	if got := sseCount(events, "message_start"); got != 1 {
		t.Fatalf("message_start = %d:\n%s", got, liveExcerpt(raw))
	}
	textDeltas := 0
	for _, ev := range sseFilter(events, "content_block_delta") {
		if strOf(asMap(sseDataMap(t, ev)["delta"])["type"]) == "text_delta" {
			textDeltas++
		}
	}
	if textDeltas == 0 {
		t.Fatalf("no text_delta in stream:\n%s", liveExcerpt(raw))
	}
	if got := sseCount(events, "message_stop"); got != 1 {
		t.Fatalf("message_stop = %d:\n%s", got, liveExcerpt(raw))
	}
	// Usage must be non-zero somewhere (message_start input or message_delta output).
	usageOK := false
	for _, ev := range events {
		m := sseDataMap(t, ev)
		if u := asMap(m["usage"]); u != nil && (intOf(u["input_tokens"]) > 0 || intOf(u["output_tokens"]) > 0) {
			usageOK = true
		}
		if msg := asMap(m["message"]); msg != nil {
			if u := asMap(msg["usage"]); u != nil && intOf(u["input_tokens"]) > 0 {
				usageOK = true
			}
		}
	}
	if !usageOK {
		t.Errorf("zero usage in converted stream:\n%s", liveExcerpt(raw))
	}
}

// A2: anthropic → codex tool round trip. turn1 forces a tool_use; turn2
// replays assistant tool_use + user tool_result (codex must accept the
// converted tool history — call_id 回填 included).
func TestLive_AnthropicToCodex_ToolRoundTrip(t *testing.T) {
	srv := liveCodexProxy(t)
	defer srv.Close()

	tool := `{"name":"get_weather","description":"Get current weather for a city","input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}`
	turn1 := `{"model":"live-cx","max_tokens":512,"stream":true,` +
		`"tools":[` + tool + `],` +
		`"messages":[{"role":"user","content":"What is the weather in Paris right now? You MUST call the get_weather tool to answer."}]}`
	status, raw1 := livePost(t, srv, "/v1/messages", turn1)
	liveCodexSkip(t, status, raw1)

	var toolID, toolName, toolInput string
	for _, ev := range drainSSE(t, strings.NewReader(raw1)) {
		if ev.data == "[DONE]" {
			continue
		}
		m := sseDataMap(t, ev)
		if cb := asMap(m["content_block"]); cb != nil && strOpt(cb["type"]) == "tool_use" {
			toolID = strOpt(cb["id"])
			toolName = strOpt(cb["name"])
		}
		if d := asMap(m["delta"]); d != nil && strOpt(d["type"]) == "input_json_delta" {
			toolInput += strOpt(d["partial_json"])
		}
	}
	liveRequireWeatherCall(t, toolID, toolName, toolInput, raw1)

	turn2 := `{"model":"live-cx","max_tokens":512,"stream":true,` +
		`"tools":[` + tool + `],` +
		`"messages":[` +
		`{"role":"user","content":"What is the weather in Paris right now? You MUST call the get_weather tool to answer."},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"` + toolID + `","name":"` + toolName + `","input":` + toolInput + `}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + toolID + `","content":"sunny, 22°C"}]}]}`
	status, raw2 := livePost(t, srv, "/v1/messages", turn2)
	liveCodexSkip(t, status, raw2)
	events2 := drainSSE(t, strings.NewReader(raw2))
	if got := sseCount(events2, "message_stop"); got != 1 {
		t.Fatalf("turn 2 message_stop = %d:\n%s", got, liveExcerpt(raw2))
	}
}

// A3 (most valuable): reasoning replay. turn1 with thinking enabled — collect
// thinking text + signature (from codex reasoning encrypted_content); turn2
// replays them. 200 = the signature round-trips byte-exact on the real
// backend (its signature validation accepts our replay).
func TestLive_AnthropicToCodex_ReasoningReplay(t *testing.T) {
	srv := liveCodexProxy(t)
	defer srv.Close()

	turn1 := `{"model":"live-cx","max_tokens":2048,"stream":true,` +
		`"thinking":{"type":"enabled","budget_tokens":1024},` +
		`"messages":[{"role":"user","content":"What is 17*23? Think briefly, then answer with just the number."}]}`
	status, raw1 := livePost(t, srv, "/v1/messages", turn1)
	liveCodexSkip(t, status, raw1)

	var thinking strings.Builder
	var signature string
	var redacted []string
	for _, ev := range drainSSE(t, strings.NewReader(raw1)) {
		if ev.data == "[DONE]" {
			continue
		}
		m := sseDataMap(t, ev)
		if cb := asMap(m["content_block"]); cb != nil && strOpt(cb["type"]) == "redacted_thinking" {
			if data := strOpt(cb["data"]); data != "" {
				redacted = append(redacted, data)
			}
		}
		if d := asMap(m["delta"]); d != nil {
			switch strOpt(d["type"]) {
			case "thinking_delta":
				thinking.WriteString(strOpt(d["thinking"]))
			case "signature_delta":
				signature += strOpt(d["signature"])
			}
		}
	}
	if signature == "" && len(redacted) == 0 {
		t.Fatalf("live: no signature/redacted_thinking in converted stream:\n%s", liveExcerpt(raw1))
	}

	// Rebuild the assistant turn: redacted blocks first, then the signed
	// thinking block (anthropic ordering), then the answer placeholder.
	var blocks strings.Builder
	for _, d := range redacted {
		blocks.WriteString(`{"type":"redacted_thinking","data":` + mustJSONStr(t, d) + `},`)
	}
	if signature != "" {
		blocks.WriteString(`{"type":"thinking","thinking":` + mustJSONStr(t, thinking.String()) + `,"signature":` + mustJSONStr(t, signature) + `},`)
	}
	blocks.WriteString(`{"type":"text","text":"391"}`)
	turn2 := `{"model":"live-cx","max_tokens":2048,"stream":true,` +
		`"thinking":{"type":"enabled","budget_tokens":1024},` +
		`"messages":[` +
		`{"role":"user","content":"What is 17*23? Think briefly, then answer with just the number."},` +
		`{"role":"assistant","content":[` + blocks.String() + `]},` +
		`{"role":"user","content":"and what is 391+9? just the number"}]}`
	status, raw2 := livePost(t, srv, "/v1/messages", turn2)
	liveCodexSkip(t, status, raw2)
	// 200 = codex's signature validation accepted our verbatim replay.
	if got := sseCount(drainSSE(t, strings.NewReader(raw2)), "message_stop"); got != 1 {
		t.Fatalf("turn 2 message_stop = %d:\n%s", got, liveExcerpt(raw2))
	}
}

// A4: chat client → codex. chat.completion.chunk shape with finish + [DONE].
func TestLive_ChatToCodex_Text(t *testing.T) {
	srv := liveCodexProxy(t)
	defer srv.Close()
	status, raw := livePost(t, srv, "/v1/chat/completions",
		`{"model":"live-cx","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"reply with exactly: pong"}]}`)
	liveCodexSkip(t, status, raw)

	events := parseSSE(raw)
	sawRole, sawContent, sawFinish, sawDone := false, false, false, false
	for _, ev := range events {
		if ev.data == "[DONE]" {
			sawDone = true
			continue
		}
		m := unmarshalMap(t, []byte(ev.data))
		for _, c := range m["choices"].([]any) {
			ch := asMap(c)
			d := asMap(ch["delta"])
			if strOpt(d["role"]) == "assistant" {
				sawRole = true
			}
			if strOpt(d["content"]) != "" {
				sawContent = true
			}
			if fr, ok := ch["finish_reason"].(string); ok && fr != "" {
				sawFinish = true
			}
		}
	}
	if !sawRole || !sawContent || !sawFinish || !sawDone {
		t.Errorf("chat chunk shape incomplete (role=%v content=%v finish=%v DONE=%v):\n%s",
			sawRole, sawContent, sawFinish, sawDone, liveExcerpt(raw))
	}
}

// B5: r→chat reasoning — effort dialect mapping + reasoning return path.
func TestLive_ResponsesToChat_Reasoning(t *testing.T) {
	providers := []struct{ name, prefer string }{
		{"deepseek", "deepseek-v4-pro"},      // ChatReasoningMode = thinking
		{"zhipu", "glm-4.7"},                 // thinking
		{"kimi-code", "k3"},                  // thinking
		{"qwen-plan", "qwen3.8-max-preview"}, // enable_thinking
		{"aqp", "glm-5.2"},                   // OpenRouter reasoning object
	}
	for _, tc := range providers {
		t.Run(tc.name, func(t *testing.T) {
			cfg := liveConfig(t)
			model := liveModel(t, cfg, tc.name, tc.prefer)
			srv, _ := liveProxy(t, cfg, map[string][]RouteTarget{
				"live-m": {{Provider: tc.name, Model: model, Protocol: "openai"}},
			}, tc.name)
			defer srv.Close()

			body := `{"model":"live-m","stream":true,"max_output_tokens":512,` +
				`"reasoning":{"effort":"low"},` +
				`"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"What is 17*23? Think briefly, then answer with just the number."}]}]}`
			raw := liveResponsesTurn(t, srv, body)
			events := drainSSE(t, strings.NewReader(raw))
			var reasoning strings.Builder
			for _, ev := range events {
				switch sseEventType(ev) {
				case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
					reasoning.WriteString(strOpt(sseDataMap(t, ev)["delta"]))
				}
			}
			if strings.TrimSpace(reasoning.String()) == "" {
				t.Fatalf("no non-empty reasoning text in stream:\n%s", liveExcerpt(raw))
			}
		})
	}
}

// B6: plain multi-turn text through r→chat (no-tool regression).
func TestLive_ResponsesToChat_MultiTurnText(t *testing.T) {
	cfg := liveConfig(t)
	model := liveModel(t, cfg, "deepseek", "deepseek-v4-pro")
	srv, _ := liveProxy(t, cfg, map[string][]RouteTarget{
		"live-m": {{Provider: "deepseek", Model: model, Protocol: "openai"}},
	}, "deepseek")
	defer srv.Close()

	body := `{"model":"live-m","stream":true,"max_output_tokens":64,` +
		`"input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"my name is Ada"}]},` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Nice to meet you, Ada."}]},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"what is my name? one word"}]}]}`
	raw := liveResponsesTurn(t, srv, body)
	events := drainSSE(t, strings.NewReader(raw))
	text := ""
	for _, ev := range sseFilter(events, "response.output_text.delta") {
		text += strOf(sseDataMap(t, ev)["delta"])
	}
	if !strings.Contains(strings.ToLower(text), "ada") {
		t.Errorf("multi-turn context lost (no 'Ada' in answer): %q", text)
	}
}

// ---- live_config_test.go ----

func TestLiveConfigPathDefaultsToRepositoryConfig(t *testing.T) {
	t.Setenv(liveConfigEnv, "")

	got, err := liveConfigPath()
	if err != nil {
		t.Fatalf("liveConfigPath: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("liveConfigPath = %q, want absolute path", got)
	}
	if filepath.Base(got) != "config.yaml" {
		t.Fatalf("liveConfigPath = %q, want repository config.yaml", got)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(got), "go.mod")); err != nil {
		t.Fatalf("liveConfigPath = %q, parent is not repository root: %v", got, err)
	}
}

func TestLiveConfigPathOverride(t *testing.T) {
	t.Run("absolute", func(t *testing.T) {
		want := filepath.Join(t.TempDir(), "live.yaml")
		t.Setenv(liveConfigEnv, want)

		got, err := liveConfigPath()
		if err != nil {
			t.Fatalf("liveConfigPath: %v", err)
		}
		if got != want {
			t.Fatalf("liveConfigPath = %q, want %q", got, want)
		}
	})

	t.Run("repository-relative", func(t *testing.T) {
		const override = "testdata/live.yaml"
		t.Setenv(liveConfigEnv, override)

		root, err := liveRepositoryRoot()
		if err != nil {
			t.Fatalf("liveRepositoryRoot: %v", err)
		}
		got, err := liveConfigPath()
		if err != nil {
			t.Fatalf("liveConfigPath: %v", err)
		}
		want := filepath.Join(root, override)
		if got != want {
			t.Fatalf("liveConfigPath = %q, want %q", got, want)
		}
	})
}

func TestValidateLiveResponsesCompleted(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{
			name: "single completed",
			raw:  "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n",
		},
		{
			name:    "missing terminal",
			raw:     "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n",
			wantErr: "completed/incomplete/failed = 0/0/0",
		},
		{
			name:    "incomplete",
			raw:     "event: response.incomplete\ndata: {\"type\":\"response.incomplete\"}\n\n",
			wantErr: "completed/incomplete/failed = 0/1/0",
		},
		{
			name: "completed and incomplete",
			raw: "event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n" +
				"event: response.incomplete\ndata: {\"type\":\"response.incomplete\"}\n\n",
			wantErr: "completed/incomplete/failed = 1/1/0",
		},
		{
			name: "duplicate completed",
			raw: "event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n" +
				"event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n",
			wantErr: "completed/incomplete/failed = 2/0/0",
		},
		{
			name:    "completed frame missing response",
			raw:     "event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n",
			wantErr: "missing nested response object",
		},
		{
			name:    "completed frame missing status",
			raw:     "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"error\":null}}\n\n",
			wantErr: "missing nested response.status",
		},
		{
			name:    "failed",
			raw:     "event: response.failed\ndata: {\"type\":\"response.failed\"}\n\n",
			wantErr: "completed/incomplete/failed = 0/0/1",
		},
		{
			name:    "cancelled",
			raw:     "event: response.cancelled\ndata: {\"type\":\"response.cancelled\"}\n\n",
			wantErr: "completed/incomplete/failed = 0/0/1",
		},
		{
			name:    "completed frame with failed status",
			raw:     "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"failed\"}}\n\n",
			wantErr: "nested status = \"failed\"",
		},
		{
			name:    "completed frame with cancelled status",
			raw:     "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"cancelled\"}}\n\n",
			wantErr: "nested status = \"cancelled\"",
		},
		{
			name:    "completed frame with nested error",
			raw:     "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"error\":{\"message\":\"bad\"}}}\n\n",
			wantErr: "nested response.error",
		},
		{
			name: "error event despite completion",
			raw: "event: error\ndata: {\"type\":\"error\",\"message\":\"bad\"}\n\n" +
				"event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n",
			wantErr: "unexpected SSE error event",
		},
		{
			name: "error payload despite completion",
			raw: "event: response.output_text.delta\ndata: {\"error\":{\"message\":\"bad\"}}\n\n" +
				"event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n",
			wantErr: "unexpected SSE error payload",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateLiveResponsesCompleted(parseSSE(tt.raw))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateLiveResponsesCompleted: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateLiveResponsesCompleted error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateLiveWeatherCall(t *testing.T) {
	tests := []struct {
		name               string
		callID, tool, args string
		wantErr            string
	}{
		{name: "valid", callID: "call-1", tool: "get_weather", args: `{"city":"Paris"}`},
		{name: "missing id", tool: "get_weather", args: `{"city":"Paris"}`, wantErr: "id is empty"},
		{name: "missing name", callID: "call-1", args: `{"city":"Paris"}`, wantErr: "name is empty"},
		{name: "missing arguments", callID: "call-1", tool: "get_weather", wantErr: "arguments are empty"},
		{name: "wrong name", callID: "call-1", tool: "other", args: `{"city":"Paris"}`, wantErr: "want get_weather"},
		{name: "invalid JSON", callID: "call-1", tool: "get_weather", args: `{"city":`, wantErr: "valid JSON object"},
		{name: "JSON array", callID: "call-1", tool: "get_weather", args: `["Paris"]`, wantErr: "valid JSON object"},
		{name: "missing city", callID: "call-1", tool: "get_weather", args: `{}`, wantErr: "want Paris"},
		{name: "wrong city", callID: "call-1", tool: "get_weather", args: `{"city":"London"}`, wantErr: "want Paris"},
		{name: "wrong city case", callID: "call-1", tool: "get_weather", args: `{"city":"paris"}`, wantErr: "want Paris"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateLiveWeatherCall(tt.callID, tt.tool, tt.args)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateLiveWeatherCall: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateLiveWeatherCall error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

// ---- live_events_test.go ----

// TestForward_EmitsLiveEvents: a served request publishes a start event (on
// entry) and an end event (on commit) to the hub, carrying agent/route/provider/
// status/latency.
func TestForward_EmitsLiveEvents(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{}`))
	}))
	defer up.Close()

	cfg := &Config{
		Providers: map[string]Provider{"z": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"glm": {{Provider: "z", Model: "glm"}}},
	}
	p := newTestProxy(t, cfg)
	p.providers["z"] = &testProv{key: "k"}
	ch, _, cancel := p.events.Subscribe()
	defer cancel()

	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()
	req, _ := http.NewRequest(http.MethodPost, px.URL+"/v1/responses", strings.NewReader(`{"model":"glm","input":[]}`))
	req.Header.Set("user-agent", "claude-cli/1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// Collect events with a short grace for the async publish.
	var got []observeevents.Event
	deadline := time.After(time.Second)
	for len(got) < 2 {
		select {
		case e := <-ch:
			got = append(got, e)
		case <-deadline:
			t.Fatalf("received %d events, want 2 (start+end): %+v", len(got), got)
		}
	}
	if got[0].Type != "start" {
		t.Errorf("first event type=%q want start", got[0].Type)
	}
	if got[0].Agent != "claude-code" || got[0].Exposed != "glm" {
		t.Errorf("start event = %+v want agent claude-code / exposed glm", got[0])
	}
	var endEv observeevents.Event
	for _, e := range got {
		if e.Type == "end" {
			endEv = e
		}
	}
	if endEv.Type != "end" {
		t.Fatal("no end event received")
	}
	if endEv.Provider != "z" || endEv.UpstreamModel != "glm" || endEv.Status != 200 {
		t.Errorf("end event = %+v want provider z / glm / 200", endEv)
	}
	if endEv.LatencyMs < 0 {
		t.Errorf("end latency=%d negative", endEv.LatencyMs)
	}
}

// TestForward_LiveEndEventCarriesStreamUsage proves that the terminal live
// event is published only after the client-facing SSE stream has passed through
// targetexec's usage capture. The usage frame is the normal OpenAI Chat
// completion shape, rather than a synthetic event DTO. The route's target model
// (gpt-x) differs from the called model (glm), so the client-facing stream is
// model-normalized while the usage frames pass through.
func TestForward_LiveEndEventCarriesStreamUsage(t *testing.T) {
	const upstreamStream = "data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-x\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-x\",\"choices\":[],\"usage\":{\"prompt_tokens\":17,\"completion_tokens\":9,\"total_tokens\":26}}\n\n" +
		"data: [DONE]\n\n"
	const clientStream = "data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"model\":\"glm\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"model\":\"glm\",\"choices\":[],\"usage\":{\"prompt_tokens\":17,\"completion_tokens\":9,\"total_tokens\":26}}\n\n" +
		"data: [DONE]\n\n"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, upstreamStream)
	}))
	defer up.Close()

	p := newTestProxy(t, &Config{
		Providers: map[string]Provider{"z": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"glm": {{Provider: "z", Model: "gpt-x", Protocol: "openai"}}},
	})
	p.providers["z"] = &testProv{key: "k"}
	ch, _, cancel := p.events.Subscribe()
	defer cancel()
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	req, err := http.NewRequest(http.MethodPost, px.URL+"/v1/chat/completions", strings.NewReader(
		`{"model":"glm","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("user-agent", "claude-cli/1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("content-type"), "text/event-stream") || string(body) != clientStream {
		t.Fatalf("client stream status=%d content-type=%q body=%q", resp.StatusCode, resp.Header.Get("content-type"), body)
	}

	var start, end observeevents.Event
	deadline := time.After(time.Second)
	for end.Type == "" {
		select {
		case event := <-ch:
			switch event.Type {
			case "start":
				start = event
			case "end":
				end = event
			}
		case <-deadline:
			t.Fatalf("did not receive terminal live event; start=%+v end=%+v", start, end)
		}
	}
	if start.Agent != "claude-code" || start.Exposed != "glm" {
		t.Errorf("start event = %+v, want agent claude-code / exposed glm", start)
	}
	if start.RequestID == "" || end.RequestID != start.RequestID ||
		end.Agent != "claude-code" || end.Protocol != "openai" ||
		end.Exposed != "glm" || end.Provider != "z" || end.UpstreamModel != "gpt-x" || end.Status != http.StatusOK ||
		end.Input != 17 || end.Output != 9 || end.LatencyMs < 0 {
		t.Errorf("start=%+v terminal=%+v, want matching request id; claude-code/openai/glm/z/gpt-x/200; usage 17/9; non-negative latency", start, end)
	}
}

// TestServeEvents_SSE: the /api/events endpoint streams events as SSE `data:`
// lines; a published event reaches an HTTP subscriber.
func TestServeEvents_SSE(t *testing.T) {
	p := newTestProxy(t, &Config{
		Providers: map[string]Provider{"z": {OpenAIBaseURL: "https://x", Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"glm": {{Provider: "z", Model: "glm"}}},
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { observeevents.ServeEvents(p.events, w, r) }))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("content-type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("content-type=%q want text/event-stream", ct)
	}

	// Publish an event; it must arrive as an SSE data line.
	p.events.Publish(observeevents.Event{
		Type: "end", RequestID: "sse-test", Agent: "codex",
		Exposed: "glm", Provider: "z", Status: http.StatusOK,
	})
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var got observeevents.Event
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &got); err != nil {
			t.Fatalf("decode SSE event %q: %v", line, err)
		}
		if got.RequestID == "sse-test" {
			break
		}
	}
	if got.RequestID != "sse-test" {
		t.Fatalf("did not receive the published SSE event before deadline; scan err=%v context err=%v", sc.Err(), ctx.Err())
	}
	want := observeevents.Event{
		Type: "end", RequestID: "sse-test", Agent: "codex", Exposed: "glm",
		Provider: "z", Status: http.StatusOK,
	}
	if got.Type != want.Type || got.Agent != want.Agent || got.Exposed != want.Exposed ||
		got.Provider != want.Provider || got.Status != want.Status {
		t.Errorf("SSE event = %+v, want core fields %+v", got, want)
	}
}

// TestLiveEvents_EarlyFailures: the two early-return paths that previously
// emitted NO event — a missing-model 400 and an unknown-model 502 — now emit a
// terminal "end" event, so an agent retry-looping on a malformed/removed model
// is visible (the core "catch a retry loop" use case).
func TestLiveEvents_EarlyFailures(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{"z": {OpenAIBaseURL: "http://127.0.0.1:1", Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"glm": {{Provider: "z", Model: "glm"}}},
	}
	p := newTestProxy(t, cfg)
	p.providers["z"] = &testProv{key: "k"}
	ch, _, cancel := p.events.Subscribe()
	defer cancel()
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	expectEnd := func(body string, wantStatus int) {
		resp, err := http.Post(px.URL+"/v1/responses", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		for {
			select {
			case e := <-ch:
				if e.Type == "end" && e.Status == wantStatus {
					return
				}
			case <-time.After(time.Second):
				t.Fatalf("no end event (status=%d) for body %q", wantStatus, body)
			}
		}
	}
	// Missing model → 400 → end event.
	expectEnd(`{"input":[]}`, http.StatusBadRequest)
	// Unknown model → 502 → end event.
	expectEnd(`{"model":"ghost","input":[]}`, http.StatusBadGateway)
}

// TestLiveEvents_CacheHitAndAllFailed (F6c): the two live-view blind spots — a
// cache hit (returns before the normal start event) and an all-targets-failed
// 502 — each emit an explicit end event. Without this, a retry-looping agent
// served from cache or always 502-ing is invisible to the live monitor.
func TestLiveEvents_CacheHitAndAllFailed(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()

	mk := func(routes map[string][]RouteTarget, cache bool) *Proxy {
		cfg := &Config{
			Providers: map[string]Provider{"z": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
			Routes:    routes,
		}
		if cache {
			cfg.Cache = CacheConfig{Enabled: true, TTL: "1h"}
		}
		p := newTestProxy(t, cfg)
		p.providers["z"] = &testProv{key: "k"}
		return p
	}

	// Cache hit → end event with Provider "(cache)".
	pc := mk(map[string][]RouteTarget{"glm": {{Provider: "z", Model: "glm"}}}, true)
	ch, _, cancel := pc.events.Subscribe()
	defer cancel()
	pxc := httptest.NewServer(http.HandlerFunc(pc.Handler))
	defer pxc.Close()
	postOK(t, pxc.URL+"/v1/responses", `{"model":"glm","input":[]}`) // prime
	postOK(t, pxc.URL+"/v1/responses", `{"model":"glm","input":[]}`) // cache hit
	gotCache := false
	deadline := time.After(time.Second)
	for !gotCache {
		select {
		case e := <-ch:
			if e.Type == "end" && e.Provider == "(cache)" {
				gotCache = true
			}
		case <-deadline:
			t.Fatal("cache hit did not emit a live end event with provider (cache)")
		}
	}

	// All-failed 502 → end event with Status 502.
	cfg := &Config{
		Providers: map[string]Provider{"dead": {OpenAIBaseURL: "http://127.0.0.1:1", Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"glm": {{Provider: "dead", Model: "glm"}}},
	}
	pf2 := newTestProxy(t, cfg)
	pf2.providers["dead"] = &testProv{key: "k"}
	ch2, _, cancel2 := pf2.events.Subscribe()
	defer cancel2()
	pxf := httptest.NewServer(http.HandlerFunc(pf2.Handler))
	defer pxf.Close()
	resp, err := http.Post(pxf.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"glm","input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("all-failed status=%d want 502", resp.StatusCode)
	}
	resp.Body.Close()
	got502 := false
	deadline2 := time.After(time.Second)
	for !got502 {
		select {
		case e := <-ch2:
			if e.Type == "end" && e.Status == http.StatusBadGateway {
				got502 = true
			}
		case <-deadline2:
			t.Fatal("all-failed 502 did not emit a live end event")
		}
	}
}
