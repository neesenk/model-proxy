package main

import (
	"fmt"
	"strings"

	"model-proxy/internal/catalog"
)

// rewriteClaude: ~/.claude/settings.json
// Sets env.ANTHROPIC_BASE_URL → proxy, env.ANTHROPIC_AUTH_TOKEN → PROXY_MANAGED.
func rewriteClaude(cfg *Config) error {
	file := cfg.Takeover.Claude
	v, err := readJSONConfig(file)
	if err != nil {
		return err
	}
	env, _ := v["env"].(map[string]any)
	if env == nil {
		env = map[string]any{}
	}
	env["ANTHROPIC_BASE_URL"] = cfg.Takeover.ProxyURL
	env["ANTHROPIC_AUTH_TOKEN"] = "PROXY_MANAGED"
	v["env"] = env
	return writeJSONConfig(file, v)
}

// providerID returns the configured provider id (default "model-proxy").
func providerID(cfg *Config) string {
	if cfg.Takeover.ProviderID != "" {
		return cfg.Takeover.ProviderID
	}
	return "model-proxy"
}

// exposedModels returns all exposed model names across all protocol routes,
// with their provider model metadata (context/output/modalities). Each entry
// is {exposedName, providerName, realModel, catalog.Model}.
type exposedModel struct {
	exposed   string
	provider  string
	realModel string
	pm        catalog.Model
}

func exposedModels(cfg *Config, meta map[string]map[string]catalog.Model, implicit map[string]RouteTarget) []exposedModel {
	var out []exposedModel
	add := func(exposed string, t RouteTarget) {
		if _, ok := cfg.Providers[t.Provider]; !ok {
			return
		}
		var pm catalog.Model
		if meta[t.Provider] != nil {
			pm = meta[t.Provider][t.Model]
		}
		out = append(out, exposedModel{
			exposed:   exposed,
			provider:  t.Provider,
			realModel: t.Model,
			pm:        pm,
		})
	}
	for exposed, targets := range cfg.Routes {
		if len(targets) == 0 {
			continue
		}
		// Use the highest-priority target's provider/model for metadata.
		// Sort by priority (same logic as schedule, minus peak/circuit filtering).
		best := targets[0]
		for _, t := range targets[1:] {
			if t.Priority < best.Priority {
				best = t
			}
		}
		add(exposed, best)
	}
	// Implicit routes (auto-derived from logged-in providers' model lists) are
	// callable through the proxy and listed in /v1/models — include them so the
	// takeover client config matches. Explicit routes win on name collision.
	for exposed, t := range implicit {
		if _, explicit := cfg.Routes[exposed]; explicit {
			continue
		}
		add(exposed, t)
	}
	return out
}

// displayName returns a human-readable name for a model.
// Currently just returns the ID; can be extended later.
func displayName(id string) string {
	return id
}

// rewriteOpencode: ~/.config/opencode/opencode.json
// Writes a provider entry pointing at the proxy, with all exposed models from
// the config's routes + provider model metadata (context/output/modalities).
func rewriteOpencode(cfg *Config, meta map[string]map[string]catalog.Model, implicit map[string]RouteTarget) error {
	file := cfg.Takeover.Opencode
	pid := providerID(cfg)
	v, err := readJSONConfig(file)
	if err != nil {
		return err
	}
	prov, _ := v["provider"].(map[string]any)
	if prov == nil {
		prov = map[string]any{}
	}
	// opencode's @ai-sdk/anthropic appends /messages to baseURL, so baseURL
	// ends with /v1 (→ <proxy>/v1/messages). Needs npm for non-built-in id.
	baseURL := strings.TrimRight(cfg.Takeover.ProxyURL, "/") + "/v1"
	prov[pid] = map[string]any{
		"name": "model-proxy",
		"npm":  "@ai-sdk/anthropic",
		"options": map[string]any{
			"apiKey":  "PROXY_MANAGED",
			"baseURL": baseURL,
		},
		"models": opencodeModels(cfg, meta, implicit),
	}
	v["provider"] = prov
	return writeJSONConfig(file, v)
}

