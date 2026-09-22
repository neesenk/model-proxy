package fusion

import (
	"encoding/json"
	"strings"

	configdomain "model-proxy/internal/config"
)

// selector.go owns the semantic side of the fusion routing decision: request
// state construction, candidate/question definitions and the engine's
// selection policy. Wire encoding and transport stay with the application
// adapter (internal/forward) via Ports.SelectPanel — this package never sees
// the decisions wire shape.

const (
	// DegradedSelectorDirect marks runs the selector answered directly: the
	// decisions model picked a candidate and scored the request easy enough
	// that orchestration is skipped (original body straight to the chosen
	// model — same shape as the other pre-fan-out degradations).
	DegradedSelectorDirect = "selector_direct"

	// selectorAction* are the SelectorObservation.Action values.
	selectorActionNone     = "none"
	selectorActionDirect   = "direct"
	selectorActionTrim     = "trim"
	selectorActionFallback = "fallback"

	// selectorStateMaxRunes caps the user text fed into the decision state:
	// decision accuracy decays when the state carries material the question
	// does not need, and the API's state budget is finite.
	selectorStateMaxRunes = 6000
)

// DefaultSelectorChoiceInstructions is the built-in choice-question prompt.
// The decisions model reads instructions literally — keep them explicit about
// the selection goal and the answer contract.
const DefaultSelectorChoiceInstructions = "You are routing an LLM request to the best candidate model. " +
	"Consider the request's difficulty, the capabilities it needs, and cost-efficiency. " +
	"Answer with the single best option for answering this request."

// DefaultSelectorDifficultyInstructions is the built-in score-question prompt.
const DefaultSelectorDifficultyInstructions = "How difficult is this request for an LLM to answer well? " +
	"Rate it on the 5-level scale."

// DefaultSelectorDifficultyLevels are the ordered difficulty levels for the
// score question (index 0 = easiest). A configurable override is deliberately
// NOT offered: direct_score_max thresholds are tuned against these levels.
var DefaultSelectorDifficultyLevels = []string{
	"trivial — a short factual lookup or a one-line answer",
	"simple — a single routine step with no ambiguity",
	"moderate — multi-step reasoning or ordinary code",
	"hard — complex debugging, design, or long-context synthesis",
	"expert — frontier difficulty, ambiguous, or high-stakes",
}

// RequestFacts carries request-derived facts the engine cannot compute itself
// (the application adapter scans the body once via internal/routing).
type RequestFacts struct {
	HasImage        bool
	EstimatedTokens int64
}

// SelectorCandidate is one routable choice offered to the decisions model.
type SelectorCandidate struct {
	Target configdomain.RouteTarget
	ID     string // stable option id ("c0", "c1", ...) echoed back as the choice
	Rubric string // one-line capability/cost description ("" → provider/model)
}

// SelectRequest is the semantic routing question built by the engine; the
// application adapter owns wire encoding and transport.
type SelectRequest struct {
	State                  map[string]any
	Candidates             []SelectorCandidate
	ChoiceInstructions     string
	DifficultyInstructions string
	DifficultyLevels       []string
}

// SelectResult is the application adapter's projection of the decisions
// response. Err covers transport/parse failures AND unusable answers (empty
// answers, unknown choice) — the engine only distinguishes "usable" from not.
type SelectResult struct {
	ChoiceID      string
	Confidence    float64
	Probabilities map[string]float64
	Difficulty    float64
	LatencyMs     int64
	Model         string // upstream model version that answered (alias drift signal)
	Usage         Usage
	Err           error
}

// SelectorObservation is the JSON-safe selector record retained with a Run.
type SelectorObservation struct {
	Mode       string  `json:"mode"`
	Action     string  `json:"action"` // none|direct|trim|fallback
	Choice     string  `json:"choice,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
	Difficulty float64 `json:"difficulty,omitempty"`
	LatencyMs  int64   `json:"latency_ms,omitempty"`
	Err        string  `json:"err,omitempty"`
}

// BuildSelectorState assembles the decisions state from the client request:
// the last user message (rune-capped), conversation facts computed in code
// (the decisions model does not count reliably), and the candidate list.
func BuildSelectorState(
	original []byte,
	protocol string,
	facts RequestFacts,
	hasTools bool,
	candidates []SelectorCandidate,
) map[string]any {
	task, turns := lastUserText(original, protocol)
	task = TruncateRunes(task, selectorStateMaxRunes)
	options := make([]map[string]any, 0, len(candidates))
	for _, c := range candidates {
		rubric := c.Rubric
		if rubric == "" {
			rubric = c.Target.Provider + "/" + c.Target.Model
		}
		options = append(options, map[string]any{
			"id":       c.ID,
			"model":    c.Target.Provider + "/" + c.Target.Model,
			"rubric":   rubric,
			"protocol": c.Target.Protocol,
		})
	}
	return map[string]any{
		"task": task,
		"conversation": map[string]any{
			"protocol":               protocol,
			"turns":                  turns,
			"has_tools":              hasTools,
			"has_image":              facts.HasImage,
			"estimated_input_tokens": facts.EstimatedTokens,
		},
		"candidates": options,
	}
}

// selectorCandidates collects the deduplicated routable options: panel members
// plus the synthesizer, keyed by provider/model.
func selectorCandidates(recipe configdomain.FusionConfig) []SelectorCandidate {
	seen := map[string]bool{}
	var out []SelectorCandidate
	add := func(target configdomain.RouteTarget) {
		key := target.Provider + "/" + target.Model
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, SelectorCandidate{
			Target: target,
			ID:     "c" + itoa(len(out)),
			Rubric: target.Rubric,
		})
	}
	for _, member := range recipe.Panel {
		add(member)
	}
	add(recipe.Synthesizer)
	return out
}

// itoa avoids importing strconv for the tiny id numbering.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits [4]byte
	i := len(digits)
	for n > 0 {
		i--
		digits[i] = byte('0' + n%10)
		n /= 10
	}
	return string(digits[i:])
}

// lastUserText extracts the trailing user message text and the message count
// from an anthropic/openai (messages) or responses (input) request body.
// Content may be a string or an array of text blocks (text / input_text).
// Unparseable bodies degrade to empty text and zero turns — the decision call
// then judges an empty task, which the confidence gate treats cautiously.
func lastUserText(original []byte, protocol string) (text string, turns int) {
	var request map[string]any
	if json.Unmarshal(original, &request) != nil {
		return "", 0
	}
	key := "messages"
	if protocol == "responses" {
		key = "input"
	}
	messages, ok := request[key].([]any)
	if !ok {
		return "", 0
	}
	turns = len(messages)
	for i := len(messages) - 1; i >= 0; i-- {
		message, ok := messages[i].(map[string]any)
		if !ok {
			continue
		}
		role, _ := message["role"].(string)
		if role != "user" {
			continue
		}
		return messageText(message["content"]), turns
	}
	return "", turns
}

// messageText flattens one message's content to plain text.
func messageText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, raw := range v {
			block, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := block["type"].(string)
			if typ == "" || typ == "text" || typ == "input_text" {
				if s, ok := block["text"].(string); ok && s != "" {
					parts = append(parts, s)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}
