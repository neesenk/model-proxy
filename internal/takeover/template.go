package takeover

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	// Variants declares the protocol variants of ONE multi-protocol agent in
	// a single template document (opencode/pi: anthropic + openai +
	// responses). ParseTemplate expands them into independent templates that
	// share the top-level file/format/client (and mcp block) — downstream
	// (family selection, split partitioning, backup units, CLI/Web) keeps
	// working on the expanded per-variant templates exactly as before. When
	// variants are present the top level may only carry shared fields;
	// per-variant write blocks live under each variant.
	Variants []VariantSpec `yaml:"variants"`
	// MCP renders the gateway's MCP surface (config mcp: + mcp_routes:) as
	// client MCP entries pointing at http://<listen>/mcp/<name>. Nil = the
	// template writes no MCP config.
	MCP *MCPTemplate `yaml:"mcp"`

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
// section per model instead of a JSON collection; codex renders a standalone
// model-catalog JSON file (model_catalog_json) the client loads on startup.
type ModelsTemplate struct {
	Shape string `yaml:"shape"` // opencode | pi | kimi | codex
	// CatalogFile is the codex shape's model-catalog JSON path (written
	// wholesale on every run; model_catalog_json in the client config points
	// here). Restore deletes it together with the takeover.
	CatalogFile string `yaml:"catalog_file"`
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

// MCPTemplate describes how the gateway's MCP servers/routes are written
// into a client config. json renders an object at json_path with one
// json_entry per entry (placeholders {{mcp.name}} / {{mcp.url}} / the global
// set); toml renders one section per entry. env format is unsupported.
//
// File optionally names a SEPARATE file the client stores MCP config in
// (claude: ~/.claude.json next to the model takeover's ~/.claude/settings.json).
// Empty renders into the template's main File. A separate File is backed up
// and restored as its own unit (<template>-mcp marker), keeping one takeover
// template = one client.
type MCPTemplate struct {
	File string `yaml:"file"`
	// JSONPath is where the rendered MCP object is stored (json format).
	JSONPath string `yaml:"json_path"`
	// JSONEntry is the per-entry value template (a YAML map/list/scalar;
	// placeholders substituted in every string leaf).
	JSONEntry any `yaml:"json_entry"`
	// IncludeRoutedMembers restores the pre-prune behavior of writing EVERY
	// configured server: by default a server aggregated into an mcp_routes
	// target is NOT rendered separately — the route is the canonical entry,
	// and duplicating its members would hand the client overlapping tools
	// (web-search the route plus zhipu-search/exa/firecrawl direct). Explicit
	// MCP subsetting (opts.MCP / Web chips) still names any server, routed or
	// not — selection at the execution point outranks the default prune.
	IncludeRoutedMembers bool `yaml:"include_routed_members"`

	// TOMLSection is the per-entry section name pattern; TOMLBody its body.
	TOMLSection string `yaml:"toml_section"`
	TOMLBody    string `yaml:"toml_body"`
}

// VariantSpec is one protocol variant inside a variants: template document:
// the per-variant slice of a client's config (provider entry + model
// collection) plus the wire protocol that family selection keys on. Fields
// omitted here fall back to the document's top-level values.
type VariantSpec struct {
	// Name is the variant's template id (the former one-variant-per-file
	// name, e.g. pi-openai) — takeover <name>, backup markers and the Web
	// surface all key on it.
	Name        string          `yaml:"name"`
	Protocol    string          `yaml:"protocol"`
	BaseURL     string          `yaml:"base_url"`
	ProviderID  string          `yaml:"provider_id"`
	ProxyURL    string          `yaml:"proxy_url"`
	DisplayName string          `yaml:"display_name"`
	JSON        *JSONTemplate   `yaml:"json"`
	TOML        *TOMLTemplate   `yaml:"toml"`
	Env         *EnvTemplate    `yaml:"env"`
	Models      *ModelsTemplate `yaml:"models"`
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
// A document with a variants: block expands into one template per variant
// (shared top-level fields + per-variant overrides); a plain document
// yields a single-element slice.
func ParseTemplate(name, source string, data []byte) ([]*Template, error) {
	var t Template
	if err := yaml.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("template %s: parse yaml: %w", name, err)
	}
	t.Name = name
	t.Source = source
	if len(t.Variants) == 0 {
		if err := validateTemplate(&t); err != nil {
			return nil, err
		}
		return []*Template{&t}, nil
	}
	// variants mode: the top level carries only shared fields — per-variant
	// write blocks and identity live under variants:. (Checked before the
	// entry count so a mixed document reports the real problem.)
	if t.JSON != nil || t.TOML != nil || t.Env != nil || t.Models != nil ||
		t.Protocol != "" || t.ProviderID != "" || t.BaseURL != "" || t.ProxyURL != "" || t.DisplayName != "" {
		return nil, fmt.Errorf("template %s: variants templates keep protocol identity and write blocks (json/toml/env/models/protocol/base_url/provider_id) under each variant; the top level carries description/file/format/client, and may share the mcp block (its rendering does not depend on the variant)", name)
	}
	if len(t.Variants) == 1 {
		return nil, fmt.Errorf("template %s: variants requires two or more entries (use a plain template otherwise)", name)
	}
	if t.File == "" {
		return nil, fmt.Errorf("template %s: file is required (client config path)", name)
	}
	switch t.Format {
	case "json", "toml", "env":
	default:
		return nil, fmt.Errorf("template %s: format must be json|toml|env, got %q", name, t.Format)
	}
	t.File = expandHome(t.File)
	seen := map[string]bool{}
	out := make([]*Template, 0, len(t.Variants))
	for i := range t.Variants {
		v := t.Variants[i]
		if v.Name == "" {
			return nil, fmt.Errorf("template %s: variants[%d] is missing name", name, i)
		}
		if seen[v.Name] {
			return nil, fmt.Errorf("template %s: variant %q declared twice", name, v.Name)
		}
		seen[v.Name] = true
		vt := Template{
			Name:        v.Name,
			Description: t.Description,
			File:        t.File,
			Format:      t.Format,
			BaseURL:     v.BaseURL,
			ProviderID:  v.ProviderID,
			ProxyURL:    v.ProxyURL,
			DisplayName: v.DisplayName,
			Client:      t.Client,
			Protocol:    v.Protocol,
			JSON:        v.JSON,
			TOML:        v.TOML,
			Env:         v.Env,
			Models:      v.Models,
			MCP:         t.MCP,
			Source:      source,
		}
		if err := validateTemplate(&vt); err != nil {
			return nil, err
		}
		out = append(out, &vt)
	}
	return out, nil
}

// validateTemplate enforces the per-template contract: file present, a known
// format with its required write block, sane base_url/protocol values, and
// the models/mcp shape constraints. Shared by plain documents and each
// expanded variant.
func validateTemplate(t *Template) error {
	if t.File == "" {
		return fmt.Errorf("template %s: file is required (client config path)", t.Name)
	}
	t.File = expandHome(t.File)
	switch t.Format {
	case "json":
		// json.set may be empty (or the whole json: block absent) when the
		// template writes ONLY mcp entries.
		hasSet := t.JSON != nil && len(t.JSON.Set) > 0
		hasMCP := t.MCP != nil && t.MCP.JSONPath != ""
		if !hasSet && !hasMCP {
			return fmt.Errorf("template %s: format json requires non-empty json.set (or an mcp: block)", t.Name)
		}
	case "toml":
		if t.TOML == nil || (len(t.TOML.TopKeys) == 0 && len(t.TOML.Sections) == 0 && t.MCP == nil) {
			return fmt.Errorf("template %s: format toml requires toml.top_keys and/or toml.sections (or an mcp: block)", t.Name)
		}
	case "env":
		if t.Env == nil || len(t.Env.Set) == 0 {
			return fmt.Errorf("template %s: format env requires non-empty env.set", t.Name)
		}
	default:
		return fmt.Errorf("template %s: format must be json|toml|env, got %q", t.Name, t.Format)
	}
	if t.BaseURL != "" && t.BaseURL != "bare" && t.BaseURL != "v1" {
		return fmt.Errorf("template %s: base_url must be bare|v1, got %q", t.Name, t.BaseURL)
	}
	switch t.Protocol {
	case "", "anthropic", "openai", "responses":
	default:
		return fmt.Errorf("template %s: protocol must be anthropic|openai|responses, got %q", t.Name, t.Protocol)
	}
	if t.Models != nil {
		switch t.Models.Shape {
		case "opencode", "pi":
			if t.Format != "json" {
				return fmt.Errorf("template %s: models shape %s requires format json", t.Name, t.Models.Shape)
			}
			if t.Models.JSONPath == "" {
				return fmt.Errorf("template %s: models.json_path is required for shape %s", t.Name, t.Models.Shape)
			}
		case "kimi":
			if t.Format != "toml" || t.Models.TOMLSection == "" || t.Models.TOMLBody == "" {
				return fmt.Errorf("template %s: models shape kimi requires format toml + toml_section + toml_body", t.Name)
			}
		case "codex":
			if t.Format != "toml" || t.Models.CatalogFile == "" {
				return fmt.Errorf("template %s: models shape codex requires format toml + catalog_file", t.Name)
			}
		default:
			return fmt.Errorf("template %s: models.shape must be opencode|pi|kimi|codex, got %q", t.Name, t.Models.Shape)
		}
	}
	if t.MCP != nil {
		if t.MCP.File != "" {
			// A separate MCP storage file is JSON (every client that splits
			// MCP out — claude's ~/.claude.json, kimi's ~/.kimi-code/mcp.json
			// — uses the mcpServers JSON shape), regardless of the main
			// config's format.
			if t.MCP.JSONPath == "" || t.MCP.JSONEntry == nil {
				return fmt.Errorf("template %s: mcp.file requires json_path + json_entry (the separate file is JSON)", t.Name)
			}
		} else {
			switch t.Format {
			case "json":
				if t.MCP.JSONPath == "" || t.MCP.JSONEntry == nil {
					return fmt.Errorf("template %s: mcp requires json_path + json_entry for format json", t.Name)
				}
			case "toml":
				if t.MCP.TOMLSection == "" || t.MCP.TOMLBody == "" {
					return fmt.Errorf("template %s: mcp requires toml_section + toml_body for format toml", t.Name)
				}
			default:
				return fmt.Errorf("template %s: mcp is unsupported for format env", t.Name)
			}
		}
	}
	return nil
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
			parsed, err := ParseTemplate(name, userDir, data)
			if err != nil {
				return nil, err
			}
			for _, t := range parsed {
				templates[t.Name] = t
			}
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
	primary     string
	fast        string
	models      []ExposedModel
	mcp         []mcpEntry // rendered into the client config
	// mcpAll is every server+route entry BEFORE the routed-member prune;
	// explicit MCP subsetting selects from it so a named server always
	// renders even when the default surface pruned it.
	mcpAll []mcpEntry
	meta   map[string]map[string]catalog.Model
	routes map[string][]configdomain.RouteTarget
}

// mcpEntry is one gateway MCP surface entry (server or route) rendered into
// client MCP config.
type mcpEntry struct {
	Name string
	URL  string
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
	// MCP surface: every route plus every server NOT aggregated into a route
	// becomes one client entry pointing at the gateway (routed members are
	// pruned — the route is the canonical entry; include_routed_members opts
	// back into the full list). Sorted for deterministic output.
	includeRouted := t.MCP != nil && t.MCP.IncludeRoutedMembers
	mcpEntries := mcpSurfaceEntries(cfg, proxyURL, includeRouted)
	mcpAll := mcpSurfaceEntries(cfg, proxyURL, true)
	modelList := ExposedModels(cfg, meta, routes)
	primary, fast := primaryAndFastModel(modelList)
	return renderContext{
		proxyURL:    proxyURL,
		baseURL:     baseURL,
		providerID:  pid,
		displayName: display,
		models:      modelList,
		primary:     primary,
		fast:        fast,
		mcp:         mcpEntries,
		mcpAll:      mcpAll,
		meta:        meta,
		routes:      routes,
	}
}

// mcpSurfaceEntries projects the gateway's MCP surface (config mcp: +
// mcp_routes:) as client-config entries. Disabled servers/routes are skipped
// (their /mcp/ endpoints 404 — writing them would hand the client dead
// entries). With includeRouted false, servers referenced by any ENABLED
// route target are skipped too: the route already exposes their aggregated
// tools, and duplicating both would give the client overlapping entries for
// the same capability. A disabled route owns nothing — its members fall back
// to direct exposure so the capability stays reachable.
func mcpSurfaceEntries(cfg *configdomain.Config, proxyURL string, includeRouted bool) []mcpEntry {
	routed := map[string]bool{}
	for _, route := range cfg.MCPRoutes {
		if !route.MCPRouteEffectiveEnabled() {
			continue
		}
		for _, target := range route.Targets {
			if target.Server != "" {
				routed[target.Server] = true
			}
		}
	}
	entries := make([]mcpEntry, 0, len(cfg.MCP)+len(cfg.MCPRoutes))
	for name, srv := range cfg.MCP {
		if !srv.MCPEffectiveEnabled() {
			continue
		}
		if routed[name] && !includeRouted {
			continue
		}
		entries = append(entries, mcpEntry{Name: name, URL: proxyURL + "/mcp/" + name})
	}
	for name, route := range cfg.MCPRoutes {
		if !route.MCPRouteEffectiveEnabled() {
			continue
		}
		entries = append(entries, mcpEntry{Name: name, URL: proxyURL + "/mcp/" + name})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries
}

// MCPSurfaceNames lists the MCP entries a takeover renders by default:
// every route plus every unrouted server (the routed-member prune). The
// admin surface and Web selection chips both key on it so the offered list
// equals what a default run writes.
func MCPSurfaceNames(cfg *configdomain.Config) []string {
	entries := mcpSurfaceEntries(cfg, "", false)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name)
	}
	return names
}

