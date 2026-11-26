package forward

import (
	"encoding/json"
	"strings"
)

const (
	selectorStateMaxRunes = 6000

	defaultSelectorChoiceInstructions = "You are routing an LLM request to the best candidate model. " +
		"Consider the request's difficulty, the capabilities it needs, and cost-efficiency. " +
		"Answer with the single best option for answering this request."
	defaultSelectorDifficultyInstructions = "How difficult is this request for an LLM to answer well? " +
		"Rate it on the 5-level scale."
)

var defaultSelectorDifficultyLevels = []string{
	"trivial — a short factual lookup or a one-line answer",
	"simple — a single routine step with no ambiguity",
	"moderate — multi-step reasoning or ordinary code",
	"hard — complex debugging, design, or long-context synthesis",
	"expert — frontier difficulty, ambiguous, or high-stakes",
}

type selectorRequestFacts struct {
	HasImage        bool
	EstimatedTokens int64
}

type selectorCandidate struct {
	Target RouteTarget
	ID     string
	Rubric string
}

func buildSelectorState(original []byte, protocol string, facts selectorRequestFacts, hasTools bool, candidates []selectorCandidate) map[string]any {
	task, turns := lastSelectorUserText(original, protocol)
	task = truncateSelectorRunes(task, selectorStateMaxRunes)
	options := make([]map[string]any, 0, len(candidates))
	for _, candidate := range candidates {
		rubric := candidate.Rubric
		if rubric == "" {
			rubric = candidate.Target.Provider + "/" + candidate.Target.Model
		}
		options = append(options, map[string]any{
			"id":       candidate.ID,
			"model":    candidate.Target.Provider + "/" + candidate.Target.Model,
			"rubric":   rubric,
			"protocol": candidate.Target.Protocol,
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

func selectorCriteria(candidates []selectorCandidate) map[string]string {
	criteria := make(map[string]string, len(candidates))
	for _, candidate := range candidates {
		label := candidate.Target.Provider + "/" + candidate.Target.Model
		if candidate.Rubric != "" {
			label += " — " + candidate.Rubric
		}
		criteria[candidate.ID] = label
	}
	return criteria
}

func lastSelectorUserText(original []byte, protocol string) (text string, turns int) {
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
		return selectorMessageText(message["content"]), turns
	}
	return "", turns
}

func selectorMessageText(content any) string {
	switch value := content.(type) {
	case string:
		return value
	case []any:
		var parts []string
		for _, raw := range value {
			block, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := block["type"].(string)
			if typ == "" || typ == "text" || typ == "input_text" {
				if text, ok := block["text"].(string); ok && text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func truncateSelectorRunes(text string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= max {
		return text
	}
	return string(runes[:max])
}
