package models

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"model-proxy/internal/configedit"
)

// TestWriteProviderModelsRewritesModelsSequence: the locked load→mutate→write
// replaces only providers.<name>.models, preserving comments and every other
// section, and leaves a timestamped .bak of the previous file.
func TestWriteProviderModelsRewritesModelsSequence(t *testing.T) {
	configFile := filepath.Join(t.TempDir(), "config.yaml")
	original := `# top comment
providers:
  aqp:
    base_url: https://example.com # keep this comment
    models: [old-a, old-b]
  other:
    models: [untouched]
routes:
  glm: [{provider: aqp, model: old-a}]
`
	if err := os.WriteFile(configFile, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := WriteProviderModels(configFile, "aqp", []string{"new-b", "new-a"}); err != nil {
		t.Fatalf("WriteProviderModels: %v", err)
	}

	data, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "# keep this comment") || !strings.Contains(string(data), "# top comment") {
		t.Errorf("comments lost in rewrite:\n%s", data)
	}
	var decoded struct {
		Providers map[string]struct {
			BaseURL string   `yaml:"base_url"`
			Models  []string `yaml:"models"`
		} `yaml:"providers"`
		Routes map[string]any `yaml:"routes"`
	}
	if err := yaml.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("rewritten config does not parse: %v", err)
	}
	if got := decoded.Providers["aqp"].Models; len(got) != 2 || got[0] != "new-b" || got[1] != "new-a" {
		t.Errorf("aqp models = %v, want [new-b new-a] in caller order", got)
	}
	if got := decoded.Providers["aqp"].BaseURL; got != "https://example.com" {
		t.Errorf("aqp base_url = %q, want preserved", got)
	}
	if got := decoded.Providers["other"].Models; len(got) != 1 || got[0] != "untouched" {
		t.Errorf("other provider models = %v, want untouched", got)
	}
	if len(decoded.Routes) != 1 {
		t.Errorf("routes = %v, want preserved", decoded.Routes)
	}

	backup := configedit.BackupPath(configFile)
	if _, err := os.Stat(backup); err != nil {
		t.Errorf("backup of previous config missing at %s: %v", backup, err)
	}
}

// TestWriteProviderModelsUnknownProvider: rewriting a provider absent from the
// config fails without touching the file.
func TestWriteProviderModelsUnknownProvider(t *testing.T) {
	configFile := filepath.Join(t.TempDir(), "config.yaml")
	original := "providers:\n  aqp:\n    models: [m1]\n"
	if err := os.WriteFile(configFile, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	err := WriteProviderModels(configFile, "ghost", []string{"m2"})
	if err == nil || !strings.Contains(err.Error(), `"ghost"`) {
		t.Fatalf("error = %v, want provider-not-found naming the provider", err)
	}
	data, _ := os.ReadFile(configFile)
	if string(data) != original {
		t.Errorf("config modified on failure:\n%s", data)
	}
}

// TestWriteProviderModelsMissingConfig: a missing config file surfaces the
// read error instead of creating one from scratch.
func TestWriteProviderModelsMissingConfig(t *testing.T) {
	configFile := filepath.Join(t.TempDir(), "config.yaml")
	err := WriteProviderModels(configFile, "aqp", []string{"m1"})
	if err == nil {
		t.Fatal("expected an error for a missing config file")
	}
	if _, statErr := os.Stat(configFile); !os.IsNotExist(statErr) {
		t.Errorf("config file created from scratch: stat err = %v", statErr)
	}
}
