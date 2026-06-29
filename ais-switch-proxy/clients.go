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
	// The built-in anthropic provider hits baseURL + /messages, so baseURL ends with /v1.
	baseURL := strings.TrimRight(cfg.Takeover.ProxyURL, "/") + "/v1"
	prov[pid] = map[string]any{
		"name": "ais-switch-proxy",
		"options": map[string]any{
			"baseURL": baseURL,
			"apiKey":  "PROXY_MANAGED",
		},
		"models": opencodeModels(),
	}
	v["provider"] = prov
	return writeJSONConfig(file, v)
}

// opencode model aliases (must be in anthropic's built-in allowlist); the proxy maps them to real model names.
func opencodeModels() map[string]any {
	return map[string]any{
		"claude-opus-4-7":   map[string]any{"name": "glm-5.2 (opus)", "limit": map[string]any{"context": 200000, "output": 32768}},
		"claude-sonnet-4-6": map[string]any{"name": "deepseek-v4-pro (sonnet)", "limit": map[string]any{"context": 200000, "output": 32768}},
		"claude-haiku-4-5":  map[string]any{"name": "deepseek-v4-flash (haiku)", "limit": map[string]any{"context": 200000, "output": 32768}},
	}
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
	models := []map[string]any{}
	for _, m := range cfg.Takeover.PiModels {
		models = append(models, map[string]any{"id": m})
	}
	if len(models) == 0 {
		models = []map[string]any{{"id": "claude-opus-4-7"}}
	}
	baseURL := strings.TrimRight(cfg.Takeover.ProxyURL, "/") + "/v1"
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
