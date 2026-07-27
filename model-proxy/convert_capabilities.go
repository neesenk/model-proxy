package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	sonic "github.com/bytedance/sonic"
)

// unsupportedConversionError is a client-input error, not a provider failure.
// Another route target may still serve the request if its protocol is compatible.
type unsupportedConversionError struct {
	ClientProto string
	TargetProto string
	Feature     string
	Detail      string
}

func (e *unsupportedConversionError) Error() string {
	detail := e.Detail
	if detail == "" {
		detail = e.Feature
	}
	return fmt.Sprintf("%s cannot be converted from %s to %s without data or execution-semantics loss",
		detail, e.ClientProto, e.TargetProto)
}

func asUnsupportedConversion(err error) (*unsupportedConversionError, bool) {
	var target *unsupportedConversionError
	return target, errors.As(err, &target)
}

func unsupportedFeature(clientProto, targetProto, feature, detail string) error {
	return &unsupportedConversionError{
		ClientProto: clientProto,
		TargetProto: targetProto,
		Feature:     feature,
		Detail:      detail,
	}
}

// validateConversionCapabilities rejects known lossy request features before a
// pairwise converter can drop them. Documented degradations such as Anthropic
// thinking → Chat reasoning_content remain converter-owned.
func validateConversionCapabilities(body []byte, clientProto, targetProto string) error {
	if !needsConversion(clientProto, targetProto) {
		return nil
	}
	var root map[string]any
	if err := sonic.Unmarshal(body, &root); err != nil {
		// Let the selected converter return its protocol-specific parse error.
		return nil
	}
	switch clientProto {
	case "openai":
		return validateChatConversionCapabilities(root, targetProto)
	case "responses":
		return validateResponsesConversionCapabilities(root, targetProto)
	case "anthropic":
		return validateAnthropicConversionCapabilities(root, targetProto)
	default:
		return nil
	}
}

func validateChatConversionCapabilities(root map[string]any, targetProto string) error {
	if targetProto == "anthropic" && strOpt(root["prompt_cache_retention"]) != "" {
		return unsupportedFeature("openai", targetProto, "cache_retention",
			"Chat Completions prompt_cache_retention (Anthropic supports at most a 1h breakpoint TTL)")
	}
	if n := intOf(root["n"]); n > 1 {
		return unsupportedFeature("openai", targetProto, "multi_choice",
			fmt.Sprintf("Chat Completions n=%d (target protocols expose one generation)", n))
	}
	if root["logprobs"] == true || intOf(root["top_logprobs"]) > 0 {
		return unsupportedFeature("openai", targetProto, "logprobs",
			"Chat Completions logprobs/top_logprobs")
	}
	if containsString(anySlice(root["modalities"]), "audio") || root["audio"] != nil {
		return unsupportedFeature("openai", targetProto, "audio",
			"Chat Completions audio output")
	}
	for _, raw := range anySlice(root["messages"]) {
		msg := asMap(raw)
		if msg == nil {
			continue
		}
		if msg["audio"] != nil {
			return unsupportedFeature("openai", targetProto, "audio",
				"Chat Completions assistant audio history")
		}
		for _, rawPart := range anySlice(msg["content"]) {
			if strOpt(asMap(rawPart)["type"]) == "input_audio" {
				return unsupportedFeature("openai", targetProto, "audio",
					"Chat Completions input_audio content")
			}
		}
	}
	for _, raw := range anySlice(root["tools"]) {
		toolType := strOpt(asMap(raw)["type"])
		if toolType != "" && toolType != "function" {
			return unsupportedFeature("openai", targetProto, "hosted_tool",
				"Chat Completions tool type "+toolType)
		}
	}
	return nil
}