// primaryAndFastModel picks the two single-model defaults clients use for
// "the strong slot" and "the background slot" (claude's
// ANTHROPIC_DEFAULT_OPUS/SONNET vs _HAIKU): the largest-context exposed
// model is the flagship, the smallest-context one the lightweight tier.
// Ties break by exposed name for determinism.
func primaryAndFastModel(models []ExposedModel) (primary, fast string) {
	if len(models) == 0 {
		return "", ""
	}
	p, f := 0, 0
	for i := 1; i < len(models); i++ {
		if models[i].PM.Context > models[p].PM.Context ||
			(models[i].PM.Context == models[p].PM.Context && models[i].Exposed < models[p].Exposed) {
			p = i
		}
		if models[i].PM.Context < models[f].PM.Context ||
			(models[i].PM.Context == models[f].PM.Context && models[i].Exposed < models[f].Exposed) {
			f = i
		}
	}
	return models[p].Exposed, models[f].Exposed
}

func (c renderContext) substitute(s string) string {
	return strings.NewReplacer(
		"{{proxy_url}}", c.proxyURL,
		"{{base_url}}", c.baseURL,
		"{{token}}", "PROXY_MANAGED",
		"{{provider_id}}", c.providerID,
		"{{display_name}}", c.displayName,
		"{{model.primary}}", c.primary,
		"{{model.fast}}", c.fast,
	).Replace(s)
}

