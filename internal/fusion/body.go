package fusion

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
)

// Permuter supplies a permutation for candidate ordering.
type Permuter func(int) []int

// BuildSynthesisBody injects the standard synthesis section. Its candidate
// order uses rand.Perm, preserving existing production behavior.
func BuildSynthesisBody(
	original []byte,
	protocol string,
	candidates []string,
	judgeReport string,
	instruction string,
) ([]byte, bool) {
	return BuildSynthesisBodyWithPermuter(
		original,
		protocol,
		candidates,
		judgeReport,
		instruction,
		rand.Perm,
	)
}

// BuildSynthesisBodyWithPermuter is BuildSynthesisBody with caller-controlled
// candidate ordering. Invalid permutations are normalized deterministically so
// an integration hook cannot panic or omit a candidate.
func BuildSynthesisBodyWithPermuter(
	original []byte,
	protocol string,
	candidates []string,
	judgeReport string,
	instruction string,
	permute Permuter,
) ([]byte, bool) {
	if instruction == "" {
		instruction = DefaultSynthesisInstruction
	}
	if permute == nil {
		permute = rand.Perm
	}
	order := validPermutation(permute(len(candidates)), len(candidates))
	var section strings.Builder
	section.WriteString(instruction)
	if judgeReport != "" {
		fmt.Fprintf(&section, "\n\n<JUDGE_ANALYSIS>\n%s\n</JUDGE_ANALYSIS>", judgeReport)
	}
	for index, candidateIndex := range order {
		fmt.Fprintf(
			&section,
			"\n\n<CANDIDATE %d>\n%s\n</CANDIDATE %d>",
			index+1,
			candidates[candidateIndex],
			index+1,
		)
	}
	return injectSection(original, protocol, section.String())
}

func validPermutation(order []int, size int) []int {
	if len(order) != size {
		return identityPermutation(size)
	}
	seen := make([]bool, size)
	for _, index := range order {
		if index < 0 || index >= size || seen[index] {
			return identityPermutation(size)
		}
		seen[index] = true
	}
	return order
}

func identityPermutation(size int) []int {
	order := make([]int, size)
	for index := range order {
		order[index] = index
	}
	return order
}

// BuildJudgeBody injects the judge instruction and candidates in collection
// order. Judge review intentionally has no candidate-order shuffle.
func BuildJudgeBody(original []byte, protocol string, candidates []string) ([]byte, bool) {
	var section strings.Builder
	section.WriteString(DefaultJudgeInstruction)
	for index, candidate := range candidates {
		fmt.Fprintf(
			&section,
			"\n\n<CANDIDATE %d>\n%s\n</CANDIDATE %d>",
			index+1,
			candidate,
			index+1,
		)
	}
	return injectSection(original, protocol, section.String())
}

func injectSection(original []byte, protocol, section string) ([]byte, bool) {
	var request map[string]any
	if json.Unmarshal(original, &request) != nil {
		return nil, false
	}
	switch protocol {
	case "anthropic":
		switch system := request["system"].(type) {
		case nil:
			request["system"] = section
		case string:
			request["system"] = system + "\n\n" + section
		case []any:
			request["system"] = append(system, map[string]any{"type": "text", "text": section})
		default:
			return nil, false
		}
	case "responses":
		message := map[string]any{
			"type": "message",
			"role": "user",
			"content": []any{
				map[string]any{"type": "input_text", "text": section},
			},
		}
		switch input := request["input"].(type) {
		case []any:
			request["input"] = append(input, message)
		case string:
			items := make([]any, 0, 2)
			if input != "" {
				items = append(items, map[string]any{
					"type": "message",
					"role": "user",
					"content": []any{
						map[string]any{"type": "input_text", "text": input},
					},
				})
			}
			request["input"] = append(items, message)
		default:
			request["input"] = []any{message}
		}
	default:
		message := map[string]any{"role": "user", "content": section}
		if messages, ok := request["messages"].([]any); ok {
			request["messages"] = append(messages, message)
		} else {
			request["messages"] = []any{message}
		}
	}
	out, err := json.Marshal(request)
	return out, err == nil
}

