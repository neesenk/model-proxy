package takeover_test

import (
	"fmt"
	"model-proxy/internal/accounts"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/routing"
	"model-proxy/internal/takeover"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTemplateOverrides writes user templates for the given name→target-file
// map into a fresh templates dir, exercising the user-override mechanism the
// same way production resolves it. Bodies mirror the embedded presets.
func writeTemplateOverrides(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	bodies := map[string]func(file string) string{
		"claude": func(f string) string {
			return "file: " + f + `
format: json
json:
  set:
    env.ANTHROPIC_BASE_URL: "{{base_url}}"
    env.ANTHROPIC_AUTH_TOKEN: "{{token}}"
  drift_path: env.ANTHROPIC_BASE_URL
`
		},
		"opencode": func(f string) string {
			return "file: " + f + `
format: json
client: opencode
protocol: anthropic
base_url: v1
json:
  set:
    provider.{{provider_id}}:
      name: "model-proxy"
      npm: "@ai-sdk/anthropic"
      options: {apiKey: "{{token}}", baseURL: "{{base_url}}"}
  drift_path: provider.{{provider_id}}.options.baseURL
models:
  shape: opencode
  json_path: provider.{{provider_id}}.models
`
		},
		"pi": func(f string) string {
			return "file: " + f + `
format: json
client: pi
protocol: anthropic
base_url: bare
json:
  set:
    providers.{{provider_id}}:
      baseUrl: "{{base_url}}"
      api: "anthropic-messages"
      apiKey: "{{token}}"
  drift_path: providers.{{provider_id}}.baseUrl
models:
  shape: pi
  json_path: providers.{{provider_id}}.models
`
		},
		"codex": func(f string) string {
			return "file: " + f + `
format: toml
base_url: bare
toml:
  top_keys:
    model_provider: '"{{provider_id}}"'
  sections:
    - name: 'model_providers."{{provider_id}}"'
      body: |
        name = "model-proxy"
        base_url = "{{base_url}}"
        wire_api = "responses"
        requires_openai_auth = true
`
		},
	}
	for name, f := range files {
		body, ok := bodies[name]
		if !ok {
			t.Fatalf("no override body for %s", name)
		}
		if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(body(f)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// --- listClients ---

func TestListClients_All(t *testing.T) {
	cfg := &configdomain.Config{}
	all, err := takeover.ListClients(cfg, "", "")
	if err != nil {
		t.Fatal(err)
	}
	// Every embedded preset resolves; the set is sorted by name.
	if len(all) < 5 {
		t.Fatalf("ListClients('') len=%d want the preset set (>=5)", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i-1].Name >= all[i].Name {
			t.Errorf("ListClients not sorted: %q before %q", all[i-1].Name, all[i].Name)
		}
	}
	again, err := takeover.ListClients(cfg, "all", "")
	if err != nil || len(again) != len(all) {
		t.Errorf("ListClients('all') len=%d err=%v, want %d", len(again), err, len(all))
	}
}

func TestListClients_OneByName(t *testing.T) {
	one, err := takeover.ListClients(&configdomain.Config{}, "codex", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 1 || one[0].Name != "codex" {
		t.Errorf("ListClients('codex')=%+v want [codex]", one)
	}
}

func TestListClients_UnknownErrors(t *testing.T) {
	if _, err := takeover.ListClients(&configdomain.Config{}, "nope", ""); err == nil ||
		!strings.Contains(err.Error(), `unknown takeover client "nope"`) {
		t.Errorf("ListClients('nope'): want unknown-client error, got %v", err)
	}
}

// --- runTakeover / runRestore end-to-end on a single client ---

func TestRunTakeover_AndRestore_Claude(t *testing.T) {
	dir := t.TempDir()
	claudeFile := filepath.Join(dir, "claude.json")
	templatesDir := writeTemplateOverrides(t, map[string]string{"claude": claudeFile})
	cfg := &configdomain.Config{
		Listen: "127.0.0.1:15721",
		Providers: map[string]configdomain.Provider{
			"aqp": {OpenAIBaseURL: "http://x", Provider: "aqp", Models: []string{"glm-5.2"}},
		},
		Routes: map[string][]configdomain.RouteTarget{"glm-5.2": {{Provider: "aqp", Model: "glm-5.2"}}},
	}
	bakDir := filepath.Join(dir, ".mp")

	// Seed an existing claude config, then takeover rewrites it (after backing up).
	os.WriteFile(claudeFile, []byte(`{"env":{"OLD":"1"}}`), 0o644)
	if err := takeover.RunTakeover(cfg, "claude", bakDir, takeover.ModelFacts{SourceDefault: -1}, templatesDir, takeover.ModeUnified); err != nil {
		t.Fatal(err)
	}
	rewritten, _ := os.ReadFile(claudeFile)
	if !strings.Contains(string(rewritten), "ANTHROPIC_BASE_URL") {
		t.Errorf("takeover did not rewrite claude: %s", rewritten)
	}
	// Backup preserved the original.
	bak, _ := os.ReadFile(filepath.Join(bakDir, "claude.bak"))
	if !strings.Contains(string(bak), "OLD") {
		t.Errorf("backup did not preserve original: %s", bak)
	}
	// Restore brings the original back.
	if err := takeover.RunRestore(cfg, "claude", bakDir, templatesDir); err != nil {
		t.Fatal(err)
	}
	restored, _ := os.ReadFile(claudeFile)
	if !strings.Contains(string(restored), "OLD") || strings.Contains(string(restored), "ANTHROPIC_BASE_URL") {
		t.Errorf("restore did not revert: %s", restored)
	}
}

func TestRunTakeover_UnknownClientErrors(t *testing.T) {
	// "nope" is a hard resolution error — a typo'd client must not no-op silently.
	if err := takeover.RunTakeover(&configdomain.Config{}, "nope", t.TempDir(), takeover.ModelFacts{SourceDefault: -1}, "", takeover.ModeUnified); err == nil {
		t.Error("runTakeover unknown client: want error, got nil")
	}
}

// --- runTakeover all: skips a client whose config file is absent ---

func TestRunTakeover_AllSkipsMissingFiles(t *testing.T) {
	// Keep the models.dev catalog fetch offline: takeover.RunTakeover("all") refreshes
	// the catalog for metadata-writing templates — point it at a local stub.
	// The stub must be a VALID catalog (at least one provider): an empty one
	// fails parsing, and with HOME isolated below there is no cached catalog
	// to fall back to.
	md := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"zhipuai":{"models":{"glm-4.7":{"limit":{"context":128000,"output":8192}}}}}`)
	}))
	defer md.Close()
	t.Setenv("MP_MODELSDEV_URL", md.URL)
	// Isolate HOME: LoadModelsCatalog persists the fetched catalog to
	// <home>/.model-proxy/models_cache.json and the accounts store reads
	// <home>/.model-proxy — without this the test would overwrite the real
	// user cache (or silently depend on its freshness). The remaining presets
	// also resolve under this fake HOME (and are absent → skipped).
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	dir := t.TempDir()
	bakDir := filepath.Join(dir, ".mp")
	claudeFile := filepath.Join(dir, "claude.json")
	templatesDir := writeTemplateOverrides(t, map[string]string{"claude": claudeFile})
	cfg := &configdomain.Config{
		Listen: "127.0.0.1:15721",
		Providers: map[string]configdomain.Provider{
			"aqp": {OpenAIBaseURL: "http://x", Provider: "aqp", Models: []string{"glm-5.2"}},
		},
		Routes: map[string][]configdomain.RouteTarget{"glm-5.2": {{Provider: "aqp", Model: "glm-5.2"}}},
	}
	if err := os.WriteFile(claudeFile, []byte(`{"env":{"OLD":"1"}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	cat, err := configdomain.LoadModelsCatalog(accounts.HomeDir(), false)
	if err != nil {
		t.Fatalf("load models catalog: %v", err)
	}
	meta, sources := routing.HydrateModels(cfg, cat)
	factsSources := make(map[string]map[string]int, len(sources))
	for provider, models := range sources {
		factsSources[provider] = make(map[string]int, len(models))
		for model, source := range models {
			factsSources[provider][model] = int(source)
		}
	}
	facts := takeover.ModelFacts{
		Routes:         routing.RouteTable(cfg),
		Meta:           meta,
		Sources:        factsSources,
		SourceDefault:  int(routing.SrcDefault),
		DefaultContext: routing.DefaultModelMetadata.Context,
		DefaultOutput:  routing.DefaultModelMetadata.Output,
	}
	if err := takeover.RunTakeover(cfg, "all", bakDir, facts, templatesDir, takeover.ModeUnified); err != nil {
		t.Fatalf("runTakeover all with missing files: want nil, got %v", err)
	}
	// claude was rewritten (backup + rewrite succeeded).
	b, _ := os.ReadFile(claudeFile)
	if !strings.Contains(string(b), "ANTHROPIC_BASE_URL") {
		t.Errorf("claude not rewritten: %s", b)
	}
	// Missing clients' files were NOT created (skipped, not rewritten to defaults).
	if entries, _ := os.ReadDir(fakeHome); len(entries) > 0 {
		for _, e := range entries {
			if e.Name() == ".model-proxy" {
				continue // catalog cache, expected
			}
			t.Errorf("unexpected file created under fake HOME: %s", e.Name())
		}
	}
}

// --- runTakeover single named client: missing file is a hard error (not skipped) ---

func TestRunTakeover_SingleMissingFileErrors(t *testing.T) {
	// Isolate HOME: family resolution may pick a NON-overridden preset
	// variant, whose file path must never resolve to the real user config.
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	templatesDir := writeTemplateOverrides(t, map[string]string{"pi": filepath.Join(dir, "nonexistent.json")})
	// Anthropic-native provider → family pi resolves to the overridden
	// anthropic variant (named "pi"), whose file is missing.
	cfg := &configdomain.Config{
		Listen: "127.0.0.1:15721",
		Providers: map[string]configdomain.Provider{
			"claude-up": {AnthropicBaseURL: "http://x/anthropic/v1", Provider: "claude-up", Models: []string{"claude-x"}},
		},
		Routes: map[string][]configdomain.RouteTarget{"claude-x": {{Provider: "claude-up", Model: "claude-x"}}},
	}
	if err := takeover.RunTakeover(cfg, "pi", dir, takeover.ModelFacts{SourceDefault: -1}, templatesDir, takeover.ModeUnified); err == nil {
		t.Error("runTakeover pi with missing file: want error, got nil (single client must not be skipped)")
	}
}

// --- runRestore all: skips a client with no backup (symmetric with takeover) ---

func TestRunRestore_AllSkipsMissingBackup(t *testing.T) {
	// Isolate HOME: the non-overridden presets resolve there — restore must
	// never touch the real user configs.
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	bakDir := filepath.Join(dir, ".mp")
	os.MkdirAll(bakDir, 0o700)
	// Only a claude backup exists.
	os.WriteFile(filepath.Join(bakDir, "claude.bak"), []byte(`{"env":{"OLD":"1"}}`), 0o600)
	claudeFile := filepath.Join(dir, "claude.json")
	templatesDir := writeTemplateOverrides(t, map[string]string{"claude": claudeFile})
	cfg := &configdomain.Config{
		Listen: "127.0.0.1:15721",
		Providers: map[string]configdomain.Provider{
			"aqp": {OpenAIBaseURL: "http://x", Provider: "aqp", Models: []string{"glm-5.2"}},
		},
		Routes: map[string][]configdomain.RouteTarget{"glm-5.2": {{Provider: "aqp", Model: "glm-5.2"}}},
	}
	if err := takeover.RunRestore(cfg, "all", bakDir, templatesDir); err != nil {
		t.Fatalf("runRestore all with missing backups: want nil, got %v", err)
	}
	b, _ := os.ReadFile(claudeFile)
	if !strings.Contains(string(b), "OLD") {
		t.Errorf("claude not restored from backup: %s", b)
	}
}

// --- create:true templates (stepcode's optional models.json) ---

// TestRunTakeover_CreateMissingConfig: a named create:true client whose
// config file does not exist gets it created (no hard error): an empty .bak
// with created:true meta stands in for the content backup, and restore
// deletes the file again — the pre-takeover state was "no file".
func TestRunTakeover_CreateMissingConfig(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	// Step Code is installed (config dir exists) but the user never
	// customized models — models.json does not exist.
	if err := os.MkdirAll(filepath.Join(fakeHome, ".stepcode"), 0o700); err != nil {
		t.Fatal(err)
	}
	modelsJSON := filepath.Join(fakeHome, ".stepcode", "models.json")
	cfg := &configdomain.Config{
		Listen: "127.0.0.1:15721",
		Providers: map[string]configdomain.Provider{
			"zhipu": {OpenAIBaseURL: "http://z/v1", Provider: "zhipu", Models: []string{"glm-5.3"}},
		},
		Routes: map[string][]configdomain.RouteTarget{"glm-5.3": {{Provider: "zhipu", Model: "glm-5.3"}}},
	}
	bakDir := filepath.Join(fakeHome, ".mp")
	facts := takeover.ModelFacts{
		SourceDefault: -1,
		Routes:        map[string][]configdomain.RouteTarget{"glm-5.3": {{Provider: "zhipu", Model: "glm-5.3"}}},
	}

	if err := takeover.RunTakeover(cfg, "stepcode", bakDir, facts, "", takeover.ModeUnified); err != nil {
		t.Fatalf("named create:true takeover with missing config must succeed: %v", err)
	}
	rewritten, err := os.ReadFile(modelsJSON)
	if err != nil {
		t.Fatalf("takeover did not create models.json: %v", err)
	}
	if !strings.Contains(string(rewritten), `"model-proxy-openai"`) || !strings.Contains(string(rewritten), `"glm-5.3"`) {
		t.Errorf("created models.json missing provider/models: %s", rewritten)
	}
	// The openai variant was auto-selected (openai-native provider) — its
	// backup is an empty file with a created:true meta.
	bak := filepath.Join(bakDir, "stepcode-openai.bak")
	bakData, err := os.ReadFile(bak)
	if err != nil {
		t.Fatalf("created marker backup missing: %v", err)
	}
	if len(bakData) != 0 {
		t.Errorf("created marker backup must be empty, got %d bytes", len(bakData))
	}
	meta, err := os.ReadFile(bak + ".meta")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(meta), `"created": true`) {
		t.Errorf("meta must mark created:true: %s", meta)
	}

	// Re-takeover keeps the created marker (the original pre-takeover state —
	// "no file" — stays restorable).
	if err := takeover.RunTakeover(cfg, "stepcode", bakDir, facts, "", takeover.ModeUnified); err != nil {
		t.Fatal(err)
	}
	meta2, _ := os.ReadFile(bak + ".meta")
	if !strings.Contains(string(meta2), `"created": true`) {
		t.Errorf("re-takeover overwrote the created marker: %s", meta2)
	}

	// Restore deletes the created file and the markers.
	if err := takeover.RunRestore(cfg, "stepcode", bakDir, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(modelsJSON); !os.IsNotExist(err) {
		t.Errorf("restore of a created file must delete it: %v", err)
	}
	if _, err := os.Stat(bak); !os.IsNotExist(err) {
		t.Errorf("restore must remove the backup marker: %v", err)
	}
}

// TestRunTakeover_AllCreatesOnlyWhenInstalled: batch mode (`all`) creates a
// missing create:true config only when the agent's config directory exists —
// it must not materialize configs for agents the user never ran.
func TestRunTakeover_AllCreatesOnlyWhenInstalled(t *testing.T) {
	md := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"zhipuai":{"models":{"glm-5.3":{"limit":{"context":128000,"output":8192}}}}}`)
	}))
	defer md.Close()
	t.Setenv("MP_MODELSDEV_URL", md.URL)
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	cfg := &configdomain.Config{
		Listen: "127.0.0.1:15721",
		Providers: map[string]configdomain.Provider{
			"zhipu": {OpenAIBaseURL: "http://z/v1", Provider: "zhipu", Models: []string{"glm-5.3"}},
		},
		Routes: map[string][]configdomain.RouteTarget{"glm-5.3": {{Provider: "zhipu", Model: "glm-5.3"}}},
	}
	bakDir := filepath.Join(fakeHome, ".mp")
	modelsJSON := filepath.Join(fakeHome, ".stepcode", "models.json")

	// No ~/.stepcode at all: stepcode is skipped like every uninstalled agent.
	report, err := takeover.RunTakeoverReport(cfg, "all", bakDir, takeover.ModelFacts{SourceDefault: -1}, "", takeover.ModeUnified, takeover.ScopeAll)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(modelsJSON); !os.IsNotExist(err) {
		t.Fatalf("batch mode created a config for an uninstalled agent: %v", err)
	}
	foundSkip := false
	for _, s := range report.Skipped {
		if s == "stepcode-openai" || s == "stepcode" {
			foundSkip = true
		}
	}
	if !foundSkip {
		t.Errorf("uninstalled stepcode must be reported skipped, got applied=%v skipped=%v", report.Applied, report.Skipped)
	}

	// ~/.stepcode exists (Step Code ran once): batch takeover creates
	// models.json.
	if err := os.MkdirAll(filepath.Join(fakeHome, ".stepcode"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := takeover.RunTakeoverReport(cfg, "all", bakDir, takeover.ModelFacts{SourceDefault: -1}, "", takeover.ModeUnified, takeover.ScopeAll); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(modelsJSON); err != nil {
		t.Fatalf("batch mode did not create models.json for an installed agent: %v", err)
	}
}

// TestClientInstalled: the single authoritative installed-detection shared by
// the config-init wizard, `takeover all` batch creation and the Web surface —
// config file present, or create:true + config directory present.
func TestClientInstalled(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// workbuddy (create:true): absent dir → not installed; dir present → installed.
	wb, err := takeover.TemplateByName("workbuddy", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if takeover.ClientInstalled(wb) {
		t.Error("workbuddy installed without ~/.workbuddy")
	}
	if err := os.MkdirAll(filepath.Join(home, ".workbuddy"), 0o700); err != nil {
		t.Fatal(err)
	}
	if !takeover.ClientInstalled(wb) {
		t.Error("workbuddy not detected with ~/.workbuddy present")
	}

	// claude (no create): only the config file counts.
	cl, err := takeover.TemplateByName("claude", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if takeover.ClientInstalled(cl) {
		t.Error("claude installed without settings.json")
	}
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if takeover.ClientInstalled(cl) {
		t.Error("claude must not count a bare ~/.claude dir (not create:true)")
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if !takeover.ClientInstalled(cl) {
		t.Error("claude not detected with settings.json present")
	}
}
