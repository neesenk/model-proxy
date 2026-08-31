package protocol

import (
	"io"
	"strings"
	"testing"
)

func TestConvertDocumentAndInputFileAcrossProtocols(t *testing.T) {
	anthropic := []byte(`{"model":"m","max_tokens":32,"messages":[{"role":"user","content":[
		{"type":"document","title":"guide.pdf","source":{"type":"base64","media_type":"application/pdf","data":"cGRm"}},
		{"type":"document","title":"remote.pdf","source":{"type":"url","url":"https://example.test/remote.pdf"}}
	]}]}`)

	responsesRaw, err := convertAnthropicRequestToResponses(anthropic)
	if err != nil {
		t.Fatal(err)
	}
	responses := unmarshalMap(t, responsesRaw)
	items := anySlice(responses["input"])
	content := anySlice(asMap(items[0])["content"])
	if got := strOpt(asMap(content[0])["file_data"]); got != "data:application/pdf;base64,cGRm" {
		t.Fatalf("a→r file_data = %q", got)
	}
	if got := strOpt(asMap(content[1])["file_url"]); got != "https://example.test/remote.pdf" {
		t.Fatalf("a→r file_url = %q", got)
	}

	anthropicRaw, err := convertResponsesRequestToAnthropic([]byte(`{"model":"m","input":[{"type":"message","role":"user","content":[
		{"type":"input_file","filename":"id.pdf","file_id":"file_1"},
		{"type":"input_file","filename":"inline.pdf","file_data":"data:application/pdf;base64,cGRm"},
		{"type":"input_file","filename":"url.pdf","file_url":"https://example.test/url.pdf"}
	]}]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	anthropicOut := unmarshalMap(t, anthropicRaw)
	blocks := anySlice(asMap(anySlice(anthropicOut["messages"])[0])["content"])
	// A file-id-only attachment degrades to an observable text note: file_id is
	// provider-scoped and would 404/400 at a different upstream. Inline and
	// URL sources are unaffected and keep their document blocks.
	if got := strOf(asMap(blocks[0])["type"]); got != "text" || !strings.Contains(strOf(asMap(blocks[0])["text"]), "file_1") {
		t.Fatalf("r→a file_id degrade note = %#v", blocks[0])
	}
	if got := strOf(asMap(asMap(blocks[1])["source"])["data"]); got != "cGRm" {
		t.Fatalf("r→a base64 data = %q", got)
	}
	if got := strOf(asMap(asMap(blocks[2])["source"])["url"]); got != "https://example.test/url.pdf" {
		t.Fatalf("r→a URL = %q", got)
	}

	chatRaw, err := convertResponsesRequestToOpenAI([]byte(`{"model":"m","input":[{"type":"message","role":"user","content":[
		{"type":"input_text","text":"read"},
		{"type":"input_file","filename":"inline.pdf","file_data":"data:application/pdf;base64,cGRm"}
	]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	chat := unmarshalMap(t, chatRaw)
	chatParts := anySlice(asMap(anySlice(chat["messages"])[0])["content"])
	filePart := asMap(chatParts[1])
	if filePart["type"] != "file" || strOpt(asMap(filePart["file"])["file_data"]) == "" {
		t.Fatalf("r→chat file part = %#v", filePart)
	}

	backRaw, err := convertOpenAIRequestToAnthropic([]byte(`{"model":"m","messages":[{"role":"user","content":[
		{"type":"file","file":{"filename":"inline.pdf","file_data":"data:application/pdf;base64,cGRm"}}
	]}]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	back := unmarshalMap(t, backRaw)
	backBlock := asMap(anySlice(asMap(anySlice(back["messages"])[0])["content"])[0])
	if backBlock["type"] != "document" || strOpt(asMap(backBlock["source"])["data"]) != "cGRm" {
		t.Fatalf("chat→a document = %#v", backBlock)
	}

	// chat→r: Chat nests file fields under "file"; they must land flat on the
	// responses input_file part (regression: nested fields were silently dropped).
	chatToResponsesRaw, err := convertOpenAIRequestToResponses([]byte(`{"model":"m","messages":[{"role":"user","content":[
		{"type":"file","file":{"filename":"inline.pdf","file_data":"data:application/pdf;base64,cGRm"}}
	]}]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	chatToResponses := unmarshalMap(t, chatToResponsesRaw)
	rItems := anySlice(chatToResponses["input"])
	rFile := asMap(anySlice(asMap(rItems[0])["content"])[0])
	if rFile["type"] != "input_file" || strOpt(rFile["file_data"]) != "data:application/pdf;base64,cGRm" {
		t.Fatalf("chat→r nested file part = %#v", rFile)
	}
}

func TestConvertHostedToolResponsesSSE(t *testing.T) {
	stream := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"r1","model":"m","status":"in_progress"}}` + "\n\n" +
		"event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"web_search_call","id":"ws_1","status":"in_progress"}}` + "\n\n" +
		"event: response.output_item.done\n" +
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"web_search_call","id":"ws_1","status":"completed","action":{"query":"news"},"sources":[{"title":"N","url":"https://example.test/n"}]}}` + "\n\n" +
		"event: response.output_item.done\n" +
		`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"tool_search_call","id":"ts_1","call_id":"ts_1","status":"completed","arguments":{"query":"calendar"}}}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"r1","model":"m","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n"

	anthropic, err := io.ReadAll(newResponsesToAnthropicSSE(strings.NewReader(stream), "m"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"type":"server_tool_use"`, `"type":"web_search_tool_result"`, `"name":"tool_search"`} {
		if !strings.Contains(string(anthropic), want) {
			t.Fatalf("r→a hosted SSE missing %s:\n%s", want, anthropic)
		}
	}

	chat, err := io.ReadAll(newResponsesToOpenAISSE(strings.NewReader(stream), "m"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"name":"web_search"`, `"name":"tool_search"`, `"finish_reason":"tool_calls"`} {
		if !strings.Contains(string(chat), want) {
			t.Fatalf("r→chat hosted SSE missing %s:\n%s", want, chat)
		}
	}
}

func TestConvertToolResultErrorMarkerRoundTrip(t *testing.T) {
	in := []byte(`{"model":"m","max_tokens":32,"messages":[
		{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"f","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","is_error":true,"content":"failed"}]}
	]}`)
	responsesRaw, err := convertAnthropicRequestToResponses(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(responsesRaw), toolResultErrorMarker) {
		t.Fatalf("a→r missing error marker: %s", responsesRaw)
	}
	backRaw, err := convertResponsesRequestToAnthropic(responsesRaw, nil)
	if err != nil {
		t.Fatal(err)
	}
	back := unmarshalMap(t, backRaw)
	var result map[string]any
	for _, msg := range anySlice(back["messages"]) {
		for _, block := range anySlice(asMap(msg)["content"]) {
			if b := asMap(block); b["type"] == "tool_result" {
				result = b
			}
		}
	}
	if result == nil || result["is_error"] != true || result["content"] != "failed" {
		t.Fatalf("r→a error result = %#v", result)
	}

	chatRaw, err := convertAnthropicRequestToOpenAI(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(chatRaw), toolResultErrorMarker) {
		t.Fatalf("a→chat missing error marker: %s", chatRaw)
	}
	backChatRaw, err := convertOpenAIRequestToAnthropic(chatRaw, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(backChatRaw), `"is_error":true`) {
		t.Fatalf("chat→a lost is_error: %s", backChatRaw)
	}
}

func TestNormalizeAnthropicInputSchema(t *testing.T) {
	raw, err := convertResponsesRequestToAnthropic([]byte(`{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"x"}]}],"tools":[{
		"type":"function","name":"f","parameters":{"oneOf":[
			{"type":"object","properties":{"a":{"type":"string","encrypted":true}}},
			{"type":"object","properties":{"encrypted":{"type":"string"}}}
		],"encrypted":true}
	}]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	out := unmarshalMap(t, raw)
	schema := asMap(asMap(anySlice(out["tools"])[0])["input_schema"])
	if schema["type"] != "object" || schema["properties"] == nil {
		t.Fatalf("normalized schema = %#v", schema)
	}
	if _, ok := schema["oneOf"]; ok {
		t.Fatalf("root composition not flattened: %#v", schema)
	}
	if _, ok := schema["encrypted"]; ok {
		t.Fatalf("transport marker not stripped: %#v", schema)
	}
	if _, ok := asMap(schema["properties"])["encrypted"]; !ok {
		t.Fatalf("property named encrypted was stripped: %#v", schema)
	}
}

func TestConvertHostedToolsAndCalls(t *testing.T) {
	request := []byte(`{"model":"m","input":[
		{"type":"tool_search_call","id":"ts_1","call_id":"ts_1","arguments":{"query":"calendar","limit":3}},
		{"type":"tool_search_output","call_id":"ts_1","output":"found"}
	],"tools":[{"type":"web_search"},{"type":"tool_search"}],"tool_choice":{"type":"tool_search"}}`)
	anthropicRaw, err := convertResponsesRequestToAnthropic(request, nil)
	if err != nil {
		t.Fatal(err)
	}
	anthropic := unmarshalMap(t, anthropicRaw)
	tools := anySlice(anthropic["tools"])
	if len(tools) != 2 || !strings.HasPrefix(strOpt(asMap(tools[0])["type"]), "web_search") || strOpt(asMap(tools[1])["name"]) != "tool_search" {
		t.Fatalf("r→a hosted tools = %#v", tools)
	}
	if got := strOpt(asMap(anthropic["tool_choice"])["name"]); got != "tool_search" {
		t.Fatalf("r→a tool choice = %q", got)
	}

	chatRaw, err := convertResponsesRequestToOpenAI(request)
	if err != nil {
		t.Fatal(err)
	}
	chat := unmarshalMap(t, chatRaw)
	chatTools := anySlice(chat["tools"])
	if len(chatTools) != 2 {
		t.Fatalf("r→chat hosted tools = %#v", chatTools)
	}
	msgs := anySlice(chat["messages"])
	if len(msgs) < 2 || strOpt(asMap(asMap(anySlice(asMap(msgs[0])["tool_calls"])[0])["function"])["name"]) != "tool_search" {
		t.Fatalf("r→chat tool_search history = %#v", msgs)
	}

	response := []byte(`{"id":"r1","status":"completed","output":[
		{"type":"web_search_call","id":"ws_1","status":"completed","action":{"query":"weather"},"sources":[{"title":"Forecast","url":"https://example.test/f"}]},
		{"type":"tool_search_call","id":"ts_1","call_id":"ts_1","arguments":{"query":"calendar"}}
	]}`)
	anthropicRespRaw, err := convertResponsesToAnthropic(response)
	if err != nil {
		t.Fatal(err)
	}
	anthropicResp := unmarshalMap(t, anthropicRespRaw)
	blocks := anySlice(anthropicResp["content"])
	if len(blocks) != 3 || asMap(blocks[0])["type"] != "server_tool_use" || asMap(blocks[1])["type"] != "web_search_tool_result" || strOpt(asMap(blocks[2])["name"]) != "tool_search" {
		t.Fatalf("r→a hosted response = %#v", blocks)
	}

	backRaw, err := convertAnthropicResponseToResponses(anthropicRespRaw)
	if err != nil {
		t.Fatal(err)
	}
	back := unmarshalMap(t, backRaw)
	var sawWeb bool
	for _, raw := range anySlice(back["output"]) {
		if asMap(raw)["type"] == "web_search_call" {
			sawWeb = true
		}
	}
	if !sawWeb {
		t.Fatalf("a→r web search response missing: %s", backRaw)
	}
}

func TestResponsesToolSearchOutputLoadsDiscoveredTools(t *testing.T) {
	request := []byte(`{"model":"m","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"use a discovered tool"}]},
		{"type":"tool_search_call","id":"ts_1","call_id":"ts_1","arguments":{"query":"calendar"}},
		{"type":"tool_search_output","call_id":"ts_1","status":"completed","tools":[
			{"type":"function","name":"calendar_create","description":"Create event","parameters":{"type":"object","properties":{"title":{"type":"string"}}}},
			{"type":"namespace","name":"mcp__files","tools":[
				{"type":"function","name":"read","description":"Read file","parameters":{"type":"object"}}
			]}
		]}
	],"tools":[{"type":"tool_search"}]}`)

	anthropicRaw, err := convertResponsesRequestToAnthropic(request, nil)
	if err != nil {
		t.Fatal(err)
	}
	anthropic := unmarshalMap(t, anthropicRaw)
	anthropicTools := anySlice(anthropic["tools"])
	var foundCalendar bool
	for _, raw := range anthropicTools {
		if strOpt(asMap(raw)["name"]) == "calendar_create" {
			foundCalendar = true
		}
	}
	if !foundCalendar {
		t.Fatalf("r→a did not materialize discovered function: %s", anthropicRaw)
	}
	var anthropicResult map[string]any
	for _, rawMessage := range anySlice(anthropic["messages"]) {
		for _, rawBlock := range anySlice(asMap(rawMessage)["content"]) {
			block := asMap(rawBlock)
			if block["type"] == "tool_result" {
				anthropicResult = block
			}
		}
	}
	if anthropicResult == nil ||
		!strings.Contains(strOpt(anthropicResult["content"]), "calendar_create") ||
		!strings.Contains(strOpt(anthropicResult["content"]), "mcp__files__read") ||
		strOpt(anthropicResult["content"]) == "null" ||
		anthropicResult["is_error"] != false {
		t.Fatalf("r→a tool search result = %#v", anthropicResult)
	}

	chatRaw, err := convertResponsesRequestToOpenAI(request)
	if err != nil {
		t.Fatal(err)
	}
	chat := unmarshalMap(t, chatRaw)
	var chatToolNames []string
	var foundChatCalendar, foundChatFile bool
	for _, raw := range anySlice(chat["tools"]) {
		name := strOpt(asMap(asMap(raw)["function"])["name"])
		chatToolNames = append(chatToolNames, name)
		foundChatCalendar = foundChatCalendar || name == "calendar_create"
		foundChatFile = foundChatFile || name == "mcp__files__read"
	}
	if !foundChatCalendar || !foundChatFile {
		t.Fatalf("r→chat discovered tools = %#v", chatToolNames)
	}
	var chatResult map[string]any
	for _, raw := range anySlice(chat["messages"]) {
		message := asMap(raw)
		if message["role"] == "tool" {
			chatResult = message
		}
	}
	if chatResult == nil ||
		!strings.Contains(strOpt(chatResult["content"]), "calendar_create") ||
		!strings.Contains(strOpt(chatResult["content"]), "mcp__files__read") ||
		strOpt(chatResult["content"]) == "null" {
		t.Fatalf("r→chat tool search result = %#v", chatResult)
	}
}

func TestResponsesToolSearchOutputStatusAndToolChoice(t *testing.T) {
	failed := []byte(`{"model":"m","input":[
		{"type":"tool_search_call","call_id":"ts_fail","arguments":{"query":"missing"}},
		{"type":"tool_search_output","call_id":"ts_fail","status":"failed","tools":[]}
	],"tools":[{"type":"tool_search"}]}`)
	anthropicRaw, err := convertResponsesRequestToAnthropic(failed, nil)
	if err != nil {
		t.Fatal(err)
	}
	anthropic := unmarshalMap(t, anthropicRaw)
	var result map[string]any
	for _, rawMessage := range anySlice(anthropic["messages"]) {
		for _, rawBlock := range anySlice(asMap(rawMessage)["content"]) {
			if block := asMap(rawBlock); block["type"] == "tool_result" {
				result = block
			}
		}
	}
	if result == nil || result["is_error"] != true || !strings.Contains(strOpt(result["content"]), "failed") {
		t.Fatalf("failed r→a tool_search_output = %#v", result)
	}

	chatRaw, err := convertResponsesRequestToOpenAI(failed)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(chatRaw), toolResultErrorMarker) {
		t.Fatalf("failed r→chat tool_search_output lost error marker: %s", chatRaw)
	}

	choice := []byte(`{"model":"m","input":[
		{"type":"tool_search_output","call_id":"ts_1","status":"completed","tools":[
			{"type":"function","name":"calendar_create","parameters":{"type":"object"}}
		]}
	],"tool_choice":{"type":"function","name":"calendar_create"}}`)
	chatChoiceRaw, err := convertResponsesRequestToOpenAI(choice)
	if err != nil {
		t.Fatal(err)
	}
	chatChoice := unmarshalMap(t, chatChoiceRaw)
	if len(anySlice(chatChoice["tools"])) != 1 ||
		strOpt(asMap(asMap(chatChoice["tool_choice"])["function"])["name"]) != "calendar_create" {
		t.Fatalf("discovered-only tool choice = %s", chatChoiceRaw)
	}
}

func TestCrossProtocolAnthropicCacheBreakpoints(t *testing.T) {
	raw, err := convertRequestFor([]byte(`{"model":"m","messages":[
		{"role":"system","content":"rules"},
		{"role":"user","content":"first"},
		{"role":"assistant","content":"ok"},
		{"role":"user","content":"last"}
	],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]}`),
		"openai", "anthropic", convertReqOpts{ImageOK: true})
	if err != nil {
		t.Fatal(err)
	}
	out := unmarshalMap(t, raw)
	system := anySlice(out["system"])
	if asMap(asMap(system[len(system)-1])["cache_control"])["type"] != "ephemeral" {
		t.Fatalf("system cache breakpoint missing: %s", raw)
	}
	tools := anySlice(out["tools"])
	if asMap(asMap(tools[len(tools)-1])["cache_control"])["type"] != "ephemeral" {
		t.Fatalf("tool cache breakpoint missing: %s", raw)
	}
	msgs := anySlice(out["messages"])
	lastContent := anySlice(asMap(msgs[len(msgs)-1])["content"])
	if asMap(asMap(lastContent[len(lastContent)-1])["cache_control"])["type"] != "ephemeral" {
		t.Fatalf("last-user cache breakpoint missing: %s", raw)
	}
}

func TestCrossProtocolExplicitPromptCacheControls(t *testing.T) {
	anthropic := []byte(`{"model":"claude","max_tokens":32,
		"system":[{"type":"text","text":"stable rules","cache_control":{"type":"ephemeral"}}],
		"messages":[{"role":"user","content":"hello"}]}`)
	chatRaw, err := convertAnthropicRequestToOpenAI(anthropic)
	if err != nil {
		t.Fatal(err)
	}
	key := strOpt(unmarshalMap(t, chatRaw)["prompt_cache_key"])
	if len(key) != 32 {
		t.Fatalf("a→chat explicit cache key = %q", key)
	}
	chatRaw2, err := convertAnthropicRequestToOpenAI(anthropic)
	if err != nil {
		t.Fatal(err)
	}
	if strOpt(unmarshalMap(t, chatRaw2)["prompt_cache_key"]) != key {
		t.Fatal("a→chat explicit cache key is not deterministic")
	}
	withoutBreakpoint, err := convertAnthropicRequestToOpenAI([]byte(`{
		"model":"claude","max_tokens":32,"system":"stable rules",
		"messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if unmarshalMap(t, withoutBreakpoint)["prompt_cache_key"] != nil {
		t.Fatalf("a→chat synthesized cache key without explicit breakpoint: %s", withoutBreakpoint)
	}

	responses := []byte(`{"model":"gpt","input":"hello","prompt_cache_key":"cohort-1","prompt_cache_retention":"24h"}`)
	toChatRaw, err := convertResponsesRequestToOpenAI(responses)
	if err != nil {
		t.Fatal(err)
	}
	toChat := unmarshalMap(t, toChatRaw)
	if toChat["prompt_cache_key"] != "cohort-1" || toChat["prompt_cache_retention"] != "24h" {
		t.Fatalf("r→chat cache controls = %s", toChatRaw)
	}
	backRaw, err := convertOpenAIRequestToResponses(toChatRaw, nil)
	if err != nil {
		t.Fatal(err)
	}
	back := unmarshalMap(t, backRaw)
	if back["prompt_cache_key"] != "cohort-1" || back["prompt_cache_retention"] != "24h" {
		t.Fatalf("chat→r cache controls = %s", backRaw)
	}

	_, err = convertRequestFor(responses, "responses", "anthropic", convertReqOpts{ImageOK: true})
	unsupported, ok := asUnsupportedConversion(err)
	if !ok || unsupported.Feature != "cache_retention" {
		t.Fatalf("r→a 24h retention error = %#v", err)
	}
}

// r→chat: file_id + file_url together — the URL is the transportable form
// and passes through as the document note instead of degrading the whole
// attachment to a file_id remark (r→a prefers the URL the same way).
func TestResponsesToChatFileIDWithURLPrefersURL(t *testing.T) {
	chatRaw, err := convertResponsesRequestToOpenAI([]byte(`{"model":"m","input":[{"type":"message","role":"user","content":[
		{"type":"input_file","filename":"combo.pdf","file_id":"file_9","file_url":"https://example.test/combo.pdf"}
	]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	// A lone document note is a text part, so the converter folds the whole
	// content back to a plain string (chatContentText fast path).
	chat := unmarshalMap(t, chatRaw)
	text := strOf(asMap(anySlice(chat["messages"])[0])["content"])
	if !strings.Contains(text, "https://example.test/combo.pdf") || strings.Contains(text, "file_9") {
		t.Fatalf("file_id+file_url note = %q, want the URL without a file_id remark", text)
	}
}

// chat→r: file_id alongside file_url/file_data — the inline/URL source is the
// transportable form and survives on the cross-protocol input_file; only the
// provider-scoped file_id is dropped, observably (r→chat/r→a prefer the
// source the same way). A file-id-only attachment still degrades to the
// observable text note, as before.
func TestChatToResponsesFileIDWithSourceDropsID(t *testing.T) {
	d := NewDiagnostics()
	raw, err := convertOpenAIRequestToResponses([]byte(`{"model":"m","messages":[{"role":"user","content":[
		{"type":"file","file":{"filename":"combo.pdf","file_id":"file_9","file_url":"https://example.test/combo.pdf"}},
		{"type":"file","file":{"filename":"inline.pdf","file_id":"file_8","file_data":"data:application/pdf;base64,cGRm"}}
	]}]}`), d)
	if err != nil {
		t.Fatal(err)
	}
	body := unmarshalMap(t, raw)
	parts := anySlice(asMap(anySlice(body["input"])[0])["content"])
	if len(parts) != 2 {
		t.Fatalf("chat→r content parts = %#v, want 2", parts)
	}
	urlPart, dataPart := asMap(parts[0]), asMap(parts[1])
	if urlPart["type"] != "input_file" || strOpt(urlPart["file_url"]) != "https://example.test/combo.pdf" {
		t.Fatalf("file_id+file_url part = %#v, want input_file carrying the URL", urlPart)
	}
	if dataPart["type"] != "input_file" || strOpt(dataPart["file_data"]) != "data:application/pdf;base64,cGRm" {
		t.Fatalf("file_id+file_data part = %#v, want input_file carrying the inline data", dataPart)
	}
	for _, part := range []map[string]any{urlPart, dataPart} {
		if _, ok := part["file_id"]; ok {
			t.Fatalf("provider-scoped file_id leaked across protocols: %#v", part)
		}
	}
	if !d.HasCode("file_id_degraded") {
		t.Fatalf("file_id drop not observable: diagnostics = %#v", d.Items())
	}

	d2 := NewDiagnostics()
	raw2, err := convertOpenAIRequestToResponses([]byte(`{"model":"m","messages":[{"role":"user","content":[
		{"type":"file","file":{"filename":"id.pdf","file_id":"file_1"}}
	]}]}`), d2)
	if err != nil {
		t.Fatal(err)
	}
	body2 := unmarshalMap(t, raw2)
	part2 := asMap(anySlice(asMap(anySlice(body2["input"])[0])["content"])[0])
	if part2["type"] != "text" || !strings.Contains(strOf(part2["text"]), "file_1") {
		t.Fatalf("file_id-only degrade note = %#v", part2)
	}
	if !d2.HasCode("file_id_degraded") {
		t.Fatalf("file_id-only degrade not observable: diagnostics = %#v", d2.Items())
	}
}
