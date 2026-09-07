package takeover

import (
	"os"
	"strings"

	"model-proxy/internal/catalog"
	configdomain "model-proxy/internal/config"
)

// 客户端改写已模板化(template.go + presets/)。本文件保留模板引擎与外部
// 共用的纯函数:暴露模型枚举、JSON 模型集合渲染器(opencode/pi 形状)、
// TOML 文本编辑 helper 与文件 I/O helper。

// ExposedModels returns all exposed model names across all protocol routes,
// with their provider model metadata (context/output/modalities). Each entry
// is {exposedName, providerName, realModel, catalog.Model}.
type ExposedModel struct {
	Exposed   string
	Provider  string
	RealModel string
	PM        catalog.Model
}

func ExposedModels(cfg *configdomain.Config, meta map[string]map[string]catalog.Model, routes map[string][]configdomain.RouteTarget) []ExposedModel {
	var out []ExposedModel
	add := func(exposed string, t configdomain.RouteTarget) {
		if _, ok := cfg.Providers[t.Provider]; !ok {
			return
		}
		var pm catalog.Model
		if meta[t.Provider] != nil {
			pm = meta[t.Provider][t.Model]
		}
		out = append(out, ExposedModel{
			Exposed:   exposed,
			Provider:  t.Provider,
			RealModel: t.Model,
			PM:        pm,
		})
	}
	for exposed, targets := range routes {
		if len(targets) == 0 {
			continue
		}
		// Use the highest-priority target's provider/model for metadata.
		add(exposed, primaryTarget(targets))
	}
	return out
}

// primaryTarget returns the target a request tries first (lowest priority
// value — the same rule schedule applies, minus peak/circuit filtering).
func primaryTarget(targets []configdomain.RouteTarget) configdomain.RouteTarget {
	best := targets[0]
	for _, t := range targets[1:] {
		if t.Priority < best.Priority {
			best = t
		}
	}
	return best
}

// DisplayName returns a human-readable name for a model.
// Currently just returns the ID; can be extended later.
func DisplayName(id string) string {
	return id
}

// opencodeModelsCollection builds the opencode model map from the exposed
// models (routes + hydrated metadata). Each model gets name, limit.{context,
// output}, modalities.{input,output}.
func opencodeModelsCollection(models []ExposedModel) map[string]any {
	out := make(map[string]any, len(models))
	for _, m := range models {
		name := DisplayName(m.Exposed)
		limit := map[string]any{}
		if m.PM.Context > 0 {
			limit["context"] = m.PM.Context
		}
		if m.PM.Output > 0 {
			limit["output"] = m.PM.Output
		}
		modalities := map[string]any{
			"input":  m.PM.Modalities.Input,
			"output": m.PM.Modalities.Output,
		}
		// Default modalities if empty.
		if len(m.PM.Modalities.Input) == 0 {
			modalities["input"] = []string{"text"}
		}
		if len(m.PM.Modalities.Output) == 0 {
			modalities["output"] = []string{"text"}
		}
		out[m.Exposed] = map[string]any{
			"name":       name,
			"limit":      limit,
			"modalities": modalities,
		}
	}
	return out
}

