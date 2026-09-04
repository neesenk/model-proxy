package takeover_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/takeover"
)

// select_test.go covers protocol-aware client resolution (select.go): family
// grouping, native-protocol coverage, variant auto-selection, and restore's
// family expansion.

func cfgWith(providers map[string]configdomain.Provider, routes map[string][]configdomain.RouteTarget) *configdomain.Config {
	return &configdomain.Config{Listen: "127.0.0.1:15721", Providers: providers, Routes: routes}
}

func namesOf(clients []takeover.ClientSpec) []string {
	out := make([]string, 0, len(clients))
	for _, c := range clients {
		out = append(out, c.Name)
	}
	return out
}

func TestResolveClients_FamilyPicksNativeProtocol(t *testing.T) {
	// All exposed models sit on openai-native providers → the pi family must
	// resolve to the openai variant, not the anthropic default.
	cfg := cfgWith(
		map[string]configdomain.Provider{
			"zhipu": {OpenAIBaseURL: "https://z/v1", Models: []string{"glm-5.3"}},
			"aqp":   {OpenAIBaseURL: "https://a/v1", Models: []string{"glm-5.3"}},
		},
		map[string][]configdomain.RouteTarget{
			"glm-5.3": {{Provider: "zhipu", Model: "glm-5.3"}, {Provider: "aqp", Model: "glm-5.3"}},
		})
	clients, err := takeover.ResolveClients(cfg, "pi", t.TempDir())
	if err != nil {
		t.Fatalf("ResolveClients(pi): %v", err)
	}
	if len(clients) != 1 || clients[0].Name != "pi-openai" {
		t.Fatalf("ResolveClients(pi) = %v, want [pi-openai]", namesOf(clients))
	}
	if !strings.Contains(clients[0].Note, "openai") || !strings.Contains(clients[0].Note, "1/1") {
		t.Errorf("selection note does not explain the pick: %q", clients[0].Note)
	}
}

func TestResolveClients_FamilyPicksAnthropicWhenNative(t *testing.T) {
	cfg := cfgWith(
		map[string]configdomain.Provider{
			"claude-up": {AnthropicBaseURL: "https://c/anthropic/v1", Models: []string{"claude-x"}},
		},
		map[string][]configdomain.RouteTarget{
			"claude-x": {{Provider: "claude-up", Model: "claude-x"}},
		})
	clients, err := takeover.ResolveClients(cfg, "pi", t.TempDir())
	if err != nil {
		t.Fatalf("ResolveClients(pi): %v", err)
	}
	if len(clients) != 1 || clients[0].Name != "pi" {
		t.Fatalf("ResolveClients(pi) = %v, want [pi] (anthropic-native provider)", namesOf(clients))
	}
}

func TestResolveClients_MajorityWinsAndConversionIsNoted(t *testing.T) {
	// Two openai-native models vs one anthropic-native → openai variant, and
	// the note must name the model that will ride the converter.
	cfg := cfgWith(
		map[string]configdomain.Provider{
			"zhipu":     {OpenAIBaseURL: "https://z/v1", Models: []string{"glm-5.3", "glm-5.3-air"}},
			"claude-up": {AnthropicBaseURL: "https://c/anthropic/v1", Models: []string{"claude-x"}},
		},
		nil)
	clients, err := takeover.ResolveClients(cfg, "opencode", t.TempDir())
	if err != nil {
		t.Fatalf("ResolveClients(opencode): %v", err)
	}
	if len(clients) != 1 || clients[0].Name != "opencode-openai" {
		t.Fatalf("ResolveClients(opencode) = %v, want [opencode-openai]", namesOf(clients))
	}
	if !strings.Contains(clients[0].Note, "2/3") || !strings.Contains(clients[0].Note, "claude-x") {
		t.Errorf("note must report coverage 2/3 and conversion for claude-x: %q", clients[0].Note)
	}
}

func TestResolveClients_ExactNameAlwaysWins(t *testing.T) {
	// Even with every route openai-native, an exact variant name pins it.
	cfg := cfgWith(
		map[string]configdomain.Provider{"zhipu": {OpenAIBaseURL: "https://z/v1", Models: []string{"glm-5.3"}}},
		nil)
	clients, err := takeover.ResolveClients(cfg, "pi-anthropic-does-not-exist", t.TempDir())
	if err == nil {
		t.Fatalf("unknown name must error, got %v", namesOf(clients))
	}
	clients, err = takeover.ResolveClients(cfg, "pi", t.TempDir())
	if err != nil || len(clients) != 1 || clients[0].Name != "pi-openai" {
		t.Fatalf("family pick = %v, %v — sanity check failed", namesOf(clients), err)
	}
	clients, err = takeover.ResolveClients(cfg, "pi-responses", t.TempDir())
	if err != nil || len(clients) != 1 || clients[0].Name != "pi-responses" {
		t.Fatalf("exact name = %v, %v, want [pi-responses]", namesOf(clients), err)
	}
	if clients[0].Note != "" {
		t.Errorf("exact-name resolution must not carry an auto-selection note, got %q", clients[0].Note)
	}
}

