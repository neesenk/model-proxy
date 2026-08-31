package takeover

import (
	"fmt"
	"os"
	"strings"

	"model-proxy/internal/catalog"
	configdomain "model-proxy/internal/config"
)

// RewriteClaude: ~/.claude/settings.json
// Sets env.ANTHROPIC_BASE_URL → proxy, env.ANTHROPIC_AUTH_TOKEN → PROXY_MANAGED.
func RewriteClaude(cfg *configdomain.Config) error {
	file := cfg.Takeover.Claude
	v, err := ReadJSONConfig(file)
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
	return WriteJSONConfig(file, v)
}

// ProviderID returns the configured provider id (default "model-proxy").
func ProviderID(cfg *configdomain.Config) string {
	if cfg.Takeover.ProviderID != "" {
		return cfg.Takeover.ProviderID
	}
	return "model-proxy"
}

// ExposedModels returns all exposed model names across all protocol routes,
// with their provider model metadata (context/output/modalities). Each entry
// is {exposedName, providerName, realModel, catalog.Model}.
type ExposedModel struct {
	Exposed   string
	Provider  string
	RealModel string
	PM        catalog.Model
}

func ExposedModels(cfg *configdomain.Config, meta map[string]map[string]catalog.Model, implicit map[string]configdomain.RouteTarget) []ExposedModel {
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

// DisplayName returns a human-readable name for a model.
// Currently just returns the ID; can be extended later.
func DisplayName(id string) string {
	return id
}

// RewriteOpencode: ~/.config/opencode/opencode.json
// Writes a provider entry pointing at the proxy, with all exposed models from
// the config's routes + provider model metadata (context/output/modalities).
func RewriteOpencode(cfg *configdomain.Config, meta map[string]map[string]catalog.Model, implicit map[string]configdomain.RouteTarget) error {
	file := cfg.Takeover.Opencode
	pid := ProviderID(cfg)
	v, err := ReadJSONConfig(file)
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
	return WriteJSONConfig(file, v)
}

// opencodeModels builds the opencode model map from the config's exposed
// models (routes + hydrated metadata). Each model gets name, limit.{context,
// output}, modalities.{input,output}.
func opencodeModels(cfg *configdomain.Config, meta map[string]map[string]catalog.Model, implicit map[string]configdomain.RouteTarget) map[string]any {
	models := ExposedModels(cfg, meta, implicit)
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

// RewritePi: ~/.pi/agent/models.json
// providers.<name> = { baseUrl, api: anthropic-messages, apiKey: PROXY_MANAGED,
// models:[{id, name, contextWindow, input, maxTokens}] }
func RewritePi(cfg *configdomain.Config, meta map[string]map[string]catalog.Model, implicit map[string]configdomain.RouteTarget) error {
	file := cfg.Takeover.Pi
	name := ProviderID(cfg)
	v, err := ReadJSONConfig(file)
	if err != nil {
		return err
	}
	prov, _ := v["providers"].(map[string]any)
	if prov == nil {
		prov = map[string]any{}
	}
	models := ExposedModels(cfg, meta, implicit)
	piModels := []map[string]any{}
	for _, m := range models {
		entry := map[string]any{
			"name":      DisplayName(m.Exposed),
			"id":        m.Exposed,
			"input":     m.PM.Modalities.Input,
			"maxTokens": m.PM.Output,
		}
		if len(m.PM.Modalities.Input) == 0 {
			entry["input"] = []string{"text"}
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
	// pi's api: anthropic-messages appends /v1/messages, so baseUrl is bare proxy URL.
	baseURL := strings.TrimRight(cfg.Takeover.ProxyURL, "/")
	prov[name] = map[string]any{
		"baseUrl": baseURL,
		"api":     "anthropic-messages",
		"apiKey":  "PROXY_MANAGED",
		"models":  piModels,
	}
	v["providers"] = prov
	return WriteJSONConfig(file, v)
}

// RewriteCodex: ~/.codex/config.toml
// Text edit: set top-level model_provider=<id> and inject a [model_providers."<id>"] section.
func RewriteCodex(cfg *configdomain.Config) error {
	file := cfg.Takeover.Codex
	data, err := readFile(file)
	if err != nil {
		return err
	}
	text := string(data)
	pid := ProviderID(cfg)
	header := fmt.Sprintf(`model_providers."%s"`, pid)

	section := fmt.Sprintf(`
[model_providers."%s"]
name = "model-proxy"
base_url = "%s"
wire_api = "responses"
requires_openai_auth = true
`, pid, cfg.Takeover.ProxyURL)

	text = ReplaceOrAppendTOMLSection(text, header, section)
	text = SetTOMLTopKey(text, "model_provider", fmt.Sprintf("%q", pid))

	// Atomic like every other takeover writer: a crash mid-rewrite must not
	// leave a truncated live config.toml (pitfalls #18), and the mode is
	// preserved — never widened to a hardcoded 0644.
	return atomicWriteFile(file, []byte(text), preserveMode(file, 0o600))
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

// kimiFallbackContextSize is the max_context_size written when a model has no
// catalog metadata. kimi-cli's LLMModel schema REQUIRES max_context_size (no
// default — omitting it fails config validation), so the value mirrors the
// conservative default the proxy uses everywhere else
// (app.DefaultModelMetadata.Context = 200000; takeover cannot import app).
const kimiFallbackContextSize = 200000

// RewriteKimi: ~/.kimi/config.toml (Kimi Code CLI — the MoonshotAI/kimi-cli
// client; "openai_legacy" below names kimi-cli's OpenAI Chat Completions
// provider type, not a legacy client).
// Text edit mirroring codex's TOML handling: inject a [providers."<id>"]
// block (openai_legacy = OpenAI Chat Completions, which the proxy speaks
// natively; api_key is a sentinel — the proxy holds the real credential) and
// one [models."<exposed>"] block per exposed model pointing at the provider.
// Re-runs replace both the provider block and every model block in place.
func RewriteKimi(cfg *configdomain.Config, meta map[string]map[string]catalog.Model, implicit map[string]configdomain.RouteTarget) error {
	file := cfg.Takeover.Kimi
	data, err := readFile(file)
	if err != nil {
		return err
	}
	text := string(data)
	pid := ProviderID(cfg)

	// openai_legacy expects the versioned base (e.g. https://api.openai.com/v1):
	// kimi-cli appends /chat/completions itself.
	base := strings.TrimRight(cfg.Takeover.ProxyURL, "/") + "/v1"
	provSection := fmt.Sprintf(`
[providers."%s"]
type = "openai_legacy"
base_url = "%s"
api_key = "PROXY_MANAGED"
`, pid, base)
	text = ReplaceOrAppendTOMLSection(text, fmt.Sprintf("providers.%q", pid), provSection)

	for _, m := range ExposedModels(cfg, meta, implicit) {
		// kimi-cli's LLMModel schema requires provider, model (the wire model
		// id — the exposed name the proxy routes) and max_context_size; the
		// dotted exposed name must be quoted or TOML reads [models.glm-5.2]
		// as nested tables (models → glm-5 → "2").
		// Drop the unquoted block the old writer emitted, if still present:
		// left behind it would parse as a nested table that fails kimi-cli's
		// model validation (provider/model missing at that path).
		text = removeTOMLSection(text, "models."+m.Exposed)
		// max_context_size is REQUIRED by kimi-cli (no schema default —
		// omitting it fails config validation), so an unknown context size
		// falls back to the same conservative 200k the proxy uses elsewhere
		// (app.DefaultModelMetadata.Context) rather than dropping the key.
		ctx := m.PM.Context
		if ctx <= 0 {
			ctx = kimiFallbackContextSize
		}
		modelSection := fmt.Sprintf("\n[models.%q]\nprovider = %q\nmodel = %q\nmax_context_size = %d\n",
			m.Exposed, pid, m.Exposed, ctx)
		text = ReplaceOrAppendTOMLSection(text, fmt.Sprintf("models.%q", m.Exposed), modelSection)
	}

	return atomicWriteFile(file, []byte(text), preserveMode(file, 0o600))
}
