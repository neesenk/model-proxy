package takeover

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"model-proxy/internal/catalog"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/routing"

	"gopkg.in/yaml.v3"
)

// takeover 客户端模板:把"改写哪个客户端配置文件、写什么"从硬编码 Rewrite
// 函数抽成声明式 YAML。内置预设在 presets/,用户可在
// ~/.model-proxy/takeover-templates/<name>.yaml 放同名文件覆盖预设或新增客户端。
// 模板渲染是幂等的:重复 takeover 产生相同结果(replace-or-append / key set)。

// Template describes one coding-agent client config rewrite.
type Template struct {
	// Name is the template id — the file's base name without .yaml.
	Name        string `yaml:"-"`
	Description string `yaml:"description"`
	// File is the client config path ("~" expanded at load).
	File string `yaml:"file"`
	// Format: json | toml | env.
	Format string `yaml:"format"`
	// BaseURL: bare (default, proxy URL as-is) | v1 (append /v1).
	BaseURL string `yaml:"base_url"`
	// ProviderID defaults to "model-proxy" — variants writing into the same
	// client file (pi-openai vs pi-responses) must pick distinct ids.
	ProviderID string `yaml:"provider_id"`
	// ProxyURL overrides the default http://<listen>.
	ProxyURL    string `yaml:"proxy_url"`
	DisplayName string `yaml:"display_name"` // default "model-proxy"

	// Client groups the variants of one agent family (pi / pi-openai /
	// pi-responses all carry client: pi). Empty defaults to the template
	// name — a family of one. `takeover <family>` and `takeover all`
	// auto-select ONE variant per family (see select.go).
	Client string `yaml:"client"`
	// Protocol declares the agent-facing wire protocol this variant writes:
	// anthropic | openai | responses. Selection prefers the variant whose
	// protocol the route providers serve natively (routing.NativeProtocols),
	// so the agent talks the protocol that needs no conversion. Required —
	// and unique — within multi-variant families; optional for a
	// single-variant family (informational only).
	Protocol string `yaml:"protocol"`

	JSON   *JSONTemplate   `yaml:"json"`
	TOML   *TOMLTemplate   `yaml:"toml"`
	Env    *EnvTemplate    `yaml:"env"`
	Models *ModelsTemplate `yaml:"models"`

	// Source is where the template was loaded from: "preset" or the user
	// templates directory. Filled by LoadTemplates, used by `takeover list`.
	Source string `yaml:"-"`
}

// JSONTemplate patches a JSON object: each Set entry is a dotted path → value.
// Values may be nested maps/lists; placeholders in strings ({{base_url}},
// {{token}}, {{provider_id}}, {{display_name}}, {{proxy_url}}) are substituted.
type JSONTemplate struct {
	Set map[string]any `yaml:"set"`
	// DriftPath is the JSON path doctor reads to detect takeover drift
	// (e.g. env.ANTHROPIC_BASE_URL); its expected value is the rendered base URL.
	DriftPath string `yaml:"drift_path"`
}

// TOMLTemplate patches a TOML file with text edits (no decoder — line-oriented,
// same helpers the hand-written rewrites used).
type TOMLTemplate struct {
	TopKeys  map[string]string `yaml:"top_keys"`
	Sections []TOMLSection     `yaml:"sections"`
}

type TOMLSection struct {
	Name string `yaml:"name"`
	Body string `yaml:"body"`
}

// EnvTemplate patches a KEY=VALUE .env-style file (comments and key order of
// untouched lines are preserved; managed keys are replaced in place or appended).
type EnvTemplate struct {
	Set map[string]string `yaml:"set"`
}

// ModelsTemplate describes per-exposed-model output. Shape selects the Go-side
// collection renderer (metadata defaults included); kimi renders one TOML
// section per model instead of a JSON collection.
type ModelsTemplate struct {
	Shape string `yaml:"shape"` // opencode | pi | kimi
	// JSONPath is where the rendered collection is stored (shape opencode/pi).
	JSONPath string `yaml:"json_path"`
	// TOMLSection is the per-model section name pattern (shape kimi);
	// TOMLBody its body. Both may use {{model.id}} / {{model.context}} /
	// {{model.output}} / {{provider_id}}.
	TOMLSection string `yaml:"toml_section"`
	TOMLBody    string `yaml:"toml_body"`
	// AlsoRemove names a legacy section dropped before writing each model
	// (kimi's old unquoted block).
	AlsoRemove string `yaml:"also_remove"`
}