func TestResolveClients_TieBreaksToDefaultVariant(t *testing.T) {
	// No routes → no native-protocol signal → the variant named like the
	// family is the default (pi = anthropic, as before this feature).
	clients, err := takeover.ResolveClients(&configdomain.Config{}, "pi", t.TempDir())
	if err != nil {
		t.Fatalf("ResolveClients(pi) on empty config: %v", err)
	}
	if len(clients) != 1 || clients[0].Name != "pi" {
		t.Fatalf("ResolveClients(pi) = %v, want default variant [pi]", namesOf(clients))
	}
}

func TestResolveClients_AllCollapsesToOnePerFamily(t *testing.T) {
	cfg := cfgWith(
		map[string]configdomain.Provider{"zhipu": {OpenAIBaseURL: "https://z/v1", Models: []string{"glm-5.3"}}},
		nil)
	clients, err := takeover.ResolveClients(cfg, "all", t.TempDir())
	if err != nil {
		t.Fatalf("ResolveClients(all): %v", err)
	}
	// 9 preset templates collapse to 6 families: claude, codex, gemini-cli,
	// kimi, opencode, pi.
	if len(clients) != 6 {
		t.Fatalf("ResolveClients(all) = %v, want one per family (6)", namesOf(clients))
	}
	seen := map[string]int{}
	for _, c := range clients {
		seen[c.Template.ClientFamily()]++
	}
	for family, n := range seen {
		if n != 1 {
			t.Errorf("family %s resolved to %d variants, want exactly 1", family, n)
		}
	}
	// The openai-native config must pull the openai variants of both
	// multi-protocol families.
	got := map[string]bool{}
	for _, c := range clients {
		got[c.Name] = true
	}
	if !got["pi-openai"] || !got["opencode-openai"] {
		t.Errorf("openai-native config should select openai variants, got %v", namesOf(clients))
	}
}

func TestResolveClients_UnknownErrorListsFamilies(t *testing.T) {
	_, err := takeover.ResolveClients(&configdomain.Config{}, "nope", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "client families:") {
		t.Fatalf("unknown client error must list families, got %v", err)
	}
}

func TestLoadTemplates_FamilyValidation(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A user variant joining the pi family without a protocol breaks the
	// multi-variant contract and must fail closed.
	write("pi-custom", `file: /tmp/x.json
format: json
client: pi
json:
  set: {a: b}
`)
	if _, err := takeover.LoadTemplates(dir); err == nil || !strings.Contains(err.Error(), "must declare a protocol") {
		t.Fatalf("family member without protocol: want validation error, got %v", err)
	}
	// Duplicate protocol inside one family is ambiguous → hard error too.
	if err := os.Remove(filepath.Join(dir, "pi-custom.yaml")); err != nil {
		t.Fatal(err)
	}
	write("pi-custom", `file: /tmp/x.json
format: json
client: pi
protocol: openai
json:
  set: {a: b}
`)
	if _, err := takeover.LoadTemplates(dir); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("duplicate family protocol: want validation error, got %v", err)
	}
	// A distinct protocol joins cleanly.
	if err := os.Remove(filepath.Join(dir, "pi-custom.yaml")); err != nil {
		t.Fatal(err)
	}
	write("pi-custom", `file: /tmp/x.json
format: json
client: pi
protocol: gemini-ish
json:
  set: {a: b}
`)
	if _, err := takeover.LoadTemplates(dir); err == nil || !strings.Contains(err.Error(), "protocol must be") {
		t.Fatalf("invalid protocol value: want validation error, got %v", err)
	}
}

func TestRunRestore_FamilyRestoresEveryTakenVariant(t *testing.T) {
	// Isolate HOME: preset pi variants all resolve under it.
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	bakDir := filepath.Join(dir, ".mp")
	if err := os.MkdirAll(bakDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Only the pi-openai variant was ever taken over.
	backup := `{"providers":{"model-proxy-openai":{"baseUrl":"http://127.0.0.1:15721/v1"}}}`
	if err := os.WriteFile(filepath.Join(bakDir, "pi-openai.bak"), []byte(backup), 0o600); err != nil {
		t.Fatal(err)
	}
	// The client config exists (takeover only ever backs up existing files).
	home, _ := os.UserHomeDir()
	target := filepath.Join(home, ".pi", "agent", "models.json")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(`{"providers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &configdomain.Config{Listen: "127.0.0.1:15721"}
	// A family name must restore the taken-over variant (skipping the ones
	// with no backup) — even though protocol auto-selection would pick a
	// different variant on this empty config.
	if err := takeover.RunRestore(cfg, "pi", bakDir, t.TempDir()); err != nil {
		t.Fatalf("RunRestore(pi): %v", err)
	}
	b, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("restored file missing: %v", err)
	}
	if string(b) != backup {
		t.Errorf("restored content = %s, want the pi-openai backup", b)
	}
	// The applied marker is gone after a successful restore.
	if _, err := os.Stat(filepath.Join(bakDir, "pi-openai.bak")); !os.IsNotExist(err) {
		t.Errorf("backup marker should be removed after restore, stat err=%v", err)
	}
}