func validateResponsesConversionCapabilities(root map[string]any, targetProto string) error {
	if targetProto == "anthropic" && strOpt(root["prompt_cache_retention"]) != "" {
		return unsupportedFeature("responses", targetProto, "cache_retention",
			"Responses prompt_cache_retention (Anthropic supports at most a 1h breakpoint TTL)")
	}
	if intOf(root["top_logprobs"]) > 0 {
		return unsupportedFeature("responses", targetProto, "logprobs",
			"Responses top_logprobs")
	}
	for _, raw := range responsesRequestTools(root) {
		tool := asMap(raw)
		toolType := strOpt(tool["type"])
		if targetProto == "anthropic" && toolType == "custom" {
			return unsupportedFeature("responses", targetProto, "custom_tool",
				"Responses custom/freeform tools (Anthropic has no raw-input tool contract)")
		}
		switch toolType {
		case "", "function", "custom", "namespace", "web_search", "web_search_preview", "tool_search":
		default:
			return unsupportedFeature("responses", targetProto, "hosted_tool",
				"Responses hosted tool type "+toolType)
		}
	}
	for _, item := range responsesInputItems(root["input"]) {
		itemType := strOpt(item["type"])
		switch itemType {
		case "", "message", "function_call", "function_call_output",
			"custom_tool_call", "custom_tool_call_output",
			"tool_search_call", "tool_search_output", "web_search_call",
			"additional_tools", "reasoning":
		default:
			return unsupportedFeature("responses", targetProto, "input_item",
				"Responses input item type "+itemType)
		}
		if itemType != "message" {
			continue
		}
		for _, rawPart := range anySlice(item["content"]) {
			partType := strOpt(asMap(rawPart)["type"])
			if partType == "input_audio" || partType == "audio" {
				return unsupportedFeature("responses", targetProto, "audio",
					"Responses audio content")
			}
		}
	}
	return nil
}

func validateAnthropicConversionCapabilities(root map[string]any, targetProto string) error {
	if root["mcp_servers"] != nil {
		return unsupportedFeature("anthropic", targetProto, "mcp",
			"Anthropic mcp_servers")
	}
	for _, raw := range anySlice(root["tools"]) {
		toolType := strOpt(asMap(raw)["type"])
		// "" and "custom" both denote a regular client-side function tool
		// ("custom" is Anthropic's explicit default type for name+input_schema
		// tools); only hosted server tools fail closed here.
		if toolType == "" || toolType == "custom" || strings.HasPrefix(toolType, "web_search") {
			continue
		}
		return unsupportedFeature("anthropic", targetProto, "hosted_tool",
			"Anthropic tool type "+toolType)
	}
	for _, raw := range anySlice(root["messages"]) {
		msg := asMap(raw)
		for _, rawBlock := range anySlice(msg["content"]) {
			block := asMap(rawBlock)
			blockType := strOpt(block["type"])
			switch blockType {
			case "", "text", "image", "document", "tool_use", "tool_result",
				"thinking", "redacted_thinking", "web_search_tool_result":
			case "server_tool_use":
				if strOpt(block["name"]) == "web_search" {
					continue
				}
				return unsupportedFeature("anthropic", targetProto, "hosted_tool",
					"Anthropic server tool call "+firstNonEmpty(strOpt(block["name"]), "unknown"))
			default:
				return unsupportedFeature("anthropic", targetProto, "content_block",
					"Anthropic content block type "+blockType)
			}
		}
	}
	return nil
}

func containsString(values []any, want string) bool {
	for _, value := range values {
		if strOpt(value) == want {
			return true
		}
	}
	return false
}

// writeUnsupportedConversionError uses the client's native HTTP error envelope.
// Request-shape failures are JSON even when stream:true because no stream began.
func writeUnsupportedConversionError(w http.ResponseWriter, proto string, err *unsupportedConversionError) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	var body any
	if proto == "anthropic" {
		body = map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    "invalid_request_error",
				"message": err.Error(),
			},
		}
	} else {
		body = map[string]any{
			"error": map[string]any{
				"type":    "invalid_request_error",
				"code":    "unsupported_protocol_conversion",
				"message": err.Error(),
			},
		}
	}
	encoded, _ := sonic.Marshal(body)
	_, _ = w.Write(encoded)
}
