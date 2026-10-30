package providerstore

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"model-proxy/internal/config"
)

const sampleConfig = `listen: 127.0.0.1:15721
# top-level comment survives
log_level: info

scheduling:
  quality: 90

providers:
  # provider comment rides the stripped section
  zhipu:
    provider_id: zhipu
    openai_base_url: https://open.bigmodel.cn/api/paas/v4
    models:
      - glm-5.2
      - glm-4.6

routes:
  glm-5.2:
    - provider: zhipu
      model: glm-5.2
`

func writeSampleConfig(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAbsorbMigratesAndStrips(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeSampleConfig(t, dir, sampleConfig)
	storePath := filepath.Join(dir, "provider.json")

	merged, err := Absorb(cfgPath, storePath)
	if err != nil {
		t.Fatalf("Absorb: %v", err)
	}
	zhipu, ok := merged["zhipu"]
	if !ok || zhipu.Provider != "zhipu" || zhipu.OpenAIBaseURL != "https://open.bigmodel.cn/api/paas/v4" {
		t.Fatalf("merged = %#v", merged)
	}
	if !reflect.DeepEqual(zhipu.Models, []string{"glm-5.2", "glm-4.6"}) {
		t.Fatalf("models = %#v", zhipu.Models)
	}

	onDisk, err := Load(storePath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(onDisk, merged) {
		t.Fatalf("store on disk %#v != merged %#v", onDisk, merged)
	}

	stripped, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(stripped)
	if strings.Contains(text, "providers:") || strings.Contains(text, "glm-4.6") {
		t.Fatalf("config.yaml still carries the providers section:\n%s", text)
	}
	for _, keep := range []string{"listen: 127.0.0.1:15721", "# top-level comment survives", "scheduling:", "routes:", "glm-5.2:"} {
		if !strings.Contains(text, keep) {
			t.Fatalf("stripped config lost %q:\n%s", keep, text)
		}
	}
	if perm := filePerm(t, cfgPath); perm != 0o644 {
		t.Fatalf("mode = %v, want preserved 0644", perm)
	}
}

func TestAbsorbIdempotent(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeSampleConfig(t, dir, sampleConfig)
	storePath := filepath.Join(dir, "provider.json")

	first, err := Absorb(cfgPath, storePath)
	if err != nil {
		t.Fatal(err)
	}
	once, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Absorb(cfgPath, storePath)
	if err != nil {
		t.Fatalf("second Absorb: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("second absorb diverged: %#v vs %#v", first, second)
	}
	twice, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(once) != string(twice) {
		t.Fatalf("second absorb rewrote yaml:\n%s\n---\n%s", once, twice)
	}
}

func TestAbsorbYAMLEntriesWin(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "provider.json")
	existing := validProvider()
	existing.OpenAIBaseURL = "https://old.example.com/v1"
	if err := Save(storePath, map[string]config.Provider{"acme": existing}); err != nil {
		t.Fatal(err)
	}
	cfgPath := writeSampleConfig(t, dir, `listen: 127.0.0.1:15721
providers:
  acme:
    provider_id: static
    openai_base_url: https://new.example.com/v1
  beta:
    provider_id: static
    anthropic_base_url: https://beta.example.com
`)

	merged, err := Absorb(cfgPath, storePath)
	if err != nil {
		t.Fatalf("Absorb: %v", err)
	}
	if got := merged["acme"].OpenAIBaseURL; got != "https://new.example.com/v1" {
		t.Fatalf("yaml entry must win on conflict, got %q", got)
	}
	if _, ok := merged["beta"]; !ok {
		t.Fatalf("new yaml entry not absorbed: %#v", merged)
	}
	if _, err := os.Stat(storePath + ".bak"); err != nil {
		t.Fatalf("absorb-overwrite must back up the previous store: %v", err)
	}
}

func TestResolveMergesWithoutWriting(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeSampleConfig(t, dir, sampleConfig)
	storePath := filepath.Join(dir, "provider.json")

	merged, err := Resolve(cfgPath, storePath)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, ok := merged["zhipu"]; !ok {
		t.Fatalf("Resolve lost yaml providers: %#v", merged)
	}
	if _, err := os.Stat(storePath); !os.IsNotExist(err) {
		t.Fatalf("Resolve must not create the store (stat err = %v)", err)
	}
	body, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "providers:") {
		t.Fatal("Resolve must not strip config.yaml")
	}
}

func TestAbsorbWithoutYAMLProvidersIsNoop(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeSampleConfig(t, dir, "listen: 127.0.0.1:15721\n")
	storePath := filepath.Join(dir, "provider.json")
	if err := Save(storePath, map[string]config.Provider{"acme": validProvider()}); err != nil {
		t.Fatal(err)
	}
	before := filePerm(t, cfgPath)

	merged, err := Absorb(cfgPath, storePath)
	if err != nil {
		t.Fatalf("Absorb: %v", err)
	}
	if _, ok := merged["acme"]; !ok {
		t.Fatalf("Absorb dropped store entries: %#v", merged)
	}
	if perm := filePerm(t, cfgPath); perm != before {
		t.Fatalf("yaml rewritten despite no providers (mode %v -> %v)", before, perm)
	}
}

func TestAbsorbMissingConfigYieldsStore(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	storePath := filepath.Join(dir, "provider.json")
	if err := Save(storePath, map[string]config.Provider{"acme": validProvider()}); err != nil {
		t.Fatal(err)
	}
	merged, err := Absorb(cfgPath, storePath)
	if err != nil {
		t.Fatalf("Absorb with missing yaml: %v", err)
	}
	if _, ok := merged["acme"]; !ok {
		t.Fatalf("merged = %#v", merged)
	}
}

func TestAbsorbStripFailureReturnsUsableMap(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission-based strip failure does not apply when running as root")
	}
	cfgDir := t.TempDir()
	storeDir := t.TempDir()
	cfgPath := writeSampleConfig(t, cfgDir, sampleConfig)
	storePath := filepath.Join(storeDir, "provider.json")
	// The config.yaml flock lives in a sibling .lock file — pre-create it so
	// the read-only directory only breaks the STRIP (CreateTemp), which is
	// the failure under test, not lock acquisition.
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml.lock"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(cfgDir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(cfgDir, 0o700) }()

	merged, err := Absorb(cfgPath, storePath)
	var warn *StripWarning
	if err == nil || !errors.As(err, &warn) {
		t.Fatalf("Absorb err = %v, want StripWarning", err)
	}
	if _, ok := merged["zhipu"]; !ok {
		t.Fatalf("merged map must stay usable after a strip failure: %#v", merged)
	}
	if _, err := Load(storePath); err != nil {
		t.Fatalf("store must be persisted even when the strip fails: %v", err)
	}
}

