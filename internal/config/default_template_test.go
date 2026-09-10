package config

import (
	"os"
	"reflect"
	"sort"
	"testing"
)

// TestDefaultTemplateMatchesRepoConfig guards the DefaultConfigYAML ↔
// config.yaml sync for every provider present in BOTH: their models lists
// (and provider_id) must be identical. The template intentionally diverges
// structurally — it activates qwen-plan (a public addable preset, commented
// out in the maintainer's config.yaml) and omits the private shopee block —
// so this is a per-provider content check, not byte equality. Without it the
// embedded preset catalog silently goes stale (wrong model counts in
// `presets list` / the Web Add-provider wizard) whenever config.yaml's model
// lists change without regenerating the template.
func TestDefaultTemplateMatchesRepoConfig(t *testing.T) {
	const repoConfig = "../../config.yaml"
	cfgBytes, err := os.ReadFile(repoConfig)
	if err != nil {
		// The template test only makes sense in a full checkout (repo root
		// present); a bare release tree (module download without the root
		// config.yaml) has nothing to compare against.
		t.Skipf("repo config.yaml not available: %v", err)
	}
	cfg, err := LoadConfigFromBytes("config.yaml", cfgBytes)
	if err != nil {
		t.Fatalf("repo config.yaml no longer parses: %v", err)
	}
	tpl, err := LoadConfigFromBytes("config.yaml", []byte(DefaultConfigYAML))
	if err != nil {
		t.Fatalf("DefaultConfigYAML no longer parses: %v", err)
	}
	if len(cfg.Providers) == 0 || len(tpl.Providers) == 0 {
		t.Fatalf("empty providers: config=%d template=%d", len(cfg.Providers), len(tpl.Providers))
	}
	checked := 0
	for name, want := range cfg.Providers {
		got, ok := tpl.Providers[name]
		if !ok {
			continue // provider absent from template (e.g. private shopee) — structural choice
		}
		checked++
		if got.Provider != want.Provider {
			t.Errorf("template provider %q provider_id = %q, want %q", name, got.Provider, want.Provider)
		}
		if len(got.Models) == 0 {
			t.Errorf("template provider %q has an empty models list", name)
		}
		wantModels := append([]string(nil), want.Models...)
		gotModels := append([]string(nil), got.Models...)
		sort.Strings(wantModels)
		sort.Strings(gotModels)
		if !reflect.DeepEqual(gotModels, wantModels) {
			t.Errorf("template provider %q models = %v, want config.yaml's %v", name, gotModels, wantModels)
		}
	}
	if checked == 0 {
		t.Fatal("no provider appears in both config.yaml and the template; sync check is vacuous")
	}
}
