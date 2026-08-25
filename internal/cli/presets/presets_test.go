package presets

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	configdomain "model-proxy/internal/config"
)

func TestList_DerivesFromTemplateAndFiltersUnregistered(t *testing.T) {
	presets, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(presets) == 0 {
		t.Fatal("catalog must not be empty — template drift or registration regression")
	}
	names := map[string]bool{}
	for _, p := range presets {
		if excludedPresets[p.Name] {
			t.Errorf("preset %q is in the exclusion list but surfaced", p.Name)
		}
		if !providerRegisteredForTest(p.ProviderID) {
			t.Errorf("preset %q (provider_id=%s) has no registered implementation", p.Name, p.ProviderID)
		}
		if len(p.Models) == 0 {
			t.Errorf("preset %q has no default models", p.Name)
		}
		names[p.Name] = true
	}
	for _, want := range []string{"zhipu", "deepseek", "kimi-code", "qwen-plan", "volcengine"} {
		if !names[want] {
			t.Errorf("expected preset %q in catalog; got %v", want, names)
		}
	}
	for i := 1; i < len(presets); i++ {
		if presets[i-1].Name >= presets[i].Name {
			t.Fatalf("catalog not sorted by name: %q >= %q", presets[i-1].Name, presets[i-1].Name)
		}
	}
}

// TestList_EveryPresetBlockValidates proves each catalog entry produces a
// loadable config when merged into a minimal one — a template block that
// validates standalone but breaks after merge would be caught here.
func TestList_EveryPresetBlockValidates(t *testing.T) {
	presets, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, p := range presets {
		t.Run(p.Name, func(t *testing.T) {
			dir := t.TempDir()
			cfgPath := filepath.Join(dir, "config.yaml")
			minimal := "listen: 127.0.0.1:15799\nlog_level: error\n"
			if err := os.WriteFile(cfgPath, []byte(minimal), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := mergePresetBlock(cfgPath, p.Name); err != nil {
				t.Fatalf("mergePresetBlock: %v", err)
			}
			if _, err := configdomain.LoadConfig(cfgPath); err != nil {
				t.Fatalf("merged config for %s does not validate: %v", p.Name, err)
			}
		})
	}
}

func providerRegisteredForTest(providerID string) bool {
	presets, err := List()
	if err != nil {
		return false
	}
	for _, p := range presets {
		if p.ProviderID == providerID {
			return true
		}
	}
	return false
}

// --- mergePresetBlock ---

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

func TestMergePresetBlock_WritesProviderAndStaysValid(t *testing.T) {
	path := writeMinimalConfig(t)
	written, err := mergePresetBlock(path, "deepseek")
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
	if _, err := mergePresetBlock(path, "zhipu"); err != nil {
		t.Fatalf("first merge: %v", err)
	}
	before, _ := os.ReadFile(path)
	written, err := mergePresetBlock(path, "zhipu")
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
	if _, err := mergePresetBlock(path, "nonexistent"); err == nil {
		t.Fatal("unknown preset must fail with an error")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("failed merge must not touch the config file")
	}
}

func TestMergePresetBlock_CreatesProvidersMapWhenMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(minimalConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := mergePresetBlock(path, "zhipu"); err != nil {
		t.Fatalf("merge without providers: key: %v", err)
	}
	if _, err := configdomain.LoadConfig(path); err != nil {
		t.Fatalf("merged config invalid: %v", err)
	}
}

// --- ambiguousModels ---

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
	// zhipu and deepseek both list glm models in the template? Use explicit
	// synthetic providers to pin the semantic regardless of template contents.
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
	got := ambiguousModels(cfg, "zhipu")
	if len(got) != 1 || got[0] != "glm-a" {
		t.Fatalf("ambiguousModels = %v, want [glm-a]", got)
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
	if got := ambiguousModels(cfg, "zhipu"); len(got) != 0 {
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
	if got := ambiguousModels(cfg, "zhipu"); len(got) != 0 {
		t.Fatalf("disjoint model lists must not be ambiguous; got %v", got)
	}
}

// --- interactive helpers (pure functions over injected streams) ---

func TestAskYesNo(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"y\n", true},
		{"yes\n", true},
		{"YES\n", true},
		{"n\n", false},
		{"no\n", false},
		{"\n", false}, // empty line → no
		{"garbage\n", false},
	}
	for _, tc := range cases {
		var out strings.Builder
		if got := askYesNo(strings.NewReader(tc.in), &out, "prompt: "); got != tc.want {
			t.Errorf("askYesNo(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
	// EOF without a newline defaults to no (fail-closed).
	var out strings.Builder
	if askYesNo(strings.NewReader(""), &out, "prompt: ") {
		t.Error("EOF must default to no")
	}
}

func TestBufioReadLine(t *testing.T) {
	r := strings.NewReader("first\nsecond\n")
	got, err := bufioReadLine(r)
	if err != nil || got != "first" {
		t.Fatalf("bufioReadLine = (%q, %v), want first", got, err)
	}
	got2, _ := bufioReadLine(r)
	if got2 != "second" {
		t.Fatalf("second read = %q, want second (must not over-buffer)", got2)
	}
	got3, err := bufioReadLine(strings.NewReader("no-newline"))
	if err == nil || got3 != "no-newline" {
		t.Fatalf("EOF without newline = (%q, %v)", got3, err)
	}
}

func TestPickPresetInteractively(t *testing.T) {
	catalog := []Preset{{Name: "zhipu"}, {Name: "deepseek"}}
	if name, err := pickPresetInteractively(strings.NewReader("1\n"), io.Discard, catalog); err != nil || name != "zhipu" {
		t.Fatalf("pick(1) = (%q, %v)", name, err)
	}
	if name, err := pickPresetInteractively(strings.NewReader("2\n"), io.Discard, catalog); err != nil || name != "deepseek" {
		t.Fatalf("pick(2) = (%q, %v)", name, err)
	}
	for _, in := range []string{"0\n", "99\n", "abc\n"} {
		if _, err := pickPresetInteractively(strings.NewReader(in), io.Discard, catalog); err == nil {
			t.Errorf("pick(%q) must fail", in)
		}
	}
	if _, err := pickPresetInteractively(strings.NewReader(""), io.Discard, catalog); err == nil {
		t.Error("EOF selection must fail")
	}
}

func TestFirstPositional(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"zhipu"}, "zhipu"},
		{[]string{"--config", "/tmp/c.yaml", "zhipu"}, "zhipu"},
		{[]string{"--config=/tmp/c.yaml", "zhipu"}, "zhipu"},
		{[]string{"--label", "work", "zhipu"}, "zhipu"},
		{[]string{"--api-key-env=K", "zhipu"}, "zhipu"},
		{[]string{}, ""},
		{[]string{"--replace"}, ""},
	}
	for _, tc := range cases {
		if got := firstPositional(tc.args); got != tc.want {
			t.Errorf("firstPositional(%v) = %q, want %q", tc.args, got, tc.want)
		}
	}
}

func TestStdinIsInteractiveAndCfgOrHint(t *testing.T) {
	if stdinIsInteractive(strings.NewReader("")) {
		t.Error("strings.Reader is not an interactive terminal")
	}
	if !stdinIsInteractive(os.Stdin) && os.Getenv("CI") != "" {
		t.Log("stdin not a TTY under CI — acceptable")
	}
	if got := cfgOrHint(""); got != "./config.yaml" {
		t.Errorf("cfgOrHint(\"\") = %q", got)
	}
	if got := cfgOrHint("/x/y.yaml"); got != "/x/y.yaml" {
		t.Errorf("cfgOrHint passthrough broken: %q", got)
	}
}
