package wirecap

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// disabled_file_test.go — per-package coverage for the disabled_models.json
// persistence form. The end-to-end toggle/restart pipeline is covered in
// internal/app (model_disable_test.go); these pin the file contract itself:
// path derivation, missing/malformed/version-mismatch loads, and the
// round-trip through the atomic save.

func TestDisabledModelsPathDerivation(t *testing.T) {
	if got := DisabledModelsPath("/h/.model-proxy/quota_state.json"); got != "/h/.model-proxy/disabled_models.json" {
		t.Fatalf("DisabledModelsPath = %q", got)
	}
	// The home form lands in the canonical state dir beside quota_state.json.
	if got := DisabledModelsPathForHome("/home/u"); got != filepath.FromSlash("/home/u/.model-proxy/disabled_models.json") {
		t.Fatalf("DisabledModelsPathForHome = %q", got)
	}
}

func TestLoadDisabledModelsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "disabled_models.json")

	// Missing file = first run, no override, not an error.
	if entries, err := LoadDisabledModelsFile(path); err != nil || entries != nil {
		t.Fatalf("missing file = %v, %v; want nil, nil", entries, err)
	}

	// Malformed file IS an error (caller warns, never trusts corrupt data).
	if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if entries, err := LoadDisabledModelsFile(path); err == nil || entries != nil {
		t.Fatalf("malformed file = %v, %v; want nil, error", entries, err)
	}

	// Version mismatch behaves like an absent file (semantics changed).
	if err := os.WriteFile(path, []byte(`{"version":999,"disabled":{"p":["m"]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if entries, err := LoadDisabledModelsFile(path); err != nil || entries != nil {
		t.Fatalf("version bump = %v, %v; want nil, nil", entries, err)
	}
}

func TestSaveDisabledModelsFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state", "disabled_models.json") // dir does not exist yet

	// Sorted within each provider for stable files; nested dir created.
	if err := SaveDisabledModelsFile(path, map[string][]string{
		"zhipu": {"glm-5.3", "glm-4.7"},
		"acme":  {"m2"},
	}); err != nil {
		t.Fatalf("SaveDisabledModelsFile: %v", err)
	}
	entries, err := LoadDisabledModelsFile(path)
	if err != nil {
		t.Fatalf("LoadDisabledModelsFile: %v", err)
	}
	if len(entries["zhipu"]) != 2 || entries["zhipu"][0] != "glm-4.7" || entries["zhipu"][1] != "glm-5.3" {
		t.Errorf("zhipu entries = %v, want sorted [glm-4.7 glm-5.3]", entries["zhipu"])
	}
	if len(entries["acme"]) != 1 || entries["acme"][0] != "m2" {
		t.Errorf("acme entries = %v", entries["acme"])
	}

	// The on-disk form is the versioned document.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Version  int                 `json:"version"`
		Disabled map[string][]string `json:"disabled"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("on-disk form: %v", err)
	}
	if doc.Version != DisabledModelsFileVersion {
		t.Errorf("version = %d, want %d", doc.Version, DisabledModelsFileVersion)
	}

	// No stray temp files from the atomic write.
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".disabled_models-*"))
	if len(matches) != 0 {
		t.Errorf("leftover temp files: %v", matches)
	}
}