// DefaultTemplatesDir is the user template root; <dir>/<name>.yaml overrides
// the preset of the same name or adds a new client.
func DefaultTemplatesDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "~"
	}
	return filepath.Join(home, ".model-proxy", "takeover-templates")
}

func expandHome(p string) string {
	if p == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
	}
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// ParseTemplate decodes + validates one template document. name is the
// template id (file base name); source is recorded for `takeover list`.
func ParseTemplate(name, source string, data []byte) (*Template, error) {
	var t Template
	if err := yaml.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("template %s: parse yaml: %w", name, err)
	}
	t.Name = name
	t.Source = source
	if t.File == "" {
		return nil, fmt.Errorf("template %s: file is required (client config path)", name)
	}
	t.File = expandHome(t.File)
	switch t.Format {
	case "json":
		if t.JSON == nil || len(t.JSON.Set) == 0 {
			return nil, fmt.Errorf("template %s: format json requires non-empty json.set", name)
		}
	case "toml":
		if t.TOML == nil || (len(t.TOML.TopKeys) == 0 && len(t.TOML.Sections) == 0) {
			return nil, fmt.Errorf("template %s: format toml requires toml.top_keys and/or toml.sections", name)
		}
	case "env":
		if t.Env == nil || len(t.Env.Set) == 0 {
			return nil, fmt.Errorf("template %s: format env requires non-empty env.set", name)
		}
	default:
		return nil, fmt.Errorf("template %s: format must be json|toml|env, got %q", name, t.Format)
	}
	if t.BaseURL != "" && t.BaseURL != "bare" && t.BaseURL != "v1" {
		return nil, fmt.Errorf("template %s: base_url must be bare|v1, got %q", name, t.BaseURL)
	}
	switch t.Protocol {
	case "", "anthropic", "openai", "responses":
	default:
		return nil, fmt.Errorf("template %s: protocol must be anthropic|openai|responses, got %q", name, t.Protocol)
	}
	if t.Models != nil {
		switch t.Models.Shape {
		case "opencode", "pi":
			if t.Format != "json" {
				return nil, fmt.Errorf("template %s: models shape %s requires format json", name, t.Models.Shape)
			}
			if t.Models.JSONPath == "" {
				return nil, fmt.Errorf("template %s: models.json_path is required for shape %s", name, t.Models.Shape)
			}
		case "kimi":
			if t.Format != "toml" || t.Models.TOMLSection == "" || t.Models.TOMLBody == "" {
				return nil, fmt.Errorf("template %s: models shape kimi requires format toml + toml_section + toml_body", name)
			}
		default:
			return nil, fmt.Errorf("template %s: models.shape must be opencode|pi|kimi, got %q", name, t.Models.Shape)
		}
	}
	return &t, nil
}

// LoadTemplates returns presets overridden by any same-named user templates in
// userDir ("" disables user templates). Result is sorted by name.
func LoadTemplates(userDir string) ([]*Template, error) {
	templates, err := presetTemplates()
	if err != nil {
		return nil, err
	}
	if userDir != "" {
		entries, err := os.ReadDir(userDir)
		if err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("read templates dir: %w", err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
				continue
			}
			name := strings.TrimSuffix(e.Name(), ".yaml")
			data, err := os.ReadFile(filepath.Join(userDir, e.Name()))
			if err != nil {
				return nil, fmt.Errorf("template %s: %w", name, err)
			}
			t, err := ParseTemplate(name, userDir, data)
			if err != nil {
				return nil, err
			}
			templates[name] = t
		}
	}
	out := make([]*Template, 0, len(templates))
	for _, t := range templates {
		out = append(out, t)
	}
	sortTemplates(out)
	if err := validateFamilies(out); err != nil {
		return nil, err
	}
	return out, nil
}

// ClientFamily returns the family this template belongs to: the explicit
// client: id, or the template name itself (family of one).
func (t *Template) ClientFamily() string {
	if t.Client != "" {
		return t.Client
	}
	return t.Name
}

