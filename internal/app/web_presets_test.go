package app

import (
	"os"
	"path/filepath"
	"testing"

	"model-proxy/internal/appapi"
)

// web_presets_test.go pins the S3 web preset endpoints against the REAL
// proxyWebAPI adapter (not a stub): list derives the shared catalog, and
// AddPreset merges + validates + surfaces ambiguity through the same
// configedit pipeline the CLI uses.

func TestWebPresetsListAndAdd(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("listen: 127.0.0.1:0\nproviders:\n  deepseek: {provider_id: deepseek, openai_base_url: https://d}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfigFromBytes(cfgPath, mustReadFile(t, cfgPath))
	if err != nil {
		t.Fatalf("base config invalid: %v", err)
	}
	p := newTestProxy(t, cfg)
	w := NewWebServer(p, cfgPath)

	catalog := w.api.Presets()
	if len(catalog) == 0 {
		t.Fatal("Presets() returned an empty catalog")
	}
	found := false
	for _, pr := range catalog {
		if pr.Name == "zhipu" {
			found = true
		}
	}
	if !found {
		t.Fatal("catalog missing zhipu preset")
	}

	// AddPreset merges the block into the real config file.
	if _, err := w.api.AddPreset("zhipu"); err != nil {
		t.Fatalf("AddPreset(zhipu): %v", err)
	}
	merged, err := os.ReadFile(cfgPath)
	if err != nil || !contains(string(merged), "zhipu:") || !contains(string(merged), "deepseek:") {
		t.Fatalf("config file after AddPreset missing provider block:\n%s", merged)
	}

	// Idempotent: second AddPreset succeeds without duplicating.
	if _, err := w.api.AddPreset("zhipu"); err != nil {
		t.Fatalf("idempotent AddPreset: %v", err)
	}

	// Unknown preset fails closed.
	if _, err := w.api.AddPreset("no-such-preset"); err == nil {
		t.Fatal("unknown preset must error")
	}
}

// TestWebAddPresetSurfacesAmbiguity pins the warning contract: adding a
// preset whose models overlap another configured provider without explicit
// routes returns those models for the UI to surface.
func TestWebAddPresetSurfacesAmbiguity(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	base := "listen: 127.0.0.1:0\nproviders:\n" +
		"  zhipu:\n    provider_id: zhipu\n    openai_base_url: https://x\n    models: [shared-model]\n"
	if err := os.WriteFile(cfgPath, []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfigFromBytes(cfgPath, mustReadFile(t, cfgPath))
	if err != nil {
		t.Fatalf("base config invalid: %v", err)
	}
	p := newTestProxy(t, cfg)
	w := NewWebServer(p, cfgPath)

	// Seed a second provider that also serves shared-model, then add the
	// zhipu preset (template models are disjoint from shared-model, so seed
	// the overlap via deepseek whose template block we merge first).
	if _, err := w.api.AddPreset("deepseek"); err != nil {
		t.Fatalf("AddPreset(deepseek): %v", err)
	}
	// Direct the overlap: patch deepseek's models to include shared-model via
	// the structured edit port, then re-derive ambiguity by adding zhipu
	// (already present → merge no-op, ambiguity still computed).
	if err := w.api.EditConfig(editReqForProviderModels("deepseek", "shared-model")); err != nil {
		t.Fatalf("edit deepseek models: %v", err)
	}
	warnings, err := w.api.AddPreset("zhipu")
	if err != nil {
		t.Fatalf("AddPreset(zhipu): %v", err)
	}
	// zhipu (seeded) serves shared-model; deepseek now also serves it with no
	// explicit route → exactly that model must surface as a warning.
	if len(warnings) != 1 || warnings[0] != "shared-model" {
		t.Fatalf("warnings = %v, want [shared-model]", warnings)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

func editReqForProviderModels(provider, model string) appapi.EditRequest {
	return appapi.EditRequest{
		Kind: "provider",
		Name: provider,
		Data: map[string]any{"models": []any{model}},
	}
}