// substituteMCP resolves the per-entry placeholders first, then the global set.
func (c renderContext) substituteMCP(s string, e mcpEntry) string {
	return c.substitute(strings.NewReplacer(
		"{{mcp.name}}", e.Name,
		"{{mcp.url}}", e.URL,
	).Replace(s))
}

// mcpAuxFile returns the MCP block's separate storage file (a takeover/
// restore unit distinct from the main config), or "" when the MCP block
// renders into the main file or does not exist.
func mcpAuxFile(t *Template) string {
	if t == nil || t.MCP == nil || t.MCP.File == "" {
		return ""
	}
	return expandHome(t.MCP.File)
}

// mcpFile returns the file the MCP block renders into: the block's own File
// when set (claude keeps MCP in ~/.claude.json, separate from settings.json),
// else the template's main File. Empty string when there is no MCP block.
func (t *Template) mcpFile() string {
	if t.MCP == nil {
		return ""
	}
	if t.MCP.File != "" {
		return expandHome(t.MCP.File)
	}
	return t.File
}

// Rewrite renders the template into the client config file (creating it when
// absent), preserving unrelated content. ClientSpec.Rewrite adapter.
func (t *Template) Rewrite(cfg *configdomain.Config, meta map[string]map[string]catalog.Model, routes map[string][]configdomain.RouteTarget) error {
	return t.RewriteFiltered(cfg, meta, routes, nil)
}

