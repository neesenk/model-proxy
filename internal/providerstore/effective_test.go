package providerstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"model-proxy/internal/config"
)

// TestLoadEffectiveConfigResolveMergesWithoutWriting: the read-only form
// surfaces yaml providers (yaml wins) without touching either file.
func TestLoadEffectiveConfigResolveMergesWithoutWriting(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeSampleConfig(t, dir, sampleConfig)
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg, err := LoadEffectiveConfig(cfgPath, false)
	if err != nil {
		t.Fatalf("LoadEffectiveConfig: %v", err)
	}
	if _, ok := cfg.Providers["zhipu"]; !ok {
		t.Fatalf("providers = %#v, want the yaml's zhipu", cfg.Providers)
	}
	if _, err := os.Stat(filepath.Join(home, ".model-proxy", "provider.json")); !os.IsNotExist(err) {
		t.Fatalf("read-only load created a store (stat err = %v)", err)
	}
	body, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(body), "providers:") {
		t.Fatal("read-only load stripped the yaml")
	}
}

// TestLoadEffectiveConfigAbsorbPersists: the absorbing form migrates the yaml
// section into the store and the resulting cfg keeps every provider.
func TestLoadEffectiveConfigAbsorbPersists(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeSampleConfig(t, dir, sampleConfig)
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg, err := LoadEffectiveConfig(cfgPath, true)
	if err != nil {
		t.Fatalf("LoadEffectiveConfig: %v", err)
	}
	if _, ok := cfg.Providers["zhipu"]; !ok {
		t.Fatalf("providers = %#v", cfg.Providers)
	}
	store, err := Load(filepath.Join(home, ".model-proxy", "provider.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store["zhipu"]; !ok {
		t.Fatalf("store after absorb = %#v", store)
	}
	body, _ := os.ReadFile(cfgPath)
	if strings.Contains(string(body), "providers:") {
		t.Fatalf("yaml not stripped:\n%s", body)
	}
}

// TestLoadEffectiveConfigValidatesEffectiveSet: a route referencing a
// provider that exists ONLY in the store validates through the effective
// merge — the post-migration normal form.
func TestLoadEffectiveConfigValidatesEffectiveSet(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeSampleConfig(t, dir, `listen: 127.0.0.1:15721
routes:
  glm-5.2:
    - provider: zhipu
      model: glm-5.2
`)
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := Save(filepath.Join(home, ".model-proxy", "provider.json"), map[string]config.Provider{
		"zhipu": {Provider: "zhipu", OpenAIBaseURL: "https://z.example.com/v1", Models: []string{"glm-5.2"}},
	}); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadEffectiveConfig(cfgPath, false)
	if err != nil {
		t.Fatalf("effective load must accept a store-only provider: %v", err)
	}
	if _, ok := cfg.Providers["zhipu"]; !ok {
		t.Fatalf("providers = %#v", cfg.Providers)
	}
}

// TestMutateAppliesEditValidatesAndStrips: an edit flows into the store, the
// yaml section is absorbed away, and the effective config validates.
func TestMutateAppliesEditValidatesAndStrips(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeSampleConfig(t, dir, sampleConfig)
	storePath := filepath.Join(dir, "provider.json")

	err := Mutate(cfgPath, storePath, func(providers map[string]config.Provider) error {
		p := providers["zhipu"]
		p.Models = []string{"glm-5.2", "glm-x"}
		providers["zhipu"] = p
		return nil
	})
	if err != nil {
		t.Fatalf("Mutate: %v", err)
	}
	store, err := Load(storePath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(store["zhipu"].Models, []string{"glm-5.2", "glm-x"}) {
		t.Fatalf("store models = %v", store["zhipu"].Models)
	}
	body, _ := os.ReadFile(cfgPath)
	if strings.Contains(string(body), "providers:") {
		t.Fatalf("yaml not stripped:\n%s", body)
	}
}

// TestMutateNoopSkipsWrite: a fn that changes nothing leaves both files alone.
func TestMutateNoopSkipsWrite(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeSampleConfig(t, dir, "listen: 127.0.0.1:15721\n")
	storePath := filepath.Join(dir, "provider.json")
	if err := Save(storePath, map[string]config.Provider{"acme": validProvider()}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(storePath)

	err := Mutate(cfgPath, storePath, func(providers map[string]config.Provider) error {
		if _, ok := providers["acme"]; !ok {
			t.Error("fn must see the store's entries")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Mutate: %v", err)
	}
	after, _ := os.ReadFile(storePath)
	if string(before) != string(after) {
		t.Fatal("no-op mutation rewrote the store")
	}
	if _, err := os.Stat(storePath + ".bak"); !os.IsNotExist(err) {
		t.Fatal("no-op mutation churned a backup")
	}
}

// TestMutateRejectsCrossConfigBreakage: deleting a provider a route still
// references is refused and nothing is written.
func TestMutateRejectsCrossConfigBreakage(t *testing.T) {
	dir := t.TempDir()
	// Two providers, one routed: deleting the ROUTED one must trip the
	// cross-config check (deleting the last provider would surface the
	// at-least-one rule instead).
	cfgPath := writeSampleConfig(t, dir, `listen: 127.0.0.1:15721
providers:
  zhipu:
    provider_id: zhipu
    openai_base_url: https://z.example.com/v4
    models: [glm-5.2]
  spare:
    provider_id: static
    openai_base_url: https://s.example.com/v1
routes:
  glm-5.2:
    - provider: zhipu
      model: glm-5.2
`)
	storePath := filepath.Join(dir, "provider.json")
	before, _ := os.ReadFile(cfgPath)

	err := Mutate(cfgPath, storePath, func(providers map[string]config.Provider) error {
		delete(providers, "zhipu")
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "not defined under providers:") {
		t.Fatalf("err = %v, want a route-references-missing-provider validation error", err)
	}
	if after, _ := os.ReadFile(cfgPath); string(after) != string(before) {
		t.Fatal("rejected mutation touched the yaml")
	}
	if _, err := os.Stat(storePath); !os.IsNotExist(err) {
		t.Fatal("rejected mutation created a store")
	}
}

// TestMutateCancellationBoundary: a cancelled context aborts before fn runs;
// after the store write starts, cancellation no longer blocks completion.
func TestMutateCancellationBoundary(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeSampleConfig(t, dir, sampleConfig)
	storePath := filepath.Join(dir, "provider.json")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := MutateContext(ctx, cfgPath, storePath, func(providers map[string]config.Provider) error {
		t.Fatal("fn must not run under a cancelled context")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// TestCloneIsDeep: mutating the clone must not leak into the original (the
// mutation rollback path depends on this).
func TestCloneIsDeep(t *testing.T) {
	original := map[string]config.Provider{
		"acme": {
			Provider: "static", OpenAIBaseURL: "https://x/v1",
			Models:       []string{"m1"},
			Alias:        map[string]string{"m1": "e1"},
			Capabilities: map[string][]string{"m1": {"image"}},
		},
	}
	cloned := Clone(original)
	cloned["acme"].Models[0] = "mutated"
	cloned["acme"].Alias["m1"] = "mutated"
	cloned["acme"].Capabilities["m1"][0] = "mutated"
	if original["acme"].Models[0] != "m1" || original["acme"].Alias["m1"] != "e1" || original["acme"].Capabilities["m1"][0] != "image" {
		t.Fatalf("Clone is shallow: original = %+v", original["acme"])
	}
}
