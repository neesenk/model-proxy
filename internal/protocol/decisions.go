package protocol

import (
	"fmt"

	sonic "github.com/bytedance/sonic"
)

// decisions.go owns the wire shape of the "decisions" protocol (System One
// decisions API, as served by TypeSafe and OpenRouter): a request carries
// {model, state, questions} and the response carries typed answers instead of
// generated text. There is no streaming, no tools, and no cross-conversion
// with the chat protocol family (those pairs are fail-closed stubs in
// conversion_registry.go).
//
// Question types:
//   - choice: pick one of up to 255 options; criteria maps option id → rubric;
//     the answer carries choice + per-option probabilities + confidence.
//   - score: a position on a 2..10-level ordered scale; criteria lists the
//     level descriptions in order; the answer carries score (may land between
//     levels) + probabilities + confidence.
//   - noul: a yes/no probability; the answer carries noul (0..1), no
//     confidence (the number already is the belief).

// SystemOneQuestion is one typed question evaluated against the state.
// Criteria is map[string]string for choice, []string for score, nil for noul.
type SystemOneQuestion struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

// SystemOneAnswer is one typed answer. Only the fields meaningful for Type
// are populated by the upstream.
type SystemOneAnswer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice,omitempty"`
	Score         float64            `json:"score,omitempty"`
	Noul          float64            `json:"noul,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Confidence    float64            `json:"confidence,omitempty"`
}

// SystemOneUsage is the decisions usage shape. Output is billed at zero on
// TypeSafe but still reported; OpenRouter additionally reports cost.
type SystemOneUsage struct {
	InputTokens  uint64  `json:"input_tokens"`
	OutputTokens uint64  `json:"output_tokens"`
	Cost         float64 `json:"cost,omitempty"`
}

// BuildSystemOneBody encodes one decisions request. The state is passed
// through as-is (string, object, or array per the API contract).
func BuildSystemOneBody(model string, state any, questions map[string]SystemOneQuestion) ([]byte, error) {
	if len(questions) == 0 {
		return nil, fmt.Errorf("decisions request needs at least one question")
	}
	return sonic.Marshal(map[string]any{
		"model":     model,
		"state":     state,
		"questions": questions,
	})
}

// ParseSystemOneResponse decodes a decisions response. It tolerates the extra
// fields OpenRouter adds (id/provider/usage.cost) and requires at least one
// answer — an empty-answers 200 is as broken as an empty-choices chat 200.
func ParseSystemOneResponse(body []byte) (model string, answers map[string]SystemOneAnswer, usage SystemOneUsage, err error) {
	var src struct {
		Model   string                     `json:"model"`
		Answers map[string]SystemOneAnswer `json:"answers"`
		Usage   SystemOneUsage             `json:"usage"`
	}
	if err := sonic.Unmarshal(body, &src); err != nil {
		return "", nil, SystemOneUsage{}, fmt.Errorf("parse decisions response: %w", err)
	}
	if len(src.Answers) == 0 {
		return "", nil, SystemOneUsage{}, fmt.Errorf("decisions response has no answers (treating as upstream failure)")
	}
	return src.Model, src.Answers, src.Usage, nil
}