// RewriteScoped renders with a scope: model keeps the provider/models part
// and leaves the MCP surface untouched; mcp writes only the MCP surface
// (claude: the aux file alone); all (the default) renders everything.
func (t *Template) RewriteScoped(cfg *configdomain.Config, meta map[string]map[string]catalog.Model, routes map[string][]configdomain.RouteTarget, scope RewriteScope) error {
	return t.RewriteOptsFiltered(cfg, meta, routes, nil, TakeoverOptions{Scope: scope})
}

// RewriteOpts renders with full options: the scope plus optional MCP/model
// subset selections (nil = everything).
func (t *Template) RewriteOpts(cfg *configdomain.Config, meta map[string]map[string]catalog.Model, routes map[string][]configdomain.RouteTarget, opts TakeoverOptions) error {
	return t.RewriteOptsFiltered(cfg, meta, routes, nil, opts)
}

// RewriteFiltered is Rewrite with an optional exposed-model filter: when only
// is non-nil, the rendered models collection contains just those exposed
// names. Split mode uses it to give each protocol variant of a family its own
// native-protocol model subset; nil renders every exposed model (unified).
func (t *Template) RewriteFiltered(cfg *configdomain.Config, meta map[string]map[string]catalog.Model, routes map[string][]configdomain.RouteTarget, only map[string]bool) error {
	return t.RewriteOptsFiltered(cfg, meta, routes, only, TakeoverOptions{})
}

