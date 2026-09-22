package presets

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	configdomain "model-proxy/internal/config"
)

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
	for _, want := range []string{"zhipu", "deepseek", "kimi-code", "qwen-plan", "volcengine", "typesafe"} {
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
			cfgPath := writeMinimalConfig(t)
			if _, err := MergeBlock(cfgPath, p.Name); err != nil {
				t.Fatalf("MergeBlock: %v", err)
			}
			if _, err := configdomain.LoadConfig(cfgPath); err != nil {
				t.Fatalf("merged config for %s does not validate: %v", p.Name, err)
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

func TestMergePresetBlock_WritesProviderAndStaysValid(t *testing.T) {
	path := writeMinimalConfig(t)
	written, err := MergeBlock(path, "deepseek")
	if err != nil || !written {
		t.Fatalf("first merge: written=%v err=%v", written, err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "deepseek:") || !strings.Contains(string(data), "providers:") {
		t.Fatalf("merged config missing deepseek provider block:\n%s", data)
	}
	if _, err := configdomain.LoadConfig(path); err != nil {
		t.Fatalf("merged config invalid: %v", err)
	}
}

func TestMergePresetBlock_IdempotentSecondRunIsNoop(t *testing.T) {
	path := writeMinimalConfig(t)
	if _, err := MergeBlock(path, "zhipu"); err != nil {
		t.Fatalf("first merge: %v", err)
	}
	before, _ := os.ReadFile(path)
	written, err := MergeBlock(path, "zhipu")
	if err != nil {
		t.Fatalf("second merge: %v", err)
	}
	if written {
		t.Fatal("second merge of an existing preset must be a no-op")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("no-op merge rewrote the config file")
	}
}

func TestMergePresetBlock_UnknownPresetFailsClosed(t *testing.T) {
	path := writeMinimalConfig(t)
	before, _ := os.ReadFile(path)
	if _, err := MergeBlock(path, "nonexistent"); err == nil {
		t.Fatal("unknown preset must fail with an error")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("failed merge must not touch the config file")
	}
}

func TestMergePresetBlock_CreatesProvidersMapWhenMissing(t *testing.T) {
	path := writeMinimalConfig(t)
	if _, err := MergeBlock(path, "zhipu"); err != nil {
		t.Fatalf("merge without providers: key: %v", err)
	}
	if _, err := configdomain.LoadConfig(path); err != nil {
		t.Fatalf("merged config invalid: %v", err)
	}
}

// --- PreviewMergeBlock ---

// TestPreviewMergeBlock_DryRunMatchesWrite pins the single-source contract:
// the preview computes exactly what MergeBlock later writes, and the preview
// itself never touches the file (the CLI add ambiguity gate relies on the
// latter: refusing the gate must leave config.yaml byte-identical).
func TestPreviewMergeBlock_DryRunMatchesWrite(t *testing.T) {
	path := writeMinimalConfig(t)
	before, _ := os.ReadFile(path)

	changed, preview, err := PreviewMergeBlock(path, "deepseek")
	if err != nil || !changed {
		t.Fatalf("preview: changed=%v err=%v", changed, err)
	}
	if _, err := configdomain.LoadConfigFromBytes(path, preview); err != nil {
		t.Fatalf("preview content invalid: %v", err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("preview must not touch the config file")
	}

	written, err := MergeBlock(path, "deepseek")
	if err != nil || !written {
		t.Fatalf("merge after preview: written=%v err=%v", written, err)
	}
	onDisk, _ := os.ReadFile(path)
	if !bytes.Equal(preview, onDisk) {
		t.Fatal("MergeBlock must write exactly the previewed content")
	}

	// The block now exists: preview is a no-op, like MergeBlock.
	changed, merged, err := PreviewMergeBlock(path, "deepseek")
	if err != nil || changed || merged != nil {
		t.Fatalf("preview of existing block: changed=%v merged=%v err=%v", changed, merged, err)
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
	if _, err := MergeBlock(path, "zhipu"); err == nil {
		t.Fatal("non-mapping config must fail")
	}
}

func TestMergePresetBlock_MissingConfigFileFails(t *testing.T) {
	if _, err := MergeBlock(filepath.Join(t.TempDir(), "absent.yaml"), "zhipu"); err == nil {
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
