package protocol

// convert_golden_test.go — golden-file replay: every *.sse stream under
// testdata/wire/ (named <proto>_<scenario>.sse, in the SOURCE protocol's wire
// format) is fed through ALL converters sourced from that protocol, asserting
// INVARIANTS rather than exact output (real-traffic replays can't use exact
// assertions):
//
//   - no panic; non-empty output
//   - output parses into SSE frames; no empty data frames; every data payload
//     is valid JSON (or the [DONE] marker)
//   - exactly ONE terminal event for the target protocol (message_stop XOR
//     error for anthropic; finish-chunk XOR error-chunk for chat;
//     completed/incomplete/failed for responses)
//   - responses targets: output_item.added/done pair by id+type (skipped for
//     failed terminals — items legitimately stay open across a failure)
//   - visible assistant text is byte-preserved, and positive terminal usage is
//     preserved after normalizing each protocol's input/cache token buckets
//
// Seeds are hand-written high-fidelity streams; `model-proxy wire record`
// captures real upstream streams into the same directory.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sonic "github.com/bytedance/sonic"
)

// goldenTarget is one converted stream plus the protocol it was converted TO
// (drives the terminal-event rule).
type goldenTarget struct {
	name   string
	target string
	out    io.Reader
}

// wireReplayTargets returns every converter sourced from src fed with r.
// nil for an unknown source protocol.
func wireReplayTargets(src string, r func() io.Reader, model string) []goldenTarget {
	switch src {
	case "anthropic":
		return []goldenTarget{
			{"anthropic→chat", "chat", newAnthropicToOpenAISSE(r(), model)},
			{"anthropic→responses", "responses", newAnthropicToResponsesSSE(r(), model)},
		}
	case "chat":
		return []goldenTarget{
			{"chat→anthropic", "anthropic", newOpenAIToAnthropicSSE(r(), model)},
			{"chat→responses", "responses", newOpenAIToResponsesSSE(r(), model)},
		}
	case "responses":
		return []goldenTarget{
			{"responses→anthropic", "anthropic", newResponsesToAnthropicSSE(r(), model)},
			{"responses→chat", "chat", newResponsesToOpenAISSE(r(), model)},
		}
	}
	return nil
}

func TestConvertGolden_Replay(t *testing.T) {
	const wireDir = "../../testdata/wire"
	entries, err := os.ReadDir(wireDir)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sse") {
			continue
		}
		n++
		src, _, _ := strings.Cut(e.Name(), "_")
		data, err := os.ReadFile(filepath.Join(wireDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		targets := wireReplayTargets(src, func() io.Reader { return strings.NewReader(string(data)) }, "seed-model")
		if targets == nil {
			t.Errorf("%s: unknown source protocol prefix %q", e.Name(), src)
			continue
		}
		for _, tgt := range targets {
			t.Run(e.Name()+"/"+tgt.name, func(t *testing.T) {
				raw, err := io.ReadAll(tgt.out)
				if err != nil {
					t.Fatalf("read: %v", err)
				}
				assertReplayInvariants(t, string(raw), tgt.target)
				assertScenarioSurvival(t, e.Name(), src, tgt.target, string(data), string(raw))
				assertGoldenSemanticParity(t, e.Name(), src, tgt.target, string(data), string(raw))
			})
		}
	}
	if n == 0 {
		t.Fatal("testdata/wire holds no .sse seeds")
	}
}