// validateFamilies enforces the multi-variant family contract: every variant
// of a family with more than one member must declare a protocol, and the
// protocols must be distinct — otherwise protocol-based auto-selection has
// nothing (or an ambiguous something) to key on.
func validateFamilies(templates []*Template) error {
	families := map[string][]*Template{}
	for _, t := range templates {
		f := t.ClientFamily()
		families[f] = append(families[f], t)
	}
	for family, variants := range families {
		if len(variants) < 2 {
			continue
		}
		seen := map[string]string{}
		for _, v := range variants {
			if v.Protocol == "" {
				return fmt.Errorf("template %s: family %q has %d variants — each must declare a protocol", v.Name, family, len(variants))
			}
			if prev, dup := seen[v.Protocol]; dup {
				return fmt.Errorf("templates %s and %s: family %q declares protocol %q twice — variants must be protocol-distinct", prev, v.Name, family, v.Protocol)
			}
			seen[v.Protocol] = v.Name
		}
	}
	return nil
}

// TemplateByName resolves one template (preset, or user override in
// templatesDir — "" = presets only) and returns it. Callers may adjust the
// returned copy (e.g. point File at a temp path in tests).
func TemplateByName(name, templatesDir string) (*Template, error) {
	templates, err := LoadTemplates(templatesDir)
	if err != nil {
		return nil, err
	}
	for _, t := range templates {
		if t.Name == name {
			return t, nil
		}
	}
	return nil, fmt.Errorf("unknown takeover template %q", name)
}

// ---- rendering ----

// renderContext carries the per-run values placeholders resolve to.
type renderContext struct {
	proxyURL    string
	baseURL     string
	providerID  string
	displayName string
	models      []ExposedModel
	meta        map[string]map[string]catalog.Model
	routes      map[string][]configdomain.RouteTarget
}

func (t *Template) contextFor(cfg *configdomain.Config, meta map[string]map[string]catalog.Model, routes map[string][]configdomain.RouteTarget) renderContext {
	proxyURL := t.ProxyURL
	if proxyURL == "" {
		proxyURL = "http://" + cfg.Listen
	}
	proxyURL = strings.TrimRight(proxyURL, "/")
	baseURL := proxyURL
	if t.BaseURL == "v1" {
		baseURL += "/v1"
	}
	pid := t.ProviderID
	if pid == "" {
		pid = "model-proxy"
	}
	display := t.DisplayName
	if display == "" {
		display = "model-proxy"
	}
	return renderContext{
		proxyURL:    proxyURL,
		baseURL:     baseURL,
		providerID:  pid,
		displayName: display,
		models:      ExposedModels(cfg, meta, routes),
		meta:        meta,
		routes:      routes,
	}
}

func (c renderContext) substitute(s string) string {
	return strings.NewReplacer(
		"{{proxy_url}}", c.proxyURL,
		"{{base_url}}", c.baseURL,
		"{{token}}", "PROXY_MANAGED",
		"{{provider_id}}", c.providerID,
		"{{display_name}}", c.displayName,
	).Replace(s)
}

// Rewrite renders the template into the client config file (creating it when
// absent), preserving unrelated content. ClientSpec.Rewrite adapter.
func (t *Template) Rewrite(cfg *configdomain.Config, meta map[string]map[string]catalog.Model, routes map[string][]configdomain.RouteTarget) error {
	ctx := t.contextFor(cfg, meta, routes)
	switch t.Format {
	case "json":
		return t.rewriteJSON(ctx)
	case "toml":
		return t.rewriteTOML(ctx)
	case "env":
		return t.rewriteEnv(ctx)
	}
	return fmt.Errorf("template %s: unknown format %q", t.Name, t.Format)
}

func (t *Template) rewriteJSON(ctx renderContext) error {
	v, err := ReadJSONConfig(t.File)
	if err != nil {
		return err
	}
	for path, value := range t.JSON.Set {
		setDotted(v, strings.Split(ctx.substitute(path), "."), substituteValue(value, ctx.substitute))
	}
	if t.Models != nil {
		setDotted(v, strings.Split(ctx.substitute(t.Models.JSONPath), "."), t.Models.renderCollection(ctx))
	}
	return WriteJSONConfig(t.File, v)
}

