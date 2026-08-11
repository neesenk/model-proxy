package protocol

import (
	"errors"
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

// Anthropic "custom" is the explicit default type of a regular function tool
// (name+input_schema); it must convert like a type-less tool. Genuine hosted
// tools (computer_*, bash_*, text_editor_*) stay dropped + warned.
func TestAnthropicCustomTypeToolsConvertAsFunctions(t *testing.T) {
	tools := []any{
		map[string]any{
			"type": "custom", "name": "get_weather", "description": "weather",
			"input_schema": map[string]any{"type": "object"},
		},
		map[string]any{"name": "no_type", "input_schema": map[string]any{"type": "object"}},
		map[string]any{"type": "computer_20241022", "name": "computer"},
	}

	chat := anthropicToolsToOpenAI(tools)
	if len(chat) != 2 {
		t.Fatalf("chat tools = %v, want custom+untyped only", chat)
	}
	if fn := asMap(chat[0]["function"]); fn["name"] != "get_weather" || fn["parameters"] == nil {
		t.Fatalf("custom tool not mapped to chat function: %v", chat[0])
	}
	if fn := asMap(chat[1]["function"]); fn["name"] != "no_type" {
		t.Fatalf("untyped tool not mapped to chat function: %v", chat[1])
	}

	resp := anthropicToolsToResponses(tools)
	if len(resp) != 2 {
		t.Fatalf("responses tools = %v, want custom+untyped only", resp)
	}
	if resp[0]["type"] != "function" || resp[0]["name"] != "get_weather" || resp[0]["parameters"] == nil {
		t.Fatalf("custom tool not mapped to responses function: %v", resp[0])
	}
	if resp[1]["type"] != "function" || resp[1]["name"] != "no_type" {
		t.Fatalf("untyped tool not mapped to responses function: %v", resp[1])
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
		// "custom" is Anthropic's explicit default type for regular function
		// tools; it must not be mistaken for a hosted tool.
		{"anthropic", "openai", `{"messages":[],"tools":[{"type":"custom","name":"get_weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}]}`},
		{"anthropic", "responses", `{"messages":[],"tools":[{"type":"custom","name":"get_weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}]}`},
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