func TestGoldenResponsesSemanticExtractionSnapshots(t *testing.T) {
	doneOnly := "event: response.output_item.done\n" +
		`data: {"type":"response.output_item.done","item":{"id":"rs-1","type":"reasoning","summary":[{"type":"summary_text","text":"done thought"}]}}` + "\n\n" +
		"event: response.output_item.done\n" +
		`data: {"type":"response.output_item.done","item":{"id":"msg-1","type":"message","content":[{"type":"output_text","text":"done answer"}]}}` + "\n\n"
	completedOnly := "event: response.completed\n" +
		`data: {"type":"response.completed","response":{"status":"completed","output":[{"id":"rs-2","type":"reasoning","summary":[],"content":[{"type":"reasoning_text","text":"completed thought"}]},{"id":"msg-2","type":"message","content":[{"type":"output_text","text":"completed answer"}]}]}}` + "\n\n"
	completedSummaryAndContent := "event: response.completed\n" +
		`data: {"type":"response.completed","response":{"status":"completed","output":[{"id":"rs-2b","type":"reasoning","summary":[{"type":"summary_text","text":"summary thought"}],"content":[{"type":"reasoning_text","text":"duplicate content thought"}]},{"id":"msg-2b","type":"message","content":[{"type":"output_text","text":"summary answer"}]}]}}` + "\n\n"
	deltasAndSnapshots := "event: response.reasoning_summary_text.delta\n" +
		`data: {"type":"response.reasoning_summary_text.delta","delta":"delta thought"}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"delta answer"}` + "\n\n" +
		"event: response.output_item.done\n" +
		`data: {"type":"response.output_item.done","item":{"id":"rs-3","type":"reasoning","summary":[{"type":"summary_text","text":"done snapshot thought"}]}}` + "\n\n" +
		"event: response.output_item.done\n" +
		`data: {"type":"response.output_item.done","item":{"id":"msg-3","type":"message","content":[{"type":"output_text","text":"done snapshot answer"}]}}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"status":"completed","output":[{"id":"rs-3","type":"reasoning","summary":[{"type":"summary_text","text":"completed snapshot thought"}]},{"id":"msg-3","type":"message","content":[{"type":"output_text","text":"completed snapshot answer"}]}]}}` + "\n\n"

	tests := []struct {
		name, raw, wantText, wantReasoning string
	}{
		{"output_item.done fallback", doneOnly, "done answer", "done thought"},
		{"response.completed fallback", completedOnly, "completed answer", "completed thought"},
		{"summary wins content without duplication", completedSummaryAndContent, "summary answer", "summary thought"},
		{"deltas override snapshots", deltasAndSnapshots, "delta answer", "delta thought"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotText := goldenVisibleText(t, tt.raw, "responses")
			gotReasoning := goldenReasoningText(t, tt.raw, "responses")
			if gotText == "" || gotReasoning == "" {
				t.Fatalf("snapshot semantic extraction was empty: text=%q reasoning=%q", gotText, gotReasoning)
			}
			if gotText != tt.wantText {
				t.Errorf("visible text = %q, want %q", gotText, tt.wantText)
			}
			if gotReasoning != tt.wantReasoning {
				t.Errorf("reasoning text = %q, want %q", gotReasoning, tt.wantReasoning)
			}
		})
	}
}

func TestConvertGolden_ErrorFixtures(t *testing.T) {
	const wireDir = "../../testdata/wire"
	entries, err := os.ReadDir(wireDir)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".err") {
			continue
		}
		count++
		raw, err := os.ReadFile(filepath.Join(wireDir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		head, body, ok := strings.Cut(string(raw), "\n\n")
		if !ok {
			body = ""
		}
		status := 0
		if _, err := fmt.Sscanf(strings.TrimSpace(head), "status: %d", &status); err != nil || status < 400 {
			t.Fatalf("%s: invalid status header %q", entry.Name(), head)
		}
		prefix, _, _ := strings.Cut(entry.Name(), "_")
		source := prefix
		if source == "chat" {
			source = "openai"
		}
		for _, target := range []string{"anthropic", "openai", "responses"} {
			if target == source {
				continue
			}
			t.Run(entry.Name()+"/"+target, func(t *testing.T) {
				converted, err := convertErrorResponse([]byte(body), target, source, status)
				var envelope map[string]any
				parseErr := sonic.UnmarshalString(body, &envelope)
				recognized := parseErr == nil && asMap(envelope["error"]) != nil
				if !recognized {
					if err == nil {
						t.Fatalf("unrecognized error fixture converted successfully: %s", converted)
					}
					return
				}
				if err != nil {
					t.Fatalf("recognized error fixture failed conversion: %v", err)
				}
				out := unmarshalMap(t, converted)
				if asMap(out["error"]) == nil {
					t.Fatalf("converted body has no error envelope: %s", converted)
				}
				if target == "anthropic" && out["type"] != "error" {
					t.Fatalf("anthropic error type = %#v", out["type"])
				}
			})
		}
	}
	if count == 0 {
		t.Fatal("testdata/wire holds no .err fixtures")
	}
}

