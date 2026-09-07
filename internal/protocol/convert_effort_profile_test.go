package protocol

import (
	"testing"
)

// convert_effort_profile_test.go — the provider effort-enum layer
// (RequestOptions.ReasoningEffortEnum / ReasoningEffortOnly, sourced from
// provider.ChatEffortProfile) on top of the reasoning dialect switch, plus
// the unknown-effort clamp-down normalization (unknown_effort diagnostic).

var (
	testDeepseekEnum = map[string]string{
		"minimal": "low", "low": "low", "medium": "high",
		"high": "high", "xhigh": "high", "max": "max",
	}
	testKimiK3Enum = map[string]string{
		"none": "low", "minimal": "low", "low": "low", "medium": "high",
		"high": "high", "xhigh": "high", "max": "max",
	}
	testQwen38Enum = map[string]string{
		"minimal": "low", "low": "low", "medium": "medium",
		"high": "xhigh", "xhigh": "xhigh", "max": "xhigh",
	}
)

func mkResponsesReasoningReq(effort string) string {
	return `{"model":"g","reasoning":{"effort":"` + effort +
		`"},"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
}

// thinking dialect + enum (deepseek): the switch stays AND the mapped
// reasoning_effort is emitted alongside it.
func TestEffortProfile_ThinkingDialectWithEnum(t *testing.T) {
	out, err := convertResponsesRequestToOpenAIFor([]byte(mkResponsesReasoningReq("medium")),
		convertReqOpts{ReasoningDialect: ReasoningThinking, ReasoningEffortEnum: testDeepseekEnum, ImageOK: true})
	if err != nil {
		t.Fatal(err)
	}
	m := unmarshalMap(t, out)
	if got := strOf(asMap(m["thinking"])["type"]); got != "enabled" {
		t.Errorf("thinking = %v, want enabled", m["thinking"])
	}
	if got := m["reasoning_effort"]; got != "high" {
		t.Errorf("reasoning_effort = %v, want \"high\" (deepseek maps medium→high)", got)
	}

	// With an enum, minimal is a real low level — the switch stays ENABLED
	// (legacy none/minimal→disabled applies only without an enum).
	out, err = convertResponsesRequestToOpenAIFor([]byte(mkResponsesReasoningReq("minimal")),
		convertReqOpts{ReasoningDialect: ReasoningThinking, ReasoningEffortEnum: testDeepseekEnum, ImageOK: true})
	if err != nil {
		t.Fatal(err)
	}
	m = unmarshalMap(t, out)
	if got := strOf(asMap(m["thinking"])["type"]); got != "enabled" {
		t.Errorf("minimal → thinking = %v, want enabled (enum maps minimal→low)", m["thinking"])
	}
	if got := m["reasoning_effort"]; got != "low" {
		t.Errorf("minimal → reasoning_effort = %v, want \"low\"", got)
	}

	// none still disables the switch; deepseek's enum has no "none" entry, so
	// no reasoning_effort is emitted.
	out, err = convertResponsesRequestToOpenAIFor([]byte(mkResponsesReasoningReq("none")),
		convertReqOpts{ReasoningDialect: ReasoningThinking, ReasoningEffortEnum: testDeepseekEnum, ImageOK: true})
	if err != nil {
		t.Fatal(err)
	}
	m = unmarshalMap(t, out)
	if got := strOf(asMap(m["thinking"])["type"]); got != "disabled" {
		t.Errorf("none → thinking = %v, want disabled", m["thinking"])
	}
	if _, has := m["reasoning_effort"]; has {
		t.Errorf("none → reasoning_effort must be absent (no enum entry), got %v", m["reasoning_effort"])
	}
}

// EnumOnly (kimi-k3): the enum REPLACES the thinking switch entirely —
// reasoning_effort is emitted and no "thinking" key may appear (k3 rejects
// thinking+reasoning_effort together). "off" is expressed as the low enum.
func TestEffortProfile_EnumOnlyReplacesSwitch(t *testing.T) {
	out, err := convertResponsesRequestToOpenAIFor([]byte(mkResponsesReasoningReq("high")),
		convertReqOpts{ReasoningDialect: ReasoningThinking, ReasoningEffortEnum: testKimiK3Enum, ReasoningEffortOnly: true, ImageOK: true})
	if err != nil {
		t.Fatal(err)
	}
	m := unmarshalMap(t, out)
	if got := m["reasoning_effort"]; got != "high" {
		t.Errorf("reasoning_effort = %v, want \"high\"", got)
	}
	if _, has := m["thinking"]; has {
		t.Errorf("EnumOnly must not emit thinking, got %v", m["thinking"])
	}

	out, err = convertResponsesRequestToOpenAIFor([]byte(mkResponsesReasoningReq("none")),
		convertReqOpts{ReasoningDialect: ReasoningThinking, ReasoningEffortEnum: testKimiK3Enum, ReasoningEffortOnly: true, ImageOK: true})
	if err != nil {
		t.Fatal(err)
	}
	m = unmarshalMap(t, out)
	if got := m["reasoning_effort"]; got != "low" {
		t.Errorf(`none → reasoning_effort = %v, want "low" (k3 expresses off as low)`, got)
	}
	if _, has := m["thinking"]; has {
		t.Errorf("EnumOnly none must not emit thinking, got %v", m["thinking"])
	}
}

// enable_thinking dialect + enum (qwen3.8): switch plus mapped enum value.
func TestEffortProfile_EnableThinkingDialectWithEnum(t *testing.T) {
	out, err := convertResponsesRequestToOpenAIFor([]byte(mkResponsesReasoningReq("high")),
		convertReqOpts{ReasoningDialect: ReasoningEnableThinking, ReasoningEffortEnum: testQwen38Enum, ImageOK: true})
	if err != nil {
		t.Fatal(err)
	}
	m := unmarshalMap(t, out)
	if got := m["enable_thinking"]; got != true {
		t.Errorf("enable_thinking = %v, want true", got)
	}
	if got := m["reasoning_effort"]; got != "xhigh" {
		t.Errorf("reasoning_effort = %v, want \"xhigh\" (qwen3.8 maps high→xhigh)", got)
	}

	// none disables the switch and emits no enum value.
	out, err = convertResponsesRequestToOpenAIFor([]byte(mkResponsesReasoningReq("none")),
		convertReqOpts{ReasoningDialect: ReasoningEnableThinking, ReasoningEffortEnum: testQwen38Enum, ImageOK: true})
	if err != nil {
		t.Fatal(err)
	}
	m = unmarshalMap(t, out)
	if got := m["enable_thinking"]; got != false {
		t.Errorf("none → enable_thinking = %v, want false", got)
	}
	if _, has := m["reasoning_effort"]; has {
		t.Errorf("none → reasoning_effort must be absent, got %v", m["reasoning_effort"])
	}
}

// Unknown non-empty effort (codex "persistent") clamps down to high with an
// unknown_effort diagnostic — r→chat and r→a callsites both normalize.
func TestEffortProfile_UnknownEffortClampsToHigh(t *testing.T) {
	// r→chat, default dialect.
	d := NewDiagnostics()
	out, err := convertResponsesRequestToOpenAIFor([]byte(mkResponsesReasoningReq("persistent")),
		convertReqOpts{ReasoningDialect: ReasoningEffort, Diag: d, ImageOK: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := unmarshalMap(t, out)["reasoning_effort"]; got != "high" {
		t.Errorf(`"persistent" → reasoning_effort = %v, want "high" (clamp-down)`, got)
	}
	if !d.HasCode("unknown_effort") {
		t.Errorf("missing unknown_effort diagnostic, got %v", d.Items())
	}

	// r→chat, thinking dialect with enum: the clamped high hits the enum.
	d = NewDiagnostics()
	out, err = convertResponsesRequestToOpenAIFor([]byte(mkResponsesReasoningReq("persistent")),
		convertReqOpts{ReasoningDialect: ReasoningThinking, ReasoningEffortEnum: testDeepseekEnum, Diag: d, ImageOK: true})
	if err != nil {
		t.Fatal(err)
	}
	m := unmarshalMap(t, out)
	if got := strOf(asMap(m["thinking"])["type"]); got != "enabled" {
		t.Errorf(`"persistent" → thinking = %v, want enabled`, m["thinking"])
	}
	if got := m["reasoning_effort"]; got != "high" {
		t.Errorf(`"persistent" → reasoning_effort = %v, want "high"`, got)
	}
	if !d.HasCode("unknown_effort") {
		t.Errorf("missing unknown_effort diagnostic, got %v", d.Items())
	}

	// r→a: the clamped high lands on the 16384 ladder rung.
	d = NewDiagnostics()
	out, err = convertResponsesRequestToAnthropic([]byte(
		`{"model":"gpt-x","max_output_tokens":100000,"reasoning":{"effort":"persistent"},`+
			`"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`), d)
	if err != nil {
		t.Fatal(err)
	}
	th := asMap(unmarshalMap(t, out)["thinking"])
	if th == nil || th["type"] != "enabled" || intOf(th["budget_tokens"]) != 16384 {
		t.Errorf(`"persistent" → thinking = %v, want enabled/16384 (clamped to high)`, th)
	}
	if !d.HasCode("unknown_effort") {
		t.Errorf("missing unknown_effort diagnostic, got %v", d.Items())
	}

	// chat→a: same normalization at the reasoning_effort callsite.
	d = NewDiagnostics()
	out, err = convertOpenAIRequestToAnthropic([]byte(
		`{"model":"g","reasoning_effort":"persistent","max_tokens":100000,"messages":[{"role":"user","content":"hi"}]}`), d)
	if err != nil {
		t.Fatal(err)
	}
	th = asMap(unmarshalMap(t, out)["thinking"])
	if th == nil || intOf(th["budget_tokens"]) != 16384 {
		t.Errorf(`chat→a "persistent" → thinking = %v, want enabled/16384`, th)
	}
	if !d.HasCode("unknown_effort") {
		t.Errorf("missing unknown_effort diagnostic, got %v", d.Items())
	}
}

// nil-Enum providers keep the pure switch shapes unchanged (no
// reasoning_effort key in thinking mode, legacy none/minimal→disabled).
func TestEffortProfile_NilEnumUnchanged(t *testing.T) {
	out, err := convertResponsesRequestToOpenAIFor([]byte(mkResponsesReasoningReq("minimal")),
		convertReqOpts{ReasoningDialect: ReasoningThinking, ImageOK: true})
	if err != nil {
		t.Fatal(err)
	}
	m := unmarshalMap(t, out)
	if got := strOf(asMap(m["thinking"])["type"]); got != "disabled" {
		t.Errorf("nil enum minimal → thinking = %v, want disabled (legacy)", m["thinking"])
	}
	if _, has := m["reasoning_effort"]; has {
		t.Errorf("nil enum must not emit reasoning_effort, got %v", m["reasoning_effort"])
	}
}
