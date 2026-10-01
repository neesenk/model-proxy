package takeover

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"model-proxy/internal/routing"
)

// workbuddy.go — models shape "workbuddy": renders model-proxy's exposed
// models as local custom models in WorkBuddy's ~/.workbuddy/models.json
// (Tencent WorkBuddy desktop; schema from the app's
// isValidLocalCustomModel: id required; name/vendor/url/apiKey strings;
// maxInputTokens/maxOutputTokens/temperature numeric; supportsToolCall/
// supportsImages/supportsReasoning/onlyReasoning/useCustomProtocol booleans).
// The file is an ARRAY of model entries (or {models: [...]} — the app's
// extractLocalModels accepts both), so the generic json.set map write cannot
// express it: this file owns the merge — entries carrying OUR vendor marker
// are dropped and re-appended, the user's own custom models are preserved.
// WorkBuddy speaks OpenAI Chat Completions only: the agent normalizes every
// custom-model URL to end in /chat/completions, so url is the /v1 base.

// rewriteWorkbuddy renders the workbuddy template: the custom-model array
// into the main models.json, the gateway MCP surface into the aux mcp.json.
// Mirrors rewriteJSON's scope/aux contract on a file whose document root may
// be an array (ReadJSONConfig only handles objects).
func (t *Template) rewriteWorkbuddy(ctx renderContext, scope RewriteScope) error {
	mcpDst := t.mcpFile()
	if scope == ScopeMCP && mcpDst != "" && mcpDst != t.File {
		return t.writeMCPAux(mcpDst, ctx)
	}
	if scope != ScopeMCP {
		if err := t.mergeWorkbuddyModels(ctx); err != nil {
			return err
		}
	}
	if mcpDst == "" || scope == ScopeModel {
		return nil
	}
	return t.writeMCPAux(mcpDst, ctx)
}

// mergeWorkbuddyModels upserts the rendered custom-model entries into
// models.json. The document is written back in the {models: [...]} form
// (an array-rooted original is lifted into it — the app accepts both).
// vendor = our provider id marks takeover-owned entries: a re-takeover
// drops and re-renders exactly those (model shrink, URL change), everything
// else is the user's and stays.
func (t *Template) mergeWorkbuddyModels(ctx renderContext) error {
	v := map[string]any{}
	data, err := readFile(t.File)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
	} else if len(strings.TrimSpace(string(data))) > 0 {
		var raw any
		if err := json.Unmarshal(data, &raw); err != nil {
			return fmt.Errorf("template %s: %s: %w", t.Name, t.File, err)
		}
		switch doc := raw.(type) {
		case map[string]any:
			v = doc
		case []any:
			v = map[string]any{"models": doc}
		default:
			return fmt.Errorf("template %s: %s: top level is %T, want an array or object", t.Name, t.File, raw)
		}
	}
	existing, _ := v["models"].([]any)
	if v["models"] != nil && existing == nil {
		return fmt.Errorf("template %s: %s: models is %T, want an array — refusing to rewrite a broken config", t.Name, t.File, v["models"])
	}
	kept := make([]any, 0, len(existing))
	for _, e := range existing {
		if m, ok := e.(map[string]any); ok && m["vendor"] == ctx.providerID {
			continue
		}
		kept = append(kept, e)
	}
	v["models"] = append(kept, workbuddyModelEntries(ctx)...)
	return WriteJSONConfig(t.File, v)
}

// workbuddyModelEntries renders one custom-model entry per exposed model,
// with the same conservative metadata contract as the other shapes: context/
// output fall back to the defaults, capabilities are written only when
// models.dev asserts them (an absent capability is simply not set — the app
// type-checks optional booleans, so unsupported means absent).
func workbuddyModelEntries(ctx renderContext) []any {
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
		entry := map[string]any{
			"id":              m.Exposed,
			"name":            m.Exposed,
			"vendor":          ctx.providerID,
			"url":             ctx.baseURL,
			"apiKey":          "PROXY_MANAGED",
			"maxInputTokens":  contextN,
			"maxOutputTokens": output,
		}
		if m.PM.ToolCall {
			entry["supportsToolCall"] = true
		}
		for _, in := range m.PM.Modalities.Input {
			if in == "image" {
				entry["supportsImages"] = true
			}
		}
		if m.PM.Reasoning {
			entry["supportsReasoning"] = true
		}
		out = append(out, entry)
	}
	return out
}