// assertReplayInvariants checks the golden-replay invariants on one converted
// stream. target ∈ anthropic | chat | responses.
func assertReplayInvariants(t *testing.T, raw, target string) {
	t.Helper()
	if raw == "" {
		t.Fatal("converter produced no output")
	}
	events := parseSSE(raw)
	if len(events) == 0 {
		t.Fatal("output parses to zero SSE frames")
	}
	for _, ev := range events {
		if ev.data == "" {
			t.Fatalf("empty data frame in output:\n%s", raw)
		}
		if ev.data == "[DONE]" {
			continue
		}
		var v any
		if err := sonic.UnmarshalString(ev.data, &v); err != nil {
			t.Fatalf("data frame is not valid JSON: %q", ev.data)
		}
	}

	switch target {
	case "anthropic":
		stops, errs := sseCount(events, "message_stop"), sseCount(events, "error")
		if stops+errs != 1 {
			t.Errorf("terminal events: message_stop=%d error=%d, want exactly one:\n%s", stops, errs, raw)
		}
	case "chat":
		finish, errs := 0, 0
		for _, ev := range events {
			if ev.data == "[DONE]" {
				continue
			}
			m := unmarshalMap(t, []byte(ev.data))
			if asMap(m["error"]) != nil {
				errs++
			}
			if choices, ok := m["choices"].([]any); ok {
				for _, c := range choices {
					if fr, ok := asMap(c)["finish_reason"].(string); ok && fr != "" {
						finish++
					}
				}
			}
		}
		if finish+errs != 1 {
			t.Errorf("terminal events: finish=%d error=%d, want exactly one:\n%s", finish, errs, raw)
		}
	case "responses":
		// Same caliber as the fuzz invariants: a bare `error` event is also a
		// terminal — a converter emitting error + completed would otherwise
		// pass this count.
		term := sseCount(events, "response.completed") + sseCount(events, "response.incomplete") +
			sseCount(events, "response.failed") + sseCount(events, "error")
		if term != 1 {
			t.Errorf("terminal events: completed+incomplete+failed+error = %d, want 1:\n%s", term, raw)
		}
		// added/done pairing only when the stream ended cleanly (a failed
		// stream legitimately leaves items open).
		if sseCount(events, "response.failed") == 0 {
			assertResponsesItemPairing(t, events)
		}
	}
}

// assertScenarioSurvival checks scenario fidelity on real recorded streams:
// when the INPUT stream actually contains a tool call or reasoning, the
// converted output must keep it in the target protocol's shape. Gated on
// input markers so recordings where the model didn't comply (no tool call
// despite tool_choice) are skipped instead of failing spuriously.
func assertScenarioSurvival(t *testing.T, file, src, target, input, output string) {
	t.Helper()
	// Match on whitespace-stripped text: real providers format SSE JSON
	// differently (zhipu writes `"type": "text_delta"` with spaces).
	strip := func(s string) string {
		return strings.Map(func(r rune) rune {
			switch r {
			case ' ', '\t', '\n', '\r':
				return -1
			}
			return r
		}, s)
	}
	in, out := strip(input), strip(output)
	containsAny := func(s string, markers []string) bool {
		for _, m := range markers {
			if strings.Contains(s, m) {
				return true
			}
		}
		return false
	}
	// Precise markers: bare "tool_use" would false-positive on usage's
	// "server_tool_use"; "tool_calls" alone matches OpenRouter's null/[] fields.
	toolOf := map[string]string{"anthropic": `"type":"tool_use"`, "chat": `"tool_calls":[{`, "responses": `"type":"function_call"`}
	if containsAny(in, []string{`"type":"tool_use"`, `"tool_calls":[{`, `"type":"function_call"`}) &&
		!strings.Contains(out, toolOf[target]) {
		t.Errorf("%s: input carries a tool call but the %s output lost it", file, target)
	}
	// Precise markers, same discipline as toolOf above: bare "thinking" would
	// false-positive when the visible text merely mentions the word. The lists
	// cover every shape our converters emit for each target protocol.
	thinkOf := map[string][]string{
		"anthropic": {`"type":"thinking"`, `"type":"thinking_delta"`, `"type":"redacted_thinking"`},
		"chat":      {`"reasoning_content":`},
		"responses": {`"type":"reasoning"`, `"reasoning_summary"`},
	}
	if containsAny(in, []string{`"type":"thinking"`, "thinking_delta", `"reasoning_content":"`, `"reasoning":"`, `"type":"reasoning"`, "reasoning_summary", "reasoning_text"}) &&
		!containsAny(out, thinkOf[target]) {
		t.Errorf("%s: input carries reasoning but the %s output lost it", file, target)
	}
}