func TestStripPreservesSiblingsAndComments(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeSampleConfig(t, dir, `# leading doc comment
listen: 127.0.0.1:15721  # inline comment

web:
  enabled: true

providers: {}

shadow:
  glm-5.2:
    provider: zhipu
`)
	if err := stripYAMLProviders(cfgPath); err != nil {
		t.Fatalf("strip: %v", err)
	}
	body, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if strings.Contains(text, "providers") {
		t.Fatalf("empty providers map not stripped:\n%s", text)
	}
	for _, keep := range []string{"# leading doc comment", "inline comment", "web:", "shadow:", "glm-5.2:"} {
		if !strings.Contains(text, keep) {
			t.Fatalf("strip lost %q:\n%s", keep, text)
		}
	}
}

func TestStripNonMappingDocumentIsNoop(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeSampleConfig(t, dir, "- just\n- a\n- list\n")
	if err := stripYAMLProviders(cfgPath); err != nil {
		t.Fatalf("strip on non-mapping doc: %v", err)
	}
	body, _ := os.ReadFile(cfgPath)
	if string(body) != "- just\n- a\n- list\n" {
		t.Fatalf("non-mapping doc was rewritten:\n%s", body)
	}
}

func filePerm(t *testing.T, path string) os.FileMode {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Mode().Perm()
}