// StripDraftFields removes tool-only and stream-only request fields from a
// draft leg. Invalid JSON is returned unchanged so the caller can handle its
// own conversion/build failure policy.
func StripDraftFields(body []byte) []byte {
	var request map[string]any
	if json.Unmarshal(body, &request) != nil {
		return body
	}
	delete(request, "tools")
	delete(request, "tool_choice")
	delete(request, "stream_options")
	request["stream"] = false
	out, err := json.Marshal(request)
	if err != nil {
		return body
	}
	return out
}

// ParseUsage extracts the non-streaming Anthropic, Responses, and OpenAI
// usage variants. Cache-token fields are retained separately and are not
// folded into Input; for the inclusive OpenAI/Responses totals the cached
// share (carried only by the *_tokens_details spellings) is deducted from
// Input once — the same extraction convention as the pass-through usage
// scanner and the request-log usage projection.
func ParseUsage(body []byte) Usage {
	var response struct {
		Usage struct {
			InputTokens      uint64 `json:"input_tokens"`
			OutputTokens     uint64 `json:"output_tokens"`
			CacheCreation    uint64 `json:"cache_creation_input_tokens"`
			CacheRead        uint64 `json:"cache_read_input_tokens"`
			PromptTokens     uint64 `json:"prompt_tokens"`
			CompletionTokens uint64 `json:"completion_tokens"`
			PromptCacheHit   uint64 `json:"prompt_cache_hit_tokens"`
			// OpenAI chat shape; prompt_tokens includes both cache shares.
			PromptTokensDetails struct {
				CachedTokens    uint64 `json:"cached_tokens"`
				CacheWriteToken uint64 `json:"cache_write_tokens"`
			} `json:"prompt_tokens_details"`
			// Responses shape; input_tokens includes both cache shares.
			InputTokensDetails struct {
				CachedTokens    uint64 `json:"cached_tokens"`
				CacheWriteToken uint64 `json:"cache_write_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &response) != nil {
		return Usage{}
	}
	u := response.Usage
	// Hit spellings (anthropic direct, chat details, deepseek prompt_cache_hit_tokens,
	// responses details) report the same bucket; merge by max like the SSE
	// scanner. Writes: anthropic direct + the details cache_write spellings.
	cacheRead := max(u.CacheRead, u.PromptTokensDetails.CachedTokens, u.PromptCacheHit, u.InputTokensDetails.CachedTokens)
	cacheWrite := max(u.CacheCreation, u.PromptTokensDetails.CacheWriteToken, u.InputTokensDetails.CacheWriteToken)
	// Anthropic input_tokens is already cache-exclusive; the OpenAI/Responses
	// totals are inclusive, so each shape's shares are deducted from its own
	// inclusive total (clamped at zero), never from the anthropic field.
	prompt := clampSub2(u.PromptTokens, max(u.PromptTokensDetails.CachedTokens, u.PromptCacheHit), u.PromptTokensDetails.CacheWriteToken)
	input := clampSub2(u.InputTokens, u.InputTokensDetails.CachedTokens, u.InputTokensDetails.CacheWriteToken)
	return Usage{
		Input:         prompt + input,
		Output:        u.OutputTokens + u.CompletionTokens,
		CacheCreation: cacheWrite,
		CacheRead:     cacheRead,
	}
}

// clampSub2 subtracts two shares from a total, floored at zero (same ledger
// as the scanner's subClamped2).
func clampSub2(total, a, b uint64) uint64 {
	if a > total {
		total = 0
	} else {
		total -= a
	}
	if b > total {
		return 0
	}
	return total - b
}

// TruncateRunes caps text by Unicode code points rather than bytes.
func TruncateRunes(text string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= max {
		return text
	}
	return string(runes[:max])
}