type goldenUsage struct {
	input  int
	output int
}

// assertGoldenSemanticParity checks values rather than only wire shape. Token
// inputs are normalized to the inclusive OpenAI convention so Anthropic's
// separate cache-read/cache-creation buckets compare across all directions.
func assertGoldenSemanticParity(t *testing.T, file, src, target, input, output string) {
	t.Helper()
	wantText := goldenVisibleText(t, input, src)
	if wantText != "" {
		if gotText := goldenVisibleText(t, output, target); gotText != wantText {
			t.Errorf("%s: visible text changed in %s→%s\nwant: %q\n got: %q", file, src, target, wantText, gotText)
		}
	}
	wantReasoning := goldenReasoningText(t, input, src)
	if wantReasoning != "" {
		if gotReasoning := goldenReasoningText(t, output, target); gotReasoning != wantReasoning {
			t.Errorf("%s: reasoning text changed in %s→%s\nwant: %q\n got: %q", file, src, target, wantReasoning, gotReasoning)
		}
	}

	wantUsage := goldenTerminalUsage(t, input, src)
	if wantUsage.input > 0 || wantUsage.output > 0 {
		if gotUsage := goldenTerminalUsage(t, output, target); gotUsage != wantUsage {
			t.Errorf("%s: normalized usage changed in %s→%s: got input/output %d/%d, want %d/%d",
				file, src, target, gotUsage.input, gotUsage.output, wantUsage.input, wantUsage.output)
		}
	}
}

func goldenVisibleText(t *testing.T, raw, protocol string) string {
	t.Helper()
	if protocol == "responses" {
		return goldenResponsesSemanticText(t, raw, false)
	}
	var text strings.Builder
	for _, event := range parseSSE(raw) {
		if event.data == "[DONE]" {
			continue
		}
		data := unmarshalMap(t, []byte(event.data))
		switch protocol {
		case "anthropic":
			switch strOpt(data["type"]) {
			case "content_block_start":
				block := asMap(data["content_block"])
				if strOpt(block["type"]) == "text" {
					text.WriteString(strOpt(block["text"]))
				}
			case "content_block_delta":
				delta := asMap(data["delta"])
				if strOpt(delta["type"]) == "text_delta" {
					text.WriteString(strOpt(delta["text"]))
				}
			}
		case "chat":
			for _, rawChoice := range anySlice(data["choices"]) {
				choice := asMap(rawChoice)
				goldenAppendText(&text, asMap(choice["delta"])["content"])
			}
		default:
			t.Fatalf("unknown golden protocol %q", protocol)
		}
	}
	return text.String()
}

func goldenAppendText(dst *strings.Builder, value any) {
	switch content := value.(type) {
	case string:
		dst.WriteString(content)
	case []any:
		for _, rawPart := range content {
			part := asMap(rawPart)
			if typ := strOpt(part["type"]); typ == "text" || typ == "output_text" {
				dst.WriteString(strOpt(part["text"]))
			}
		}
	}
}

func goldenReasoningText(t *testing.T, raw, protocol string) string {
	t.Helper()
	if protocol == "responses" {
		return goldenResponsesSemanticText(t, raw, true)
	}
	var text strings.Builder
	for _, event := range parseSSE(raw) {
		if event.data == "[DONE]" {
			continue
		}
		data := unmarshalMap(t, []byte(event.data))
		switch protocol {
		case "anthropic":
			switch strOpt(data["type"]) {
			case "content_block_start":
				block := asMap(data["content_block"])
				if strOpt(block["type"]) == "thinking" {
					text.WriteString(strOpt(block["thinking"]))
				}
			case "content_block_delta":
				delta := asMap(data["delta"])
				if strOpt(delta["type"]) == "thinking_delta" {
					text.WriteString(strOpt(delta["thinking"]))
				}
			}
		case "chat":
			for _, rawChoice := range anySlice(data["choices"]) {
				delta := asMap(asMap(rawChoice)["delta"])
				if value := strOpt(delta["reasoning_content"]); value != "" {
					text.WriteString(value)
					continue
				}
				if value := goldenReasoningValue(delta["reasoning"]); value != "" {
					text.WriteString(value)
					continue
				}
				text.WriteString(goldenReasoningDetails(delta["reasoning_details"]))
			}
		default:
			t.Fatalf("unknown golden protocol %q", protocol)
		}
	}
	return text.String()
}

