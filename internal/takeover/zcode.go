package takeover

import (
	"fmt"

	"model-proxy/internal/routing"
)

// zcode.go — models shape "zcode": renders model-proxy's exposed models as a
// personal provider layer in ZCode's provider_config.json
// (github.com/zai-org/ZCode; on-disk schema validated against
// packages/provider-node's provider-config-file-codec +
// packages/provider's rule-data-schema). The file is RULE-based with ARRAYS
// (providerRules / providerModelRules), so the generic json.set (map-only,
// replace semantics) cannot express the write — this file owns the merge:
// entries whose providerId is ours are dropped and re-appended (upsert),
// every other personal provider/rule the user added through the ZCode UI is
// preserved.

// zcodeAPIType maps the variant's declared chat protocol onto ZCode's
// provider api.type enum (providerApiTypeDataSchema). Fail-closed: shape
// zcode requires an explicit protocol, guessing a wire format would hand
// ZCode a provider it cannot drive.
func zcodeAPIType(protocol string) (string, error) {
	switch protocol {
	case "anthropic":
		return "anthropic-messages", nil
	case "openai":
		return "openai-chat-completions", nil
	case "responses":
		return "openai-responses", nil
	}
	return "", fmt.Errorf("models shape zcode requires protocol anthropic|openai|responses, got %q", protocol)
}

// mergeZcodeProviderConfig folds the rendered provider layer into the decoded
// provider_config.json document v (in place): our provider rule, one model
// rule per exposed model, and the default model selection. Idempotent —
// a re-takeover replaces exactly the entries it owns.
func mergeZcodeProviderConfig(v map[string]any, t *Template, ctx renderContext) error {
	apiType, err := zcodeAPIType(t.Protocol)
	if err != nil {
		return fmt.Errorf("template %s: %w", t.Name, err)
	}
	cfg := dottedMap(v, []string{"config"})

	rules, err := ownedArray(cfg, []string{"providerConfigRules", "providerRules"})
	if err != nil {
		return fmt.Errorf("template %s: %w", t.Name, err)
	}
	cfg["providerConfigRules"].(map[string]any)["providerRules"] =
		append(dropProviderEntries(rules, ctx.providerID), zcodeProviderRule(apiType, ctx))

	modelRules, err := ownedArray(cfg, []string{"modelConfigRules", "providerModelRules"})
	if err != nil {
		return fmt.Errorf("template %s: %w", t.Name, err)
	}
	cfg["modelConfigRules"].(map[string]any)["providerModelRules"] =
		append(dropProviderEntries(modelRules, ctx.providerID), zcodeModelRules(ctx)...)

	if len(ctx.models) > 0 {
		selection := map[string]any{
			"providerId": ctx.providerID,
			"modelId":    ctx.primary,
		}
		if level := zcodeDefaultReasoningLevel(ctx); level != "" {
			selection["options"] = map[string]any{"reasoningLevel": level}
		}
		cfg["defaultModelSelection"] = selection
	}
	return nil
}

// ownedArray navigates (creating) the map at path[:-1] and returns the array
// at path[-1]. A non-array value in the terminal slot is a broken file —
// fail closed instead of silently discarding user data.
func ownedArray(v map[string]any, path []string) ([]any, error) {
	parent := dottedMap(v, path[:len(path)-1])
	switch existing := parent[path[len(path)-1]].(type) {
	case nil:
		return nil, nil
	case []any:
		return existing, nil
	default:
		return nil, fmt.Errorf("%s is %T, want an array — refusing to rewrite a broken config", path, existing)
	}
}

// dropProviderEntries removes every array element carrying our providerId
// (a previous takeover's provider rule and all its model rules). Elements
// without a providerId or with a different one are user-owned and preserved.
func dropProviderEntries(arr []any, providerID string) []any {
	out := make([]any, 0, len(arr))
	for _, e := range arr {
		if m, ok := e.(map[string]any); ok && m["providerId"] == providerID {
			continue
		}
		out = append(out, e)
	}
	return out
}

