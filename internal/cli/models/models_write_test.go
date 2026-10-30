package models

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/providerstore"
)

// TestWriteProviderModelsRewritesStoreEntry: the locked load→mutate→write
// replaces only the provider's models in the provider.json store (absorbing
// and stripping the yaml's providers: section in the same mutation),
// preserving every other yaml section and comment.
func TestWriteProviderModelsRewritesModelsSequence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configFile := filepath.Join(t.TempDir(), "config.yaml")
	original := `# top comment
listen: 127.0.0.1:15721
providers:
  aqp:
    provider_id: aqp # keep this comment
    openai_base_url: https://example.com
    models: [old-a, old-b]
  other:
    provider_id: static
    openai_base_url: https://other.example.com
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

	store, err := providerstore.Load(providerstore.Path())
	if err != nil {
		t.Fatal(err)
	}
	if got := store["aqp"].Models; !reflect.DeepEqual(got, []string{"new-b", "new-a"}) {
		t.Errorf("aqp models = %v, want [new-b new-a] in caller order", got)
	}
	if got := store["aqp"].OpenAIBaseURL; got != "https://example.com" {
		t.Errorf("aqp base url = %q, want preserved", got)
	}
	if got := store["other"].Models; !reflect.DeepEqual(got, []string{"untouched"}) {
		t.Errorf("other provider models = %v, want untouched", got)
	}

	// The yaml keeps every non-provider section (comments included) and loses
	// the absorbed providers: block.
	data, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "# top comment") || !strings.Contains(string(data), "routes:") {
		t.Errorf("comments/routes lost in rewrite:\n%s", data)
	}
	if strings.Contains(string(data), "providers:") {
		t.Errorf("providers section must be stripped into the store:\n%s", data)
	}

	// The strip that absorbed the yaml section left a timestamped backup of
	// the pre-absorption yaml (the section's comments are lossy otherwise).
	baks, err := filepath.Glob(filepath.Join(filepath.Dir(configFile), ".model-proxy", "back", "config.yaml.*.bak"))
	if err != nil || len(baks) != 1 {
		t.Fatalf("want exactly 1 yaml backup of the absorption, got %v (err=%v)", baks, err)
	}
	bak, err := os.ReadFile(baks[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(bak), "openai_base_url: https://example.com") {
		t.Errorf("backup does not hold the pre-absorption providers section:\n%s", bak)
	}
}

// TestWriteProviderModelsUnknownProvider: rewriting a provider absent from the
// effective set fails without touching the yaml or creating a store.
func TestWriteProviderModelsUnknownProvider(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	configFile := filepath.Join(t.TempDir(), "config.yaml")
	original := "listen: 127.0.0.1:15721\nproviders:\n  aqp: {provider_id: aqp, openai_base_url: https://example.com, models: [m1]}\n"
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
	if _, statErr := os.Stat(providerstore.Path()); !os.IsNotExist(statErr) {
		t.Errorf("store created on failure: stat err = %v", statErr)
	}
}

// TestWriteProviderModelsMissingConfig: a missing config file surfaces the
// read error instead of creating one from scratch.
func TestWriteProviderModelsMissingConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	configFile := filepath.Join(t.TempDir(), "config.yaml")
	err := WriteProviderModels(configFile, "aqp", []string{"m1"})
	if err == nil {
		t.Fatal("expected an error for a missing config file")
	}
	if _, statErr := os.Stat(configFile); !os.IsNotExist(statErr) {
		t.Errorf("config file created from scratch: stat err = %v", statErr)
	}
}

// TestWriteProviderModelsRejectsInvalidResult: a would-be store state that
// violates the provider rules (alias key outside models) is refused and
// nothing hits disk.
func TestWriteProviderModelsRejectsInvalidResult(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configFile := filepath.Join(t.TempDir(), "config.yaml")
	// No providers in the yaml: the store is the authority here, so the
	// seeded alias survives into the mutation (yaml entries would win over
	// the store and drop it).
	if err := os.WriteFile(configFile, []byte("listen: 127.0.0.1:15721\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Seed an alias whose key the new model list no longer contains.
	storePath := providerstore.Path()
	if err := providerstore.Save(storePath, map[string]configdomain.Provider{
		"aqp": {Provider: "aqp", OpenAIBaseURL: "https://example.com", Models: []string{"m1"}, Alias: map[string]string{"m1": "m1x"}},
	}); err != nil {
		t.Fatal(err)
	}
	yamlBefore, _ := os.ReadFile(configFile)
	if err := WriteProviderModels(configFile, "aqp", []string{"m2"}); err == nil {
		t.Fatal("expected validation error (alias key m1 no longer in models)")
	}
	if after, _ := os.ReadFile(configFile); string(after) != string(yamlBefore) {
		t.Error("rejected write touched the yaml")
	}
	store, err := providerstore.Load(storePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := store["aqp"].Models; !reflect.DeepEqual(got, []string{"m1"}) {
		t.Errorf("rejected write changed the store: %v", got)
	}
}