// goldenResponsesSemanticText reads semantic text from either streaming
// deltas or the two Responses snapshot locations. Deltas are authoritative;
// snapshots are only a fallback for valid streams that emit no text deltas.
// response.completed.output supersedes output_item.done when both snapshots
// are populated, avoiding double-counting the same completed item.
func goldenResponsesSemanticText(t *testing.T, raw string, reasoning bool) string {
	t.Helper()
	var deltas, doneSnapshots strings.Builder
	sawDelta := false
	completedSnapshot := ""
	for _, event := range parseSSE(raw) {
		if event.data == "[DONE]" {
			continue
		}
		data := unmarshalMap(t, []byte(event.data))
		switch sseEventType(event) {
		case "response.output_text.delta":
			if !reasoning {
				sawDelta = true
				deltas.WriteString(strOpt(data["delta"]))
			}
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			if reasoning {
				sawDelta = true
				deltas.WriteString(strOpt(data["delta"]))
			}
		case "response.output_item.done":
			if item := asMap(data["item"]); item != nil {
				doneSnapshots.WriteString(goldenResponsesItemsText([]map[string]any{item}, reasoning))
			}
		case "response.completed":
			if response := asMap(data["response"]); response != nil {
				if snapshot := goldenResponsesItemsText(responsesOutputItems(response), reasoning); snapshot != "" {
					completedSnapshot = snapshot
				}
			}
		}
	}
	if sawDelta {
		return deltas.String()
	}
	if completedSnapshot != "" {
		return completedSnapshot
	}
	return doneSnapshots.String()
}

func goldenResponsesItemsText(items []map[string]any, reasoning bool) string {
	var text strings.Builder
	for _, item := range items {
		if reasoning {
			if strOpt(item["type"]) == "reasoning" {
				value, _ := responsesReasoningText(item)
				text.WriteString(value)
			}
			continue
		}
		switch strOpt(item["type"]) {
		case "message":
			goldenAppendText(&text, item["content"])
		case "output_text", "text":
			text.WriteString(strOpt(item["text"]))
		}
	}
	return text.String()
}

func goldenReasoningValue(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	item := asMap(value)
	return firstNonEmpty(strOpt(item["content"]), strOpt(item["text"]), strOpt(item["summary"]))
}

func goldenReasoningDetails(value any) string {
	items := anySlice(value)
	if item := asMap(value); item != nil {
		items = []any{item}
	}
	var parts []string
	for _, rawItem := range items {
		if text := goldenReasoningValue(rawItem); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n\n")
}

func goldenTerminalUsage(t *testing.T, raw, protocol string) goldenUsage {
	t.Helper()
	var input, cacheRead, cacheCreate, output int
	for _, event := range parseSSE(raw) {
		if event.data == "[DONE]" {
			continue
		}
		data := unmarshalMap(t, []byte(event.data))
		var usage map[string]any
		switch protocol {
		case "anthropic":
			usage = asMap(data["usage"])
			if message := asMap(data["message"]); message != nil {
				if startUsage := asMap(message["usage"]); startUsage != nil {
					usage = startUsage
				}
			}
			goldenUpdateInt(usage, "input_tokens", &input)
			goldenUpdateInt(usage, "cache_read_input_tokens", &cacheRead)
			goldenUpdateInt(usage, "cache_creation_input_tokens", &cacheCreate)
			goldenUpdateInt(usage, "output_tokens", &output)
		case "chat":
			usage = asMap(data["usage"])
			goldenUpdateInt(usage, "prompt_tokens", &input)
			goldenUpdateInt(usage, "completion_tokens", &output)
		case "responses":
			if response := asMap(data["response"]); response != nil {
				usage = asMap(response["usage"])
			}
			if usage == nil {
				usage = asMap(data["usage"])
			}
			goldenUpdateInt(usage, "input_tokens", &input)
			goldenUpdateInt(usage, "output_tokens", &output)
		default:
			t.Fatalf("unknown golden protocol %q", protocol)
		}
	}
	return goldenUsage{input: input + cacheRead + cacheCreate, output: output}
}

func goldenUpdateInt(values map[string]any, key string, target *int) {
	if values == nil {
		return
	}
	switch value := values[key].(type) {
	case float64:
		*target = int(value)
	case int:
		*target = value
	case int64:
		*target = int(value)
	}
}
