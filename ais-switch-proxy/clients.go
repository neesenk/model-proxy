package main

import (
	"fmt"
	"strings"
)

// rewriteClaude: ~/.claude/settings.json
// Sets env.ANTHROPIC_BASE_URL → proxy, env.ANTHROPIC_AUTH_TOKEN → PROXY_MANAGED.
func rewriteClaude(cfg *Config) error {
	file := cfg.Takeover.ClaudeFile
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

// rewriteOpencode: ~/.config/opencode/opencode.json
// Sets provider.<id>.options.{baseURL,apiKey} → proxy.
func rewriteOpencode(cfg *Config) error {
	file := cfg.Takeover.OpencodeFile
	pid := cfg.Takeover.OpencodeProviderID
	if pid == "" {
		pid = "anthropic"
	}
	v, err := readJSONConfig(file)
	if err != nil {
		return err
	}
	prov, _ := v["provider"].(map[string]any)
	if prov == nil {
		prov = map[string]any{}
	}
	// opencode's @ai-sdk/anthropic appends /messages to baseURL, so baseURL ends
	// with /v1 (→ <proxy>/v1/messages). For a non-built-in provider id, opencode
	// needs `npm` to point at the SDK implementation; @ai-sdk/anthropic ships in
	// the opencode binary.
	baseURL := strings.TrimRight(cfg.Takeover.ProxyURL, "/") + "/v1"
	prov[pid] = map[string]any{
		"name":  "AIS Switch",
		"npm":   "@ai-sdk/anthropic",
		"options": map[string]any{
			"apiKey":  "PROXY_MANAGED",
			"baseURL": baseURL,
		},
		"models": opencodeModels(cfg),
	}
	v["provider"] = prov
	return writeJSONConfig(file, v)
}

// gatewayModels returns the real model IDs to expose to clients (pi/opencode),
// sourced from the models cache (refreshed by `serve`/`models`). Falls back to a
// hardcoded default if the cache is absent. Clients send these real names; the
// proxy passes them through (no model_map aliasing needed).
func gatewayModels(cfg *Config) []string {
	if c, _ := loadModelsCache(modelsCachePath(cfg)); c != nil && len(c.Data) > 0 {
		ids := make([]string, 0, len(c.Data))
		for _, m := range c.Data {
			ids = append(ids, m.ID)
		}
		return ids
	}
	// Fallback: the known gateway models.
	return []string{"glm-5.2", "deepseek-v4-pro", "deepseek-v4-flash"}
}

// opencodeModels builds the opencode provider model map using real gateway model
// IDs (no aliasing). Fields mirror the verified opencode config template:
// name (from pricing table), limit.{context,output}, modalities.{input,output}.
// context: config model_limits.context > gateway models cache > omitted.
// output/modalities: config model_limits, falling back to defaults.
func opencodeModels(cfg *Config) map[string]any {
	cache, _ := loadModelsCache(modelsCachePath(cfg))
	ids := gatewayModels(cfg)
	out := make(map[string]any, len(ids))
	for _, id := range ids {
		name := id
		if p := lookupPricing(id); p != nil && p.DisplayName != "" {
			name = p.DisplayName
		}
		ml := cfg.Takeover.modelLimit(id)
		ctx := ml.Context
		if ctx == 0 && cache != nil {
			for _, m := range cache.Data {
				if m.ID == id && m.ContextWindow > 0 {
					ctx = m.ContextWindow
					break
				}
			}
		}
		limit := map[string]any{"output": ml.OutputTokens}
		if ctx > 0 {
			limit["context"] = ctx
		}
		out[id] = map[string]any{
			"name":  name,
			"limit": limit,
			"modalities": map[string]any{
				"input":  ml.Input,
				"output": ml.Output,
			},
		}
	}
	return out
}

// rewritePi: ~/.pi/agent/models.json
// providers.<name> = { baseUrl, api: anthropic-messages, apiKey: PROXY_MANAGED, models:[{id}] }
func rewritePi(cfg *Config) error {
	file := cfg.Takeover.PiFile
	name := cfg.Takeover.PiProviderName
	if name == "" {
		name = "ais-switch-proxy"
	}
	v, err := readJSONConfig(file)
	if err != nil {
		return err
	}
	prov, _ := v["providers"].(map[string]any)
	if prov == nil {
		prov = map[string]any{}
	}
	// Use real gateway model IDs (from the cache) unless PiModels is explicitly
	// configured (an override). Real names pass through the proxy without aliasing.
	var modelIDs []string
	if len(cfg.Takeover.PiModels) > 0 {
		modelIDs = cfg.Takeover.PiModels
	} else {
		modelIDs = gatewayModels(cfg)
	}
	cache, _ := loadModelsCache(modelsCachePath(cfg))
	models := []map[string]any{}
	for _, id := range modelIDs {
		name := id
		if p := lookupPricing(id); p != nil && p.DisplayName != "" {
			name = p.DisplayName
		}
		ml := cfg.Takeover.modelLimit(id)
		ctx := ml.Context
		if ctx == 0 && cache != nil {
			for _, m := range cache.Data {
				if m.ID == id && m.ContextWindow > 0 {
					ctx = m.ContextWindow
					break
				}
			}
		}
		entry := map[string]any{
			"name":      name,
			"id":        id,
			"input":     ml.Input,
			"maxTokens": ml.OutputTokens,
		}
		if ctx > 0 {
			entry["contextWindow"] = ctx
		}
		models = append(models, entry)
	}
	if len(models) == 0 {
		ml := defaultModelLimit
		models = []map[string]any{{"id": "glm-5.2", "name": "glm-5.2", "input": ml.Input, "maxTokens": ml.OutputTokens}}
	}
	// pi's `api: anthropic-messages` appends /v1/messages itself, so baseUrl must
	// be the bare proxy URL WITHOUT /v1 (otherwise pi hits /v1/v1/messages → 502).
	baseURL := strings.TrimRight(cfg.Takeover.ProxyURL, "/")
	prov[name] = map[string]any{
		"baseUrl": baseURL,
		"api":     "anthropic-messages",
		"apiKey":  "PROXY_MANAGED",
		"models":  models,
	}
	v["providers"] = prov
	return writeJSONConfig(file, v)
}

// rewriteCodex: ~/.codex/config.toml
// Text edit: set top-level model_provider=ais_switch_proxy and inject a [model_providers.ais_switch_proxy] section.
func rewriteCodex(cfg *Config) error {
	file := cfg.Takeover.CodexFile
	data, err := readFile(file)
	if err != nil {
		return err
	}
	text := string(data)

	// Inject / replace the model_providers.ais_switch_proxy section.
	section := fmt.Sprintf(`
[model_providers.ais_switch_proxy]
name = "AIS Switch Proxy"
base_url = "%s"
wire_api = "responses"
requires_openai_auth = true
`, cfg.Takeover.ProxyURL)

	text = replaceOrAppendTOMLSection(text, "model_providers.ais_switch_proxy", section)

	// Set the top-level model_provider.
	text = setTOMLTopKey(text, "model_provider", `"ais_switch_proxy"`)

	return writeFile(file, []byte(text), 0o644)
}

// setTOMLTopKey sets a top-level bare key (placed before any [section]).
func setTOMLTopKey(text, key, val string) string {
	lines := strings.Split(text, "\n")
	// Find the first section line.
	firstSection := -1
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "[") {
			firstSection = i
			break
		}
	}
	// Replace an existing top-level key if present.
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
	// Otherwise insert before the first section (or at the end).
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
		// Truncate at the next section or end of file.
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