// opencodeModels builds the opencode model map from the config's exposed
// models (routes + hydrated metadata). Each model gets name, limit.{context,
// output}, modalities.{input,output}.
func opencodeModels(cfg *Config, meta map[string]map[string]catalog.Model, implicit map[string]RouteTarget) map[string]any {
	models := exposedModels(cfg, meta, implicit)
	out := make(map[string]any, len(models))
	for _, m := range models {
		name := displayName(m.exposed)
		limit := map[string]any{}
		if m.pm.Context > 0 {
			limit["context"] = m.pm.Context
		}
		if m.pm.Output > 0 {
			limit["output"] = m.pm.Output
		}
		modalities := map[string]any{
			"input":  m.pm.Modalities.Input,
			"output": m.pm.Modalities.Output,
		}
		// Default modalities if empty.
		if len(m.pm.Modalities.Input) == 0 {
			modalities["input"] = []string{"text"}
		}
		if len(m.pm.Modalities.Output) == 0 {
			modalities["output"] = []string{"text"}
		}
		out[m.exposed] = map[string]any{
			"name":       name,
			"limit":      limit,
			"modalities": modalities,
		}
	}
	return out
}

// rewritePi: ~/.pi/agent/models.json
// providers.<name> = { baseUrl, api: anthropic-messages, apiKey: PROXY_MANAGED,
// models:[{id, name, contextWindow, input, maxTokens}] }
func rewritePi(cfg *Config, meta map[string]map[string]catalog.Model, implicit map[string]RouteTarget) error {
	file := cfg.Takeover.Pi
	name := providerID(cfg)
	v, err := readJSONConfig(file)
	if err != nil {
		return err
	}
	prov, _ := v["providers"].(map[string]any)
	if prov == nil {
		prov = map[string]any{}
	}
	models := exposedModels(cfg, meta, implicit)
	piModels := []map[string]any{}
	for _, m := range models {
		entry := map[string]any{
			"name":      displayName(m.exposed),
			"id":        m.exposed,
			"input":     m.pm.Modalities.Input,
			"maxTokens": m.pm.Output,
		}
		if len(m.pm.Modalities.Input) == 0 {
			entry["input"] = []string{"text"}
		}
		if m.pm.Output == 0 {
			entry["maxTokens"] = 4096
		}
		if m.pm.Context > 0 {
			entry["contextWindow"] = m.pm.Context
		}
		piModels = append(piModels, entry)
	}
	if len(piModels) == 0 {
		piModels = []map[string]any{{"id": "glm-5.2", "name": "glm-5.2", "input": []string{"text"}, "maxTokens": 4096}}
	}
	// pi's api: anthropic-messages appends /v1/messages, so baseUrl is bare proxy URL.
	baseURL := strings.TrimRight(cfg.Takeover.ProxyURL, "/")
	prov[name] = map[string]any{
		"baseUrl": baseURL,
		"api":     "anthropic-messages",
		"apiKey":  "PROXY_MANAGED",
		"models":  piModels,
	}
	v["providers"] = prov
	return writeJSONConfig(file, v)
}

// rewriteCodex: ~/.codex/config.toml
// Text edit: set top-level model_provider=<id> and inject a [model_providers."<id>"] section.
func rewriteCodex(cfg *Config) error {
	file := cfg.Takeover.Codex
	data, err := readFile(file)
	if err != nil {
		return err
	}
	text := string(data)
	pid := providerID(cfg)
	header := fmt.Sprintf(`model_providers."%s"`, pid)

	section := fmt.Sprintf(`
[model_providers."%s"]
name = "model-proxy"
base_url = "%s"
wire_api = "responses"
requires_openai_auth = true
`, pid, cfg.Takeover.ProxyURL)

	text = replaceOrAppendTOMLSection(text, header, section)
	text = setTOMLTopKey(text, "model_provider", fmt.Sprintf("%q", pid))

	return writeFile(file, []byte(text), 0o644)
}

// setTOMLTopKey sets a top-level bare key (placed before any [section]).
func setTOMLTopKey(text, key, val string) string {
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
		if strings.HasPrefix(strings.TrimSpace(lines[i]), prefix) {
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

// replaceOrAppendTOMLSection replaces an existing [section] block, or appends a new one.
func replaceOrAppendTOMLSection(text, sectionHeader, section string) string {
	header := "[" + sectionHeader + "]"
	idx := strings.Index(text, header)
	if idx >= 0 {
		rest := text[idx+len(header):]
		nextSec := strings.Index(rest, "\n[")
		var end int
		if nextSec >= 0 {
			end = idx + len(header) + nextSec
		} else {
			end = len(text)
		}
		text = text[:idx] + strings.TrimSpace(section) + "\n" + text[end:]
		return text
	}
	if len(text) > 0 && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	text += strings.TrimSpace(section) + "\n"
	return text
}
