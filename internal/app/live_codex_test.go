package app

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

import (
	"net/http/httptest"
	"strings"
	"testing"
)

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
