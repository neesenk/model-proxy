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
			})
		}
	}
	if n == 0 {
		t.Fatal("testdata/wire holds no .sse seeds")
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
		term := sseCount(events, "response.completed") + sseCount(events, "response.incomplete") + sseCount(events, "response.failed")
		if term != 1 {
			t.Errorf("terminal events: completed+incomplete+failed = %d, want 1:\n%s", term, raw)
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
//
// Exception: anthropic→chat drops thinking by design (protocol-conversion.md
// 有损字段; a↔chat replay cache 未实现), so that direction is exempt from the
// reasoning check — tool calls survive ALL directions.
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
	if src == "anthropic" && target == "chat" {
		return // thinking a→chat is documented-lossy
	}
	thinkOf := map[string]string{"anthropic": "thinking", "chat": "reasoning_content", "responses": "reasoning"}
	if containsAny(in, []string{`"type":"thinking"`, "thinking_delta", `"reasoning_content":"`, `"reasoning":"`, `"type":"reasoning"`, "reasoning_summary", "reasoning_text"}) &&
		!strings.Contains(out, thinkOf[target]) {
		t.Errorf("%s: input carries reasoning but the %s output lost it", file, target)
	}
}
