package protocol

import (
	"strings"
	"testing"
)

func TestBuildSystemOneBody(t *testing.T) {
	body, err := BuildSystemOneBody("jev-1.13", "I was charged twice.", map[string]SystemOneQuestion{
		"refund": {Type: "noul", Instructions: "Is the customer asking for money back?"},
		"dept": {Type: "choice", Instructions: "Which team?",
			Criteria: map[string]string{"billing": "Charges and refunds", "technical": "Bugs"}},
		"severity": {Type: "score", Instructions: "How severe?",
			Criteria: []string{"cosmetic", "degraded", "blocking"}},
	})
	if err != nil {
		t.Fatalf("BuildSystemOneBody: %v", err)
	}
	text := string(body)
	for _, want := range []string{
		`"model":"jev-1.13"`, `"state":"I was charged twice."`,
		`"type":"noul"`, `"type":"choice"`, `"type":"score"`,
		`"billing":"Charges and refunds"`, `"cosmetic"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("body missing %s: %s", want, text)
		}
	}
	if _, err := BuildSystemOneBody("m", "s", nil); err == nil {
		t.Error("empty questions must fail")
	}
}

func TestParseSystemOneResponse(t *testing.T) {
	body := []byte(`{
		"id": "gen-dec-1",
		"model": "typesafe/jev-1.13-20260917",
		"provider": "TypeSafe",
		"answers": {
			"refund": {"type": "noul", "noul": 0.98},
			"dept": {"type": "choice", "choice": "billing",
				"probabilities": {"billing": 0.91, "technical": 0.09}, "confidence": 0.87},
			"severity": {"type": "score", "score": 1.035,
				"probabilities": {"0": 0.1, "1": 0.8, "2": 0.1}, "confidence": 0.76}
		},
		"usage": {"input_tokens": 275, "output_tokens": 20, "cost": 0.00003}
	}`)
	model, answers, usage, err := ParseSystemOneResponse(body)
	if err != nil {
		t.Fatalf("ParseSystemOneResponse: %v", err)
	}
	if model != "typesafe/jev-1.13-20260917" {
		t.Errorf("model = %q", model)
	}
	if answers["refund"].Noul != 0.98 {
		t.Errorf("noul = %+v", answers["refund"])
	}
	dept := answers["dept"]
	if dept.Choice != "billing" || dept.Confidence != 0.87 || dept.Probabilities["technical"] != 0.09 {
		t.Errorf("choice answer = %+v", dept)
	}
	if answers["severity"].Score != 1.035 {
		t.Errorf("score answer = %+v", answers["severity"])
	}
	if usage.InputTokens != 275 || usage.OutputTokens != 20 {
		t.Errorf("usage = %+v", usage)
	}

	if _, _, _, err := ParseSystemOneResponse([]byte(`{"model":"m","answers":{}}`)); err == nil {
		t.Error("empty answers must fail (broken upstream)")
	}
	if _, _, _, err := ParseSystemOneResponse([]byte(`not json`)); err == nil {
		t.Error("malformed body must fail")
	}
}

func TestDecisionsProtocolIdentity(t *testing.T) {
	if got := ForPath("/v1/decisions"); got != Decisions {
		t.Errorf("ForPath(/v1/decisions) = %q", got)
	}
	if got := BackendPath(Decisions); got != "/systemone" {
		t.Errorf("BackendPath(decisions) = %q", got)
	}
	if _, ok := Parse("decisions"); !ok {
		t.Error("Parse(decisions) rejected")
	}
	if NeedsConversion(Decisions, Decisions) {
		t.Error("same-protocol decisions must not need conversion")
	}
	for _, pair := range [][2]Protocol{
		{OpenAI, Decisions}, {Anthropic, Decisions}, {Responses, Decisions},
		{Decisions, OpenAI}, {Decisions, Anthropic}, {Decisions, Responses},
	} {
		if !NeedsConversion(pair[0], pair[1]) {
			t.Errorf("NeedsConversion(%s, %s) = false, want true (fail-closed stub)", pair[0], pair[1])
		}
	}
}

func TestDecisionsConversionStubsFailClosed(t *testing.T) {
	chatBody := []byte(`{"model":"jev","messages":[{"role":"user","content":"hi"}]}`)
	if _, err := ConvertRequestWithOptions(chatBody, OpenAI, Decisions, RequestOptions{}); err == nil {
		t.Fatal("openai→decisions request conversion must refuse")
	} else if _, ok := AsUnsupported(err); !ok {
		t.Fatalf("error must be typed unsupported, got %v", err)
	}
	if _, err := ConvertResponse([]byte(`{"model":"m","answers":{"a":{"type":"noul","noul":1}}}`), OpenAI, Decisions, ResponseContext{}); err == nil {
		t.Fatal("decisions→openai response conversion must refuse")
	}
	// Same-protocol decisions traffic is byte-identical passthrough.
	decBody, err := BuildSystemOneBody("jev", "s", map[string]SystemOneQuestion{
		"q": {Type: "noul", Instructions: "i"},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := ConvertRequestWithOptions(decBody, Decisions, Decisions, RequestOptions{})
	if err != nil || string(out) != string(decBody) {
		t.Errorf("decisions→decisions must passthrough unchanged: out=%s err=%v", out, err)
	}
}