// RewriteOptsFiltered is the one rendering entry: `only` is the split-mode
// model assignment, opts.Models the user's subset — they intersect.
func (t *Template) RewriteOptsFiltered(cfg *configdomain.Config, meta map[string]map[string]catalog.Model, routes map[string][]configdomain.RouteTarget, only map[string]bool, opts TakeoverOptions) error {
	ctx := t.contextFor(cfg, meta, routes)
	if opts.MCP != nil {
		want := map[string]bool{}
		for _, n := range opts.MCP {
			want[n] = true
		}
		// Subsetting selects from the FULL entry list: naming a routed member
		// is an explicit direct-connection request and outranks the prune.
		filtered := make([]mcpEntry, 0, len(ctx.mcpAll))
		for _, e := range ctx.mcpAll {
			if want[e.Name] {
				filtered = append(filtered, e)
			}
		}
		ctx.mcp = filtered
	}
	if only != nil || opts.Models != nil {
		var want map[string]bool
		if opts.Models != nil {
			want = map[string]bool{}
			for _, m := range opts.Models {
				want[m] = true
			}
		}
		filtered := make([]ExposedModel, 0, len(ctx.models))
		for _, m := range ctx.models {
			if (only == nil || only[m.Exposed]) && (want == nil || want[m.Exposed]) {
				filtered = append(filtered, m)
			}
		}
		ctx.models = filtered
	}
	switch t.Format {
	case "json":
		return t.rewriteJSON(ctx, opts.Scope)
	case "toml":
		return t.rewriteTOML(ctx, opts.Scope)
	case "env":
		return t.rewriteEnv(ctx, opts.Scope)
	}
	return fmt.Errorf("template %s: unknown format %q", t.Name, t.Format)
}