// zcodeProviderRule renders the single personal provider entry
// (personalProviderConfigRuleSchema): api-key access with the PROXY_MANAGED
// placeholder, the variant's api type, and the exposed model list both as
// personalModelIds (what the provider serves) and modelOrder (picker order).
func zcodeProviderRule(apiType string, ctx renderContext) map[string]any {
	ids := make([]any, 0, len(ctx.models))
	for _, m := range ctx.models {
		ids = append(ids, m.Exposed)
	}
	return map[string]any{
		"providerId":   ctx.providerID,
		"providerName": ctx.displayName,
		"enabled":      true,
		"config": map[string]any{
			"group": "standard-personal",
			"access": map[string]any{
				"type":   "api-key",
				"apiKey": "PROXY_MANAGED",
			},
			"api": map[string]any{
				"type":    apiType,
				"baseUrl": ctx.baseURL,
			},
			"personalModelIds": ids,
			"modelOrder":       ids,
			"visibility":       "visible",
		},
	}
}

// zcodeModelRules renders one providerModelRules entry per exposed model.
// Metadata follows the same conservative contract as the other shapes — no
// models.dev fact, no capability claim: context/output fall back to the
// conservative defaults, input formats list only advertised modalities, and
// reasoningLevel values are written only for reasoning models (the effort
// dial as advertised, or the generic disabled/enabled toggle when the model
// reasons but advertises no dial — ZCode's own builtin rule shape). The
// option maps (reasoning_level → wire params) come from ZCode's builtin
// per-api-type rules, so only values/max are written here.
func zcodeModelRules(ctx renderContext) []any {
	out := make([]any, 0, len(ctx.models))
	for _, m := range ctx.models {
		contextN := m.PM.Context
		if contextN <= 0 {
			contextN = routing.DefaultModelMetadata.Context
		}
		output := m.PM.Output
		if output <= 0 {
			output = routing.DefaultModelMetadata.Output
		}
		inputFormat := map[string]any{"supportsText": true}
		for _, in := range m.PM.Modalities.Input {
			switch in {
			case "image":
				inputFormat["supportsImage"] = true
			case "video":
				inputFormat["supportsVideo"] = true
			case "audio":
				inputFormat["supportsAudio"] = true
			case "pdf":
				inputFormat["supportsPdf"] = true
			}
		}
		optionSpecs := map[string]any{
			"maxOutputTokens": map[string]any{"max": output},
		}
		if m.PM.Reasoning {
			values := m.PM.ReasoningEfforts
			if len(values) == 0 {
				values = []string{"disabled", "enabled"}
			}
			vs := make([]any, 0, len(values))
			for _, v := range values {
				vs = append(vs, v)
			}
			optionSpecs["reasoningLevel"] = map[string]any{"values": vs}
		}
		out = append(out, map[string]any{
			"providerId": ctx.providerID,
			"modelId":    m.Exposed,
			"config": map[string]any{
				"enabled": true,
				"properties": map[string]any{
					"contextWindow":    contextN,
					"inputFormat":      inputFormat,
					"supportsToolCall": m.PM.ToolCall,
				},
				"optionSpecs": optionSpecs,
			},
		})
	}
	return out
}

// zcodeDefaultReasoningLevel picks the default selection's reasoning level:
// the primary model's highest advertised effort dial ("enabled" when it
// reasons without a dial), empty for non-reasoning models (no options key —
// a level the model does not support would fail selection validation).
func zcodeDefaultReasoningLevel(ctx renderContext) string {
	for _, m := range ctx.models {
		if m.Exposed != ctx.primary {
			continue
		}
		if !m.PM.Reasoning {
			return ""
		}
		if n := len(m.PM.ReasoningEfforts); n > 0 {
			return m.PM.ReasoningEfforts[n-1]
		}
		return "enabled"
	}
	return ""
}

// nestedStringProviderAware walks v along path like nestedString, but when an
// intermediate node is an ARRAY it selects the FIRST element whose provider
// marker — "providerId" (ZCode's providerRules) or "vendor" (WorkBuddy's
// custom models) — equals providerID. This keeps drift probes usable on
// array-based config without positional indexes (the client/UI may reorder
// entries); for WorkBuddy every owned entry carries the same URL, so the
// first vendor match is the right probe target.
func nestedStringProviderAware(v map[string]any, providerID string, path ...string) (string, bool) {
	var cur any = v
	for i, k := range path {
		if arr, ok := cur.([]any); ok {
			cur = nil
			for _, e := range arr {
				if m, ok := e.(map[string]any); ok && (m["providerId"] == providerID || m["vendor"] == providerID) {
					cur = m
					break
				}
			}
			if cur == nil {
				return "", false
			}
		}
		m, ok := cur.(map[string]any)
		if !ok {
			return "", false
		}
		if i == len(path)-1 {
			s, ok := m[k].(string)
			return s, ok
		}
		cur = m[k]
	}
	return "", false
}