// substituteValue walks decoded YAML values, substituting placeholders in
// every string leaf.
func substituteValue(v any, sub func(string) string) any {
	switch x := v.(type) {
	case string:
		return sub(x)
	case map[string]any:
		for k, e := range x {
			x[k] = substituteValue(e, sub)
		}
		return x
	case []any:
		for i, e := range x {
			x[i] = substituteValue(e, sub)
		}
		return x
	}
	return v
}

// setDotted assigns value at v[path...], creating intermediate maps.
func setDotted(v map[string]any, path []string, value any) {
	for _, k := range path[:len(path)-1] {
		next, _ := v[k].(map[string]any)
		if next == nil {
			next = map[string]any{}
			v[k] = next
		}
		v = next
	}
	v[path[len(path)-1]] = value
}

// renderCollection builds the JSON models collection for shape opencode/pi.
func (m *ModelsTemplate) renderCollection(ctx renderContext) any {
	switch m.Shape {
	case "opencode":
		return opencodeModelsCollection(ctx.models)
	case "pi":
		return piModelsCollection(ctx.models)
	}
	return nil
}

func (t *Template) rewriteTOML(ctx renderContext) error {
	data, err := readFile(t.File)
	if err != nil {
		if os.IsNotExist(err) {
			data = nil
		} else {
			return err
		}
	}
	text := string(data)
	for key, val := range t.TOML.TopKeys {
		text = SetTOMLTopKey(text, key, ctx.substitute(val)) // val is written verbatim after substitution (template authors quote strings)
	}
	for _, s := range t.TOML.Sections {
		body := "\n[" + ctx.substitute(s.Name) + "]\n" + ctx.substitute(s.Body) + "\n"
		text = ReplaceOrAppendTOMLSection(text, ctx.substitute(s.Name), body)
	}
	if t.Models != nil && t.Models.Shape == "kimi" {
		for _, m := range ctx.models {
			mv := modelVars(m, ctx)
			if t.Models.AlsoRemove != "" {
				text = removeTOMLSection(text, substituteModel(t.Models.AlsoRemove, mv, ctx))
			}
			name := substituteModel(t.Models.TOMLSection, mv, ctx)
			body := "\n[" + name + "]\n" + substituteModel(t.Models.TOMLBody, mv, ctx) + "\n"
			text = ReplaceOrAppendTOMLSection(text, name, body)
		}
	}
	return atomicWriteFile(t.File, []byte(text), preserveMode(t.File, 0o600))
}

// modelVars resolves the per-model placeholders for the TOML loop. Context
// falls back to the conservative default (kimi-cli requires max_context_size).
func modelVars(m ExposedModel, ctx renderContext) map[string]string {
	contextN := m.PM.Context
	if contextN <= 0 {
		contextN = routing.DefaultModelMetadata.Context
	}
	output := m.PM.Output
	return map[string]string{
		"{{model.id}}":      m.Exposed,
		"{{model.context}}": fmt.Sprintf("%d", contextN),
		"{{model.output}}":  fmt.Sprintf("%d", output),
	}
}

func substituteModel(s string, vars map[string]string, ctx renderContext) string {
	out := ctx.substitute(s)
	for k, v := range vars {
		out = strings.ReplaceAll(out, k, v)
	}
	return out
}