func (t *Template) rewriteJSON(ctx renderContext, scope RewriteScope) error {
	// MCP-only scope against a separate aux file never touches the main
	// config at all.
	mcpDst := t.mcpFile()
	if scope == ScopeMCP && mcpDst != "" && mcpDst != t.File {
		mv, err := ReadJSONConfig(mcpDst)
		if err != nil {
			return err
		}
		mergeMCPIntoJSON(mv, t.MCP, ctx)
		return WriteJSONConfig(mcpDst, mv)
	}
	v, err := ReadJSONConfig(t.File)
	if err != nil {
		return err
	}
	if scope != ScopeMCP {
		if t.JSON != nil {
			for path, value := range t.JSON.Set {
				setDotted(v, strings.Split(ctx.substitute(path), "."), substituteValue(value, ctx.substitute))
			}
		}
		if t.Models != nil {
			setDotted(v, strings.Split(ctx.substitute(t.Models.JSONPath), "."), t.Models.renderCollection(ctx))
		}
	}
	if mcpDst == "" || mcpDst == t.File {
		// Single-file template (or no MCP block at all): the MCP block merges
		// into the same document (opencode/codex style) and everything is
		// written once.
		if scope != ScopeModel {
			mergeMCPIntoJSON(v, t.MCP, ctx)
		}
		return WriteJSONConfig(t.File, v)
	}
	// Separate MCP storage (claude keeps MCP in ~/.claude.json next to the
	// model takeover's settings.json): write the main config first, then
	// merge the gateway surface into the aux file's own document. A
	// model-scope run leaves the aux file COMPLETELY untouched — reading it
	// absent and writing the empty object back would clobber the client's
	// MCP config.
	if err := WriteJSONConfig(t.File, v); err != nil {
		return err
	}
	if scope == ScopeModel {
		return nil
	}
	mv, err := ReadJSONConfig(mcpDst)
	if err != nil {
		return err
	}
	mergeMCPIntoJSON(mv, t.MCP, ctx)
	return WriteJSONConfig(mcpDst, mv)
}

// mergeMCPIntoJSON folds the gateway MCP surface into one JSON document.
// Empty gateway surface: leave the client's existing MCP config alone.
// Otherwise MERGE: entries pointing elsewhere (the user's own servers) are
// preserved; this proxy's surface is replaced wholesale — current entries
// plus stale leftovers from earlier takeovers, both identified by the /mcp/
// URL under this proxy URL.
func mergeMCPIntoJSON(v map[string]any, mcp *MCPTemplate, ctx renderContext) {
	if mcp == nil || len(ctx.mcp) == 0 {
		return
	}
	path := strings.Split(ctx.substitute(mcp.JSONPath), ".")
	existing := dottedMap(v, path)
	proxyPrefix := ctx.proxyURL + "/mcp/"
	for k, e := range existing {
		if m, ok := e.(map[string]any); ok {
			if u, ok := m["url"].(string); ok && strings.HasPrefix(u, proxyPrefix) {
				delete(existing, k)
			}
		}
	}
	for _, e := range ctx.mcp {
		existing[e.Name] = substituteValue(mcp.JSONEntry, func(s string) string { return ctx.substituteMCP(s, e) })
	}
}

// dottedMap navigates (creating) the map at v[path...] and returns it.
// A non-map value in the way is replaced (the mcp writer owns that subtree).
func dottedMap(v map[string]any, path []string) map[string]any {
	for _, k := range path {
		next, _ := v[k].(map[string]any)
		if next == nil {
			next = map[string]any{}
			v[k] = next
		}
		v = next
	}
	return v
}

