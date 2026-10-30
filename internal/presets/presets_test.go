package presets

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/providerstore"
)

// mergeFixture returns a config path plus a hermetic provider-store path: the
// store lives under a temp HOME so preset merges never touch ~/.model-proxy.
func mergeFixture(t *testing.T) (cfgPath, storePath string) {
	t.Helper()
	return writeMinimalConfig(t), providerstore.PathUnder(t.TempDir())
}

// effectiveLoad validates cfgPath against a store's provider set — the same
// effective merge every loader applies post-migration.
func effectiveLoad(t *testing.T, cfgPath, storePath string) *configdomain.Config {
	t.Helper()
	providers, err := providerstore.Load(storePath)
	if err != nil {
		t.Fatalf("load store: %v", err)
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := configdomain.LoadConfigEffective(cfgPath, data, providers)
	if err != nil {
		t.Fatalf("effective load: %v", err)
	}
	return cfg
}

func TestList_DerivesFromTemplateAndFiltersUnregistered(t *testing.T) {
	catalog, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(catalog) == 0 {
		t.Fatal("catalog must not be empty — template drift or registration regression")
	}
	names := map[string]bool{}
	for _, p := range catalog {
		if excludedPresets[p.Name] {
			t.Errorf("preset %q is in the exclusion list but surfaced", p.Name)
		}
		if len(p.Models) == 0 {
			t.Errorf("preset %q has no default models", p.Name)
		}
		names[p.Name] = true
	}
	for _, want := range []string{"zhipu", "deepseek", "kimi-code", "mimo", "qwen-plan", "step-plan", "volcengine", "typesafe", "openrouter", "opencode-go"} {
		if !names[want] {
			t.Errorf("expected preset %q in catalog; got %v", want, names)
		}
	}
	if names["aqp"] {
		t.Error("internal-network aqp must never surface in the public catalog")
	}
	for i := 1; i < len(catalog); i++ {
		if catalog[i-1].Name >= catalog[i].Name {
			t.Fatalf("catalog not sorted by name: %q >= %q", catalog[i-1].Name, catalog[i].Name)
		}
	}
}

// TestList_EveryPresetBlockValidates proves each catalog entry produces a
// loadable config when merged into a minimal one — a template block that
// validates standalone but breaks after merge would be caught here.
func TestList_EveryPresetBlockValidates(t *testing.T) {
	catalog, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, p := range catalog {
		t.Run(p.Name, func(t *testing.T) {
			cfgPath, storePath := mergeFixture(t)
			if _, err := MergeBlock(cfgPath, storePath, p.Name); err != nil {
				t.Fatalf("MergeBlock: %v", err)
			}
			if cfg := effectiveLoad(t, cfgPath, storePath); cfg.Providers[p.Name].Provider == "" {
				t.Fatalf("merged config for %s lost the preset entry", p.Name)
			}
		})
	}
}

const minimalConfig = `listen: 127.0.0.1:15721
log_level: info
`

func writeMinimalConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(minimalConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// --- MergeBlock ---

func TestMergePresetBlock_WritesStoreEntryAndStaysValid(t *testing.T) {
	cfgPath, storePath := mergeFixture(t)
	before, _ := os.ReadFile(cfgPath)
	written, err := MergeBlock(cfgPath, storePath, "deepseek")
	if err != nil || !written {
		t.Fatalf("first merge: written=%v err=%v", written, err)
	}
	store, err := providerstore.Load(storePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store["deepseek"]; !ok {
		t.Fatalf("store after merge = %#v, want the deepseek preset entry", store)
	}
	if after, _ := os.ReadFile(cfgPath); string(after) != string(before) {
		t.Fatalf("merge must not rewrite the yaml (providers live in the store):\n%s", after)
	}
	if cfg := effectiveLoad(t, cfgPath, storePath); len(cfg.Providers) != 1 {
		t.Fatalf("effective providers = %#v, want exactly deepseek", cfg.Providers)
	}
}

func TestMergePresetBlock_IdempotentSecondRunIsNoop(t *testing.T) {
	cfgPath, storePath := mergeFixture(t)
	if _, err := MergeBlock(cfgPath, storePath, "zhipu"); err != nil {
		t.Fatalf("first merge: %v", err)
	}
	before, err := providerstore.Load(storePath)
	if err != nil {
		t.Fatal(err)
	}
	written, err := MergeBlock(cfgPath, storePath, "zhipu")
	if err != nil {
		t.Fatalf("second merge: %v", err)
	}
	if written {
		t.Fatal("second merge of an existing preset must be a no-op")
	}
	after, err := providerstore.Load(storePath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("no-op merge rewrote the store")
	}
}

func TestMergePresetBlock_UnknownPresetFailsClosed(t *testing.T) {
	cfgPath, storePath := mergeFixture(t)
	before, _ := os.ReadFile(cfgPath)
	if _, err := MergeBlock(cfgPath, storePath, "nonexistent"); err == nil {
		t.Fatal("unknown preset must fail with an error")
	}
	after, _ := os.ReadFile(cfgPath)
	if string(before) != string(after) {
		t.Fatal("failed merge must not touch the config file")
	}
	if _, err := os.Stat(storePath); !os.IsNotExist(err) {
		t.Fatalf("failed merge must not create the store (stat err = %v)", err)
	}
}

// A config that never carried a providers: section (the post-migration
// normal form) merges straight into the store.
func TestMergePresetBlock_MinimalConfigWithoutProviders(t *testing.T) {
	cfgPath, storePath := mergeFixture(t)
	if _, err := MergeBlock(cfgPath, storePath, "zhipu"); err != nil {
		t.Fatalf("merge without providers: key: %v", err)
	}
	if cfg := effectiveLoad(t, cfgPath, storePath); cfg.Providers["zhipu"].Provider != "zhipu" {
		t.Fatalf("effective providers = %#v", cfg.Providers)
	}
}

// --- PreviewMergeBlock ---

// TestPreviewMergeBlock_DryRunMatchesWrite pins the single-source contract:
// the preview computes exactly what MergeBlock later writes, and the preview
// itself never touches the file (the CLI add ambiguity gate relies on the
// latter: refusing the gate must leave config.yaml byte-identical).
func TestPreviewMergeBlock_DryRunMatchesWrite(t *testing.T) {
	cfgPath, storePath := mergeFixture(t)
	before, _ := os.ReadFile(cfgPath)

	changed, preview, err := PreviewMergeBlock(cfgPath, storePath, "deepseek")
	if err != nil || !changed {
		t.Fatalf("preview: changed=%v err=%v", changed, err)
	}
	if _, ok := preview.Providers["deepseek"]; !ok {
		t.Fatalf("preview config lost the preset entry: %#v", preview.Providers)
	}
	after, _ := os.ReadFile(cfgPath)
	if string(before) != string(after) {
		t.Fatal("preview must not touch the config file")
	}
	if _, err := os.Stat(storePath); !os.IsNotExist(err) {
		t.Fatal("preview must not create the store")
	}

	written, err := MergeBlock(cfgPath, storePath, "deepseek")
	if err != nil || !written {
		t.Fatalf("merge after preview: written=%v err=%v", written, err)
	}
	merged := effectiveLoad(t, cfgPath, storePath)
	if _, ok := merged.Providers["deepseek"]; !ok {
		t.Fatal("MergeBlock must persist the previewed preset entry")
	}

	// The entry now exists: preview reports a no-op, like MergeBlock.
	changed, again, err := PreviewMergeBlock(cfgPath, storePath, "deepseek")
	if err != nil || changed || again == nil {
		t.Fatalf("preview of existing entry: changed=%v cfg=%v err=%v", changed, again, err)
	}
}

// --- AmbiguousModels ---

func ambiguousFixture(t *testing.T, extra string) *configdomain.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := minimalConfig + "\n" + extra
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := configdomain.LoadConfig(path)
	if err != nil {
		t.Fatalf("fixture config invalid: %v\n---\n%s", err, content)
	}
	return cfg
}

func TestAmbiguousModels_DetectsOverlapWithoutExplicitRoute(t *testing.T) {
	cfg := ambiguousFixture(t, `
providers:
  zhipu:
    provider_id: zhipu
    openai_base_url: https://z.example.com/v1
    usage_url: https://z.example.com/v1/models
    models: [glm-a]
  other:
    provider_id: deepseek
    openai_base_url: https://d.example.com/v1
    usage_url: https://d.example.com/v1/models
    models: [glm-a, d1]
`)
	got := AmbiguousModels(cfg, "zhipu")
	if len(got) != 1 || got[0] != "glm-a" {
		t.Fatalf("AmbiguousModels = %v, want [glm-a]", got)
	}
}

func TestAmbiguousModels_SilentWithExplicitRoute(t *testing.T) {
	cfg := ambiguousFixture(t, `
providers:
  zhipu:
    provider_id: zhipu
    openai_base_url: https://z.example.com/v1
    usage_url: https://z.example.com/v1/models
    models: [glm-a]
  other:
    provider_id: deepseek
    openai_base_url: https://d.example.com/v1
    usage_url: https://d.example.com/v1/models
    models: [glm-a]
routes:
  glm-a:
    - {provider: zhipu, model: glm-a, priority: 1}
    - {provider: other, model: glm-a, priority: 2}
`)
	if got := AmbiguousModels(cfg, "zhipu"); len(got) != 0 {
		t.Fatalf("explicitly routed model must not be ambiguous; got %v", got)
	}
}

func TestAmbiguousModels_NoOverlapNoWarning(t *testing.T) {
	cfg := ambiguousFixture(t, `
providers:
  zhipu:
    provider_id: zhipu
    openai_base_url: https://z.example.com/v1
    usage_url: https://z.example.com/v1/models
    models: [glm-a]
  other:
    provider_id: deepseek
    openai_base_url: https://d.example.com/v1
    usage_url: https://d.example.com/v1/models
    models: [d1]
`)
	if got := AmbiguousModels(cfg, "zhipu"); len(got) != 0 {
		t.Fatalf("disjoint model lists must not be ambiguous; got %v", got)
	}
}

// TestListCarriesPrefillFields pins the Add Provider prefill contract: the
// catalog entry carries the template's editable fields (anthropic base,
// priority) so the form's prefilled inputs match what the backend template
// would persist.
func TestListCarriesPrefillFields(t *testing.T) {
	catalog, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	byName := map[string]Preset{}
	for _, p := range catalog {
		byName[p.Name] = p
	}
	zhipu, ok := byName["zhipu"]
	if !ok {
		t.Fatal("zhipu preset missing")
	}
	if zhipu.AnthropicBaseURL != "https://open.bigmodel.cn/api/anthropic" {
		t.Errorf("zhipu anthropic_base_url = %q", zhipu.AnthropicBaseURL)
	}
	if zhipu.Priority != 2 {
		t.Errorf("zhipu priority = %d, want the template's 2", zhipu.Priority)
	}
}

func TestLookup(t *testing.T) {
	if _, ok := Lookup("zhipu"); !ok {
		t.Fatal("Lookup(zhipu) must resolve")
	}
	if _, ok := Lookup("no-such"); ok {
		t.Fatal("unknown preset must not resolve")
	}
}

func TestNames(t *testing.T) {
	catalog := []Preset{{Name: "b"}, {Name: "a"}}
	if got := Names(catalog); got != "b|a" {
		t.Fatalf("Names = %q", got)
	}
}

func TestMergePresetBlock_RejectsNonMappingConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("- just\n- a\n- list\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := MergeBlock(path, providerstore.PathUnder(t.TempDir()), "zhipu"); err == nil {
		t.Fatal("non-mapping config must fail")
	}
}

func TestMergePresetBlock_MissingConfigFileFails(t *testing.T) {
	if _, err := MergeBlock(filepath.Join(t.TempDir(), "absent.yaml"), providerstore.PathUnder(t.TempDir()), "zhipu"); err == nil {
		t.Fatal("missing config file must fail")
	}
}

func TestAmbiguousModels_MissingProviderIsNil(t *testing.T) {
	// Construct the config directly: `providers: {}` is rejected by config
	// validate ("no providers configured"), so loading it from YAML would make
	// this test skip unconditionally. AmbiguousModels is a pure function and
	// must stay nil-safe for a preset name that is simply absent.
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"zhipu": {OpenAIBaseURL: "http://x", Provider: "zhipu", Models: []string{"glm"}},
		},
	}
	if got := AmbiguousModels(cfg, "absent"); got != nil {
		t.Fatalf("AmbiguousModels for missing provider = %v, want nil", got)
	}
}
