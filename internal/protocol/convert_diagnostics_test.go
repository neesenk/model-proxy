package protocol

import (
	"strings"
	"testing"
)

// TestConvertRequestDiagnostics: lossy-but-degradable request conversions
// collect structured diagnostics with stable codes alongside the legacy log.
func TestConvertRequestDiagnostics(t *testing.T) {
	diag := NewDiagnostics()
	body := []byte(`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"stop":"END"}`)
	out, err := ConvertRequestWithOptions(body, Protocol("openai"), Protocol("responses"),
		RequestOptions{ImageOK: true, Diag: diag})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"input"`) {
		t.Fatalf("conversion broken: %s", out)
	}
	if !diag.HasCode("stop_dropped") {
		t.Errorf("stop_dropped missing: %+v", diag.Items())
	}

	// chat→anthropic: a percent-encoded data URI degrades observably.
	diagA := NewDiagnostics()
	outA, err := ConvertRequestWithOptions(
		[]byte(`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":[
			{"type":"text","text":"hi"},
			{"type":"image_url","image_url":{"url":"data:text/html,%3Cb%3E"}}]}]}`),
		Protocol("openai"), Protocol("anthropic"),
		RequestOptions{ImageOK: true, Diag: diagA})
	if err != nil {
		t.Fatal(err)
	}
	_ = outA
	if !diagA.HasCode("data_uri_dropped") {
		t.Errorf("data_uri_dropped missing: %+v", diagA.Items())
	}
}

// TestConvertRequestStrictLossy: strict mode refuses lossy conversions
// through the capability-scanner channel (target skip + 400 envelope reuse).
func TestConvertRequestStrictLossy(t *testing.T) {
	diag := NewDiagnostics()
	_, err := ConvertRequestWithOptions(
		[]byte(`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"stop":"END"}`),
		Protocol("openai"), Protocol("responses"),
		RequestOptions{ImageOK: true, Diag: diag, StrictLossy: true})
	unsupported, ok := AsUnsupported(err)
	if !ok {
		t.Fatalf("strict lossy error = %v, want unsupported channel", err)
	}
	if !strings.HasPrefix(unsupported.Feature, "strict_lossy:") ||
		!strings.Contains(unsupported.Feature, "stop_dropped") {
		t.Fatalf("unsupported feature = %q", unsupported.Feature)
	}

	// A clean conversion passes strict mode untouched.
	clean, err := ConvertRequestWithOptions(
		[]byte(`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`),
		Protocol("openai"), Protocol("responses"),
		RequestOptions{ImageOK: true, Diag: NewDiagnostics(), StrictLossy: true})
	if err != nil {
		t.Fatalf("clean conversion refused under strict: %v", err)
	}
	if !strings.Contains(string(clean), `"input"`) {
		t.Fatalf("clean conversion broken: %s", clean)
	}
}

// The no-vision image collapse (tool-result images → placeholder text) is a
// lossy-but-degradable mapping and must be observable as media_degraded —
// and strict mode must refuse it like every other lossy degradation
// (previously the capability gate collapsed images with no diagnostic, so
// strict mode let the content degrade silently).
func TestConvertRequestMediaDegradedDiagnosticAndStrict(t *testing.T) {
	toolResultImage := `{"model":"m","max_tokens":16,"messages":[
		{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"shot","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":[
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="}}]}]}]}`

	// Non-strict: collapses with the diagnostic collected.
	diag := NewDiagnostics()
	out, err := ConvertRequestWithOptions([]byte(toolResultImage), Protocol("anthropic"), Protocol("openai"),
		RequestOptions{ImageOK: false, Diag: diag})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), mediaOmittedPlaceholder) {
		t.Fatalf("image collapse missing placeholder: %s", out)
	}
	if !diag.HasCode("media_degraded") {
		t.Errorf("media_degraded missing: %+v", diag.Items())
	}

	// Strict: refused through the unsupported channel.
	_, err = ConvertRequestWithOptions([]byte(toolResultImage), Protocol("anthropic"), Protocol("openai"),
		RequestOptions{ImageOK: false, Diag: NewDiagnostics(), StrictLossy: true})
	unsupported, ok := AsUnsupported(err)
	if !ok {
		t.Fatalf("strict image collapse error = %v, want unsupported channel", err)
	}
	if !strings.Contains(unsupported.Feature, "media_degraded") {
		t.Fatalf("unsupported feature = %q, want media_degraded", unsupported.Feature)
	}

	// StrictLossy without Diag no longer silently disarms the gate.
	_, err = ConvertRequestWithOptions([]byte(toolResultImage), Protocol("anthropic"), Protocol("openai"),
		RequestOptions{ImageOK: false, StrictLossy: true})
	if _, ok := AsUnsupported(err); !ok {
		t.Fatalf("strict without Diag must still refuse: err = %v", err)
	}

	// Same target with vision: no diagnostic, strict passes.
	clean, err := ConvertRequestWithOptions([]byte(toolResultImage), Protocol("anthropic"), Protocol("openai"),
		RequestOptions{ImageOK: true, Diag: NewDiagnostics(), StrictLossy: true})
	if err != nil {
		t.Fatalf("vision-capable target refused under strict: %v", err)
	}
	_ = clean
}

// A hosted anthropic tool (computer use, bash, text editor, …) that the tool
// converters drop reports the structured diagnostic server_tool_dropped on
// BOTH cross-protocol targets — the same code a→chat collects goes into the
// request log for a→responses too. The capability scanner fail-closes such
// tools at the conversion entry for every direction; the converters are the
// second line behind it, and a second line that only logs on one target is
// how the pair drifts.
func TestAnthropicServerToolDropReportsTheSameDiagnosticInEveryDirection(t *testing.T) {
	tools := []any{
		map[string]any{"type": "bash_20250124", "name": "bash"},
		map[string]any{"name": "lookup", "input_schema": map[string]any{"type": "object"}},
	}
	chatDiag, responsesDiag := NewDiagnostics(), NewDiagnostics()
	if got := anthropicToolsToOpenAI(tools, chatDiag); len(got) != 1 {
		t.Fatalf("chat tools = %v, want only the function tool", got)
	}
	if got := anthropicToolsToResponses(tools, responsesDiag); len(got) != 1 {
		t.Fatalf("responses tools = %v, want only the function tool", got)
	}
	for target, diag := range map[string]*Diagnostics{"a→chat": chatDiag, "a→responses": responsesDiag} {
		if !diag.HasCode("server_tool_dropped") {
			t.Errorf("%s: server_tool_dropped missing, diagnostics = %+v", target, diag.Items())
		}
	}
}