// rewriteEnv patches a KEY=VALUE file, replacing managed keys in place and
// appending missing ones; unmanaged lines (incl. comments) are untouched.
func (t *Template) rewriteEnv(ctx renderContext) error {
	data, err := readFile(t.File)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	remaining := map[string]string{}
	for k, v := range t.Env.Set {
		remaining[k] = ctx.substitute(v)
	}
	for i, l := range lines {
		trimmed := strings.TrimSpace(l)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, _, ok := strings.Cut(trimmed, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if val, managed := remaining[key]; managed {
			lines[i] = key + "=" + val
			delete(remaining, key)
		}
	}
	// Append missing keys in sorted order for determinism.
	for _, k := range sortedKeys(remaining) {
		lines = append(lines, k+"="+remaining[k])
	}
	out := strings.Join(lines, "\n")
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return atomicWriteFile(t.File, []byte(out), preserveMode(t.File, 0o600))
}

// BaseURLEndpoint returns the rendered base URL (doctor's drift expectation).
func (t *Template) BaseURLEndpoint(cfg *configdomain.Config) string {
	return t.contextFor(cfg, nil, nil).baseURL
}

// ProviderIDValue returns the effective provider id (default "model-proxy").
func (t *Template) ProviderIDValue() string {
	if t.ProviderID != "" {
		return t.ProviderID
	}
	return "model-proxy"
}

// Pointer returns the client's proxy pointer — the current value found in the
// config file and the value this template would write — for doctor's takeover
// drift check. JSON/TOML/env text scans only (the repo has no TOML decoder).
func (t *Template) Pointer(cfg *configdomain.Config) (current, expected string) {
	ctx := t.contextFor(cfg, nil, nil)
	expected = ctx.baseURL
	data, err := readFile(t.File)
	if err != nil {
		return "(file missing)", expected
	}
	switch t.Format {
	case "json":
		if t.JSON.DriftPath == "" {
			return "(no drift probe)", expected
		}
		var v map[string]any
		if err := json.Unmarshal(data, &v); err != nil {
			return "(unreadable: " + err.Error() + ")", expected
		}
		s, ok := nestedString(v, strings.Split(ctx.substitute(t.JSON.DriftPath), ".")...)
		if !ok {
			return "(missing)", expected
		}
		return s, expected
	case "toml":
		text := string(data)
		if _, hasSelector := t.TOML.TopKeys["model_provider"]; hasSelector {
			// codex style: drift when the top-level selector no longer points at
			// our provider, or the provider section's base_url moved.
			selector, selectorFound := tomlTopKeyValue(text, "model_provider")
			if !selectorFound {
				return "model_provider (missing)", expected
			}
			if selector != ctx.providerID {
				return "model_provider = " + strconv.Quote(selector), expected
			}
		}
		section := ""
		if len(t.TOML.Sections) > 0 {
			section = ctx.substitute(t.TOML.Sections[0].Name)
		}
		baseURL := tomlSectionScalar(text, section, "base_url")
		if baseURL == "" {
			return "(missing)", expected
		}
		return baseURL, expected
	case "env":
		key := t.driftEnvKey(ctx)
		for _, l := range strings.Split(string(data), "\n") {
			k, v, ok := strings.Cut(strings.TrimSpace(l), "=")
			if ok && strings.TrimSpace(k) == key {
				return strings.TrimSpace(v), expected
			}
		}
		return "(missing)", expected
	}
	return "(unknown format)", expected
}

// driftEnvKey picks the managed env key whose rendered value is the base URL
// (the proxy pointer), else the alphabetically-first managed key.
func (t *Template) driftEnvKey(ctx renderContext) string {
	for _, k := range sortedKeys(t.Env.Set) {
		if ctx.substitute(t.Env.Set[k]) == ctx.baseURL {
			return k
		}
	}
	return sortedKeys(t.Env.Set)[0]
}

// nestedString walks v along path and returns the terminal string.
func nestedString(v map[string]any, path ...string) (string, bool) {
	cur := v
	for i, k := range path {
		if i == len(path)-1 {
			s, ok := cur[k].(string)
			return s, ok
		}
		next, ok := cur[k].(map[string]any)
		if !ok {
			return "", false
		}
		cur = next
	}
	return "", false
}

// tomlTopKeyValue reads a top-level TOML string key (before any [section]).
func tomlTopKeyValue(text, key string) (string, bool) {
	for _, line := range strings.Split(text, "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "[") {
			break
		}
		if !strings.HasPrefix(l, key) {
			continue
		}
		if i := strings.Index(l, "="); i >= 0 {
			return strings.Trim(strings.TrimSpace(l[i+1:]), `"`), true
		}
	}
	return "", false
}

// tomlSectionScalar reads a bare scalar key inside one [section] (header is
// the trimmed [name] line; "" never matches).
func tomlSectionScalar(text, section, key string) string {
	if section == "" {
		return ""
	}
	header := "[" + section + "]"
	inSection := false
	for _, line := range strings.Split(text, "\n") {
		l := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(l, "["):
			inSection = l == header
		case inSection && strings.HasPrefix(l, key):
			if i := strings.Index(l, "="); i >= 0 {
				return strings.Trim(strings.TrimSpace(l[i+1:]), `"`)
			}
		}
	}
	return ""
}