// piInputModalities projects catalog input modalities onto pi's models.json
// schema, which only accepts "text" and "image" (Literal union). models.dev
// also lists "pdf"/"video" for many models — passing those through writes an
// invalid models.json and pi refuses to START, so anything else is dropped.
func piInputModalities(in []string) []string {
	out := make([]string, 0, len(in))
	for _, m := range in {
		if m == "text" || m == "image" {
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		return []string{"text"}
	}
	return out
}

// piModelsCollection builds pi's model list: {id, name, input, maxTokens,
// contextWindow} per exposed model, with conservative defaults for missing
// metadata and one fallback entry when the route table is empty.
func piModelsCollection(models []ExposedModel) []map[string]any {
	piModels := []map[string]any{}
	for _, m := range models {
		entry := map[string]any{
			"name":      DisplayName(m.Exposed),
			"id":        m.Exposed,
			"input":     piInputModalities(m.PM.Modalities.Input),
			"maxTokens": m.PM.Output,
		}
		// reasoning is what makes pi send reasoning params and surface
		// thinking blocks; omitting it silently disables thinking for
		// reasoning-capable models. Only asserted true — pi defaults false.
		if m.PM.Reasoning {
			entry["reasoning"] = true
		}
		if m.PM.Output == 0 {
			entry["maxTokens"] = 4096
		}
		if m.PM.Context > 0 {
			entry["contextWindow"] = m.PM.Context
		}
		piModels = append(piModels, entry)
	}
	if len(piModels) == 0 {
		piModels = []map[string]any{{"id": "glm-5.2", "name": "glm-5.2", "input": []string{"text"}, "maxTokens": 4096}}
	}
	return piModels
}

// SetTOMLTopKey sets a top-level bare key (placed before any [section]).
// The match is whitespace-tolerant: a pre-existing `key="x"` line (no spaces
// around `=`) is replaced in place just like `key = "x"` — matching only the
// spaced prefix would insert a duplicate key, which TOML rejects with a
// parse error (bricking the whole client config). The scan stays
// line-oriented and conservative: only a line whose pre-`=` token is exactly
// the key (optionally quoted, since TOML treats "key" and key alike)
// qualifies; a `=` inside a value can never produce that shape.
func SetTOMLTopKey(text, key, val string) string {
	lines := strings.Split(text, "\n")
	firstSection := -1
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "[") {
			firstSection = i
			break
		}
	}
	prefix := key + " = "
	for i := 0; i < len(lines); i++ {
		if firstSection >= 0 && i >= firstSection {
			break
		}
		l := strings.TrimSpace(lines[i])
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		eq := strings.IndexByte(l, '=')
		if eq < 0 {
			continue
		}
		k := strings.Trim(strings.TrimSpace(l[:eq]), `"`)
		if k == key {
			lines[i] = prefix + val
			return strings.Join(lines, "\n")
		}
	}
	newLine := prefix + val
	if firstSection >= 0 {
		lines = append(lines[:firstSection], append([]string{newLine}, lines[firstSection:]...)...)
	} else {
		lines = append(lines, newLine)
	}
	return strings.Join(lines, "\n")
}

// ReplaceOrAppendTOMLSection replaces an existing [section] block, or appends a new one.
// The header must match a whole (trimmed) line — a substring search would also
// hit the header text embedded in a quoted value (e.g. `x = "[foo]"`) and
// corrupt the file.
func ReplaceOrAppendTOMLSection(text, sectionHeader, section string) string {
	header := "[" + sectionHeader + "]"
	lines := strings.Split(text, "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == header {
			start = i
			break
		}
	}
	if start >= 0 {
		// The old section body runs until the next header line (or EOF).
		end := len(lines)
		for i := start + 1; i < len(lines); i++ {
			if strings.HasPrefix(strings.TrimSpace(lines[i]), "[") {
				end = i
				break
			}
		}
		out := make([]string, 0, len(lines))
		out = append(out, lines[:start]...)
		out = append(out, strings.Split(strings.TrimSpace(section), "\n")...)
		out = append(out, lines[end:]...)
		return strings.Join(out, "\n")
	}
	if len(text) > 0 && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	text += strings.TrimSpace(section) + "\n"
	return text
}

// removeTOMLSection drops an entire [section] block (header + body up to the
// next header line or EOF). No-op when the header is absent.
func removeTOMLSection(text, sectionHeader string) string {
	header := "[" + sectionHeader + "]"
	lines := strings.Split(text, "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == header {
			start = i
			break
		}
	}
	if start < 0 {
		return text
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "[") {
			end = i
			break
		}
	}
	out := make([]string, 0, len(lines))
	out = append(out, lines[:start]...)
	out = append(out, lines[end:]...)
	return strings.Join(out, "\n")
}

// readFile is a tiny local I/O helper kept here so the package has no
// application dependency.
func readFile(path string) ([]byte, error) { return os.ReadFile(path) }

// The kimi fallback context below mirrors routing.DefaultModelMetadata.Context:
// kimi-cli's LLMModel schema REQUIRES max_context_size (no default — omitting
// it fails config validation), so the value is the same conservative default
// the proxy uses everywhere else, taken from its routing-package owner.
