package config

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/provider"
)

// runWizard drives runConfigInitWizard in-process: CWD pinned to dir (the
// wizard writes ./config.yaml), HOME pinned to home (client detection and
// takeover paths resolve there — never the real HOME), input pre-fed as the
// user's typed answers.
func runWizard(t *testing.T, dir, home, input string) (string, error) {
	t.Helper()
	t.Setenv("HOME", home)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = runConfigInitWizard(strings.NewReader(input), &out)
	return out.String(), err
}

// wizardProviderAnswers builds the y/n answer lines for the provider prompts,
// derived from the same authoritative source the wizard uses (template
// providers with a registered implementation, sorted) so the test doesn't
// hardcode a second provider list.
func wizardProviderAnswers(t *testing.T, want map[string]bool) string {
	t.Helper()
	tpl, err := configdomain.LoadConfigFromBytes("config.yaml", []byte(configdomain.DefaultConfigYAML))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for name, p := range tpl.Providers {
		if provider.IsRegistered(p.Provider) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	var sb strings.Builder
	for _, n := range names {
		if want[n] {
			sb.WriteString("y\n")
		} else {
			sb.WriteString("n\n")
		}
	}
	return sb.String()
}

func TestConfigInitWizard_SelectProviders(t *testing.T) {
	dir := t.TempDir()
	out, err := runWizard(t, dir, t.TempDir(), wizardProviderAnswers(t, map[string]bool{"deepseek": true, "zhipu": true}))
	if err != nil {
		t.Fatalf("wizard: %v", err)
	}

	cfg, err := configdomain.LoadConfig(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatalf("generated config does not load: %v", err)
	}
	if len(cfg.Providers) != 2 || cfg.Providers["deepseek"].Provider != "deepseek" || cfg.Providers["zhipu"].Provider != "zhipu" {
		t.Fatalf("providers = %v, want exactly deepseek+zhipu", cfg.Providers)
	}
	// No explicit routes are written; the table is derived from the selected
	// providers, so unselected providers (codex/gpt-6-astra, codex-only model)
	// contribute nothing.
	if len(cfg.Routes) != 0 {
		t.Errorf("wizard wrote explicit routes %v, want none (routes are derived)", cfg.Routes)
	}
	derived := map[string]bool{}
	for _, prov := range cfg.Providers {
		for _, m := range prov.Models {
			derived[prov.ExposedModelName(m)] = true
		}
	}
	if derived["gpt-6-astra"] {
		t.Errorf("gpt-6-astra derived although codex was not selected")
	}

	for _, want := range []string{"model-proxy login deepseek", "model-proxy login zhipu", "model-proxy serve", "model-proxy test deepseek-v4-pro"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing next-step %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "takeover") {
		t.Errorf("no clients detected, yet output mentions takeover:\n%s", out)
	}
}

func TestConfigInitWizard_NoProvidersSelected(t *testing.T) {
	dir := t.TempDir()
	_, err := runWizard(t, dir, t.TempDir(), wizardProviderAnswers(t, map[string]bool{}))
	if err == nil || !strings.Contains(err.Error(), "no providers selected") {
		t.Fatalf("err = %v, want 'no providers selected'", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "config.yaml")); !os.IsNotExist(statErr) {
		t.Fatalf("config.yaml must not be written when nothing is selected")
	}
}

func TestConfigInitWizard_EOFDefaultsToNo(t *testing.T) {
	dir := t.TempDir()
	// Empty input: every prompt reads EOF, which must default to "no".
	_, err := runWizard(t, dir, t.TempDir(), "")
	if err == nil || !strings.Contains(err.Error(), "no providers selected") {
		t.Fatalf("err = %v, want 'no providers selected'", err)
	}
}

func TestConfigInitWizard_TakeoverConfirmed(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	claudeFile := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(claudeFile), 0o755); err != nil {
		t.Fatal(err)
	}
	original := []byte(`{"model":"opus"}`)
	if err := os.WriteFile(claudeFile, original, 0o644); err != nil {
		t.Fatal(err)
	}

	input := wizardProviderAnswers(t, map[string]bool{"codex": true}) + "y\n"
	out, err := runWizard(t, dir, home, input)
	if err != nil {
		t.Fatalf("wizard: %v", err)
	}

	// Idempotent backup landed next to the written config.
	bak, err := os.ReadFile(filepath.Join(dir, ".model-proxy", "claude.bak"))
	if err != nil {
		t.Fatalf("takeover backup missing: %v", err)
	}
	if !bytes.Equal(bak, original) {
		t.Errorf("backup = %q, want verbatim original %q", bak, original)
	}
	// Client config now points at the proxy.
	rewritten, err := os.ReadFile(claudeFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rewritten), "http://127.0.0.1:15721") {
		t.Errorf("claude settings not rewritten to proxy:\n%s", rewritten)
	}
	// Takeover already ran — next steps must not suggest it again.
	if strings.Contains(out, "model-proxy takeover claude") {
		t.Errorf("next steps still suggest takeover after it ran:\n%s", out)
	}
	if !strings.Contains(out, "model-proxy test gpt-5.6-luna") {
		t.Errorf("output missing codex test hint:\n%s", out)
	}
}

func TestConfigInitWizard_TakeoverDeclined(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	claudeFile := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(claudeFile), 0o755); err != nil {
		t.Fatal(err)
	}
	original := []byte(`{"model":"opus"}`)
	if err := os.WriteFile(claudeFile, original, 0o644); err != nil {
		t.Fatal(err)
	}

	input := wizardProviderAnswers(t, map[string]bool{"codex": true}) + "n\n"
	out, err := runWizard(t, dir, home, input)
	if err != nil {
		t.Fatalf("wizard: %v", err)
	}
	got, err := os.ReadFile(claudeFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Errorf("declined takeover modified %s", claudeFile)
	}
	if _, statErr := os.Stat(filepath.Join(dir, ".model-proxy")); !os.IsNotExist(statErr) {
		t.Errorf("declined takeover created a backup dir")
	}
	if !strings.Contains(out, "model-proxy takeover claude") {
		t.Errorf("next steps missing takeover hint after decline:\n%s", out)
	}
}