// substituteValue walks decoded YAML values, substituting placeholders in
// every string leaf. Pure: containers are rebuilt, never mutated in place —
// t.JSON.Set values belong to the shared *Template, and a caller reusing the
// template (TemplateByName encourages it) must not see the first render's
// substitutions baked into the second.
func substituteValue(v any, sub func(string) string) any {
	switch x := v.(type) {
	case string:
		return sub(x)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = substituteValue(e, sub)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = substituteValue(e, sub)
		}
		return out
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

func (t *Template) rewriteTOML(ctx renderContext, scope RewriteScope) error {
	data, err := readFile(t.File)
	if err != nil {
		if os.IsNotExist(err) {
			data = nil
		} else {
			return err
		}
	}
	text := string(data)
	if scope != ScopeMCP {
		for key, val := range t.TOML.TopKeys {
			text = SetTOMLTopKey(text, key, ctx.substitute(val)) // val is written verbatim after substitution (template authors quote strings)
		}
		for _, s := range t.TOML.Sections {
			body := "\n[" + ctx.substitute(s.Name) + "]\n" + ctx.substitute(s.Body) + "\n"
			text = ReplaceOrAppendTOMLSection(text, ctx.substitute(s.Name), body)
		}
	}
	if t.Models != nil && t.Models.Shape == "kimi" && scope != ScopeMCP {
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
	if t.Models != nil && t.Models.Shape == "codex" && scope != ScopeMCP && len(ctx.models) > 0 {
		catalog, err := json.MarshalIndent(map[string]any{"models": codexModelCatalog(ctx.models)}, "", "  ")
		if err != nil {
			return err
		}
		catalogPath := expandHome(t.Models.CatalogFile)
		// A fresh install may not have the client's config directory yet
		// (codex never ran) — the atomic write needs it to exist.
		if err := os.MkdirAll(filepath.Dir(catalogPath), 0o755); err != nil {
			return fmt.Errorf("template %s: model catalog dir: %w", t.Name, err)
		}
		if err := atomicWriteFile(catalogPath, append(catalog, '\n'), preserveMode(catalogPath, 0o600)); err != nil {
			return fmt.Errorf("template %s: model catalog: %w", t.Name, err)
		}
		// The catalog replaces codex's bundled list for this process — the
		// /model picker then shows exactly the proxy's exposed models.
		text = SetTOMLTopKey(text, "model_catalog_json", strconv.Quote(catalogPath))
	}
	// A separate MCP storage file is JSON regardless of the main format
	// (kimi: config.toml + ~/.kimi-code/mcp.json) — merge the gateway
	// surface there and leave the TOML mcp rendering out entirely.
	if mcpDst := t.mcpFile(); t.MCP != nil && mcpDst != "" && mcpDst != t.File {
		if scope != ScopeModel {
			mv, err := ReadJSONConfig(mcpDst)
			if err != nil {
				return err
			}
			mergeMCPIntoJSON(mv, t.MCP, ctx)
			if err := WriteJSONConfig(mcpDst, mv); err != nil {
				return err
			}
		}
		return atomicWriteFile(t.File, []byte(text), preserveMode(t.File, 0o600))
	}
	if t.MCP != nil && scope != ScopeModel {
		// Drop stale proxy mcp sections first (previous takeover leftovers):
		// any [<prefix>...] section whose body carries this proxy's /mcp/ URL,
		// where <prefix> is the template's literal section-name prefix.
		prefix := strings.Split(t.MCP.TOMLSection, "{{")[0]
		if prefix != "" {
			text = removeTOMLSectionsWithURL(text, prefix, ctx.proxyURL+"/mcp/")
		}
		for _, e := range ctx.mcp {
			name := ctx.substituteMCP(t.MCP.TOMLSection, e)
			body := "\n[" + name + "]\n" + ctx.substituteMCP(t.MCP.TOMLBody, e) + "\n"
			text = ReplaceOrAppendTOMLSection(text, name, body)
		}
	}
	return atomicWriteFile(t.File, []byte(text), preserveMode(t.File, 0o600))
}

// modelVars resolves the per-model placeholders for the TOML loop. Context
// falls back to the conservative default (kimi-cli requires max_context_size).
// {{model.capabilities}} and {{model.efforts}} project models.dev metadata
// onto kimi-code's per-model capability block (kimi is the only consumer of
// the TOML models loop today); see kimiCapabilities/kimiEffortBlock.
func modelVars(m ExposedModel, ctx renderContext) map[string]string {
	contextN := m.PM.Context
	if contextN <= 0 {
		contextN = routing.DefaultModelMetadata.Context
	}
	output := m.PM.Output
	return map[string]string{
		"{{model.id}}":           m.Exposed,
		"{{model.context}}":      fmt.Sprintf("%d", contextN),
		"{{model.output}}":       fmt.Sprintf("%d", output),
		"{{model.capabilities}}": tomlStringArray(kimiCapabilities(m.PM)),
		"{{model.efforts}}":      kimiEffortBlock(m.PM),
	}
}

// tomlStringArray renders a TOML array-of-strings literal ("[]" for empty).
func tomlStringArray(items []string) string {
	quoted := make([]string, 0, len(items))
	for _, it := range items {
		quoted = append(quoted, strconv.Quote(it))
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// kimiCapabilities derives kimi-code's per-model capabilities array from
// models.dev metadata. The mapping is verified against kimi-code's own
// managed config entries:
//
//	thinking, always_thinking  <- reasoning
//	image_in, video_in         <- input modalities (image / video)
//	tool_use                   <- tool_call
//	dynamically_loaded_tools   <- tool_call AND an effort dial — every
//	                             effort-capable kimi model ships it, the
//	                             effort-less ones (e.g. highspeed) do not
//
// Models without models.dev metadata render an empty array — equivalent to
// today's capability-less sections, never a fabricated capability.
func kimiCapabilities(pm catalog.Model) []string {
	var caps []string
	if pm.Reasoning {
		caps = append(caps, "thinking", "always_thinking")
	}
	for _, in := range pm.Modalities.Input {
		switch in {
		case "image":
			caps = append(caps, "image_in")
		case "video":
			caps = append(caps, "video_in")
		}
	}
	if pm.ToolCall {
		caps = append(caps, "tool_use")
		if len(pm.ReasoningEfforts) > 0 {
			caps = append(caps, "dynamically_loaded_tools")
		}
	}
	return caps
}

// kimiEffortBlock renders the support_efforts/default_effort lines from the
// model's effort dial, or "" when it has none (writing empty values would
// give kimi-code a broken selector). default_effort takes the HIGHEST
// advertised level — models.dev carries no per-model default marker (kimi's
// own managed configs disagree: kimi-for-coding defaults to max, k3 to high),
// so takeover surfaces the full dial and the user picks.
func kimiEffortBlock(pm catalog.Model) string {
	if len(pm.ReasoningEfforts) == 0 {
		return ""
	}
	return "support_efforts = " + tomlStringArray(pm.ReasoningEfforts) +
		"\ndefault_effort = " + strconv.Quote(pm.ReasoningEfforts[len(pm.ReasoningEfforts)-1])
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
func (t *Template) rewriteEnv(ctx renderContext, scope RewriteScope) error {
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
		// json may be nil on mcp-only templates (no drift probe either way).
		if t.JSON == nil || t.JSON.DriftPath == "" {
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
		if t.TOML == nil {
			return "(no drift probe)", expected
		}
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
// The key match is exact ("model" must not hit "model_provider") and any line
// after the first "[" belongs to a table, not the top level.
func tomlTopKeyValue(text, key string) (string, bool) {
	prefix := key + " "
	prefixTab := key + "\t"
	for _, line := range strings.Split(text, "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "[") {
			break
		}
		if !strings.HasPrefix(l, prefix) && !strings.HasPrefix(l, prefixTab) {
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
