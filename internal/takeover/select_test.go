package takeover_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/routing"
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
	// 10 preset templates collapse to 6 families: claude (model+mcp merged),
	// codex, gemini-cli, kimi, opencode, pi.
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

// --- split mode ---

// mixedNativeCfg exposes three models whose primary targets natively speak
// anthropic (claude-up), openai (zhipu) and responses (codex, via hint).
func mixedNativeCfg() *configdomain.Config {
	return cfgWith(
		map[string]configdomain.Provider{
			"claude-up": {AnthropicBaseURL: "https://c/anthropic/v1", Models: []string{"claude-x"}},
			"zhipu":     {OpenAIBaseURL: "https://z/v1", Models: []string{"glm-5.3"}},
			"codex":     {OpenAIBaseURL: "https://x/v1", Provider: "codex", Models: []string{"gpt-5.4-mini"}},
		},
		nil)
}

func TestResolveClients_SplitPartitionsByNativeProtocol(t *testing.T) {
	clients, err := takeover.ResolveClientsMode(mixedNativeCfg(), "pi", t.TempDir(), takeover.ModeSplit)
	if err != nil {
		t.Fatalf("ResolveClientsMode(pi, split): %v", err)
	}
	want := []string{"pi", "pi-openai", "pi-responses"}
	if got := namesOf(clients); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("split resolved %v, want %v (one entry per native protocol)", got, want)
	}
	if !takeover.SplitWouldChange(mixedNativeCfg(), "pi", t.TempDir()) {
		t.Error("mixed native protocols: SplitWouldChange must be true (CLI prompts)")
	}
}

func TestResolveClients_SplitPartitionsOpencodeByNativeProtocol(t *testing.T) {
	clients, err := takeover.ResolveClientsMode(mixedNativeCfg(), "opencode", t.TempDir(), takeover.ModeSplit)
	if err != nil {
		t.Fatalf("ResolveClientsMode(opencode, split): %v", err)
	}
	want := []string{"opencode", "opencode-openai", "opencode-responses"}
	if got := namesOf(clients); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("split resolved %v, want %v (one entry per native protocol)", got, want)
	}
	if !takeover.SplitWouldChange(mixedNativeCfg(), "opencode", t.TempDir()) {
		t.Error("mixed native protocols: SplitWouldChange must be true for opencode (CLI prompts)")
	}
}

func TestResolveClients_OpencodePicksResponsesWhenMajority(t *testing.T) {
	// Two codex responses-native models vs one openai-native → responses variant,
	// and the note must name the model that will ride the converter.
	cfg := cfgWith(
		map[string]configdomain.Provider{
			"codex": {OpenAIBaseURL: "https://x/v1", Provider: "codex", Models: []string{"gpt-5.4-mini", "gpt-5.5"}},
			"zhipu": {OpenAIBaseURL: "https://z/v1", Models: []string{"glm-5.3"}},
		},
		nil)
	clients, err := takeover.ResolveClients(cfg, "opencode", t.TempDir())
	if err != nil {
		t.Fatalf("ResolveClients(opencode): %v", err)
	}
	if len(clients) != 1 || clients[0].Name != "opencode-responses" {
		t.Fatalf("ResolveClients(opencode) = %v, want [opencode-responses]", namesOf(clients))
	}
	if !strings.Contains(clients[0].Note, "2/3") || !strings.Contains(clients[0].Note, "glm-5.3") {
		t.Errorf("note must report coverage 2/3 and conversion for glm-5.3: %q", clients[0].Note)
	}
}

func TestResolveClients_SplitSkipsUnassignedVariants(t *testing.T) {
	// Everything natively openai → split resolves to just the openai variant
	// (no empty anthropic/responses entries) and matches unified → no prompt.
	cfg := cfgWith(
		map[string]configdomain.Provider{"zhipu": {OpenAIBaseURL: "https://z/v1", Models: []string{"glm-5.3"}}},
		nil)
	clients, err := takeover.ResolveClientsMode(cfg, "pi", t.TempDir(), takeover.ModeSplit)
	if err != nil {
		t.Fatalf("ResolveClientsMode(pi, split): %v", err)
	}
	if got := namesOf(clients); len(got) != 1 || got[0] != "pi-openai" {
		t.Fatalf("split resolved %v, want [pi-openai]", got)
	}
	if takeover.SplitWouldChange(cfg, "pi", t.TempDir()) {
		t.Error("uniform native protocol: SplitWouldChange must be false (no prompt)")
	}
}

func TestResolveClients_SplitNoRoutesFallsBackToDefault(t *testing.T) {
	clients, err := takeover.ResolveClientsMode(&configdomain.Config{}, "pi", t.TempDir(), takeover.ModeSplit)
	if err != nil {
		t.Fatalf("ResolveClientsMode(pi, split) on empty config: %v", err)
	}
	if got := namesOf(clients); len(got) != 1 || got[0] != "pi" {
		t.Fatalf("split with no routes resolved %v, want default variant [pi]", got)
	}
}

func TestResolveClients_MultiNativeModelLandsOnDefaultVariant(t *testing.T) {
	// Provider declares BOTH endpoints → the model is native in anthropic and
	// openai; split must file it under the default variant (pi), and
	// pi-openai (nothing exclusively openai) disappears.
	cfg := cfgWith(
		map[string]configdomain.Provider{
			"both": {OpenAIBaseURL: "https://x/v1", AnthropicBaseURL: "https://x/anthropic/v1", Models: []string{"glm-5.3"}},
		},
		nil)
	clients, err := takeover.ResolveClientsMode(cfg, "pi", t.TempDir(), takeover.ModeSplit)
	if err != nil {
		t.Fatalf("ResolveClientsMode(pi, split): %v", err)
	}
	if got := namesOf(clients); len(got) != 1 || got[0] != "pi" {
		t.Fatalf("multi-native split resolved %v, want [pi] (default absorbs multi-native)", got)
	}
	if takeover.SplitWouldChange(cfg, "pi", t.TempDir()) {
		t.Error("all models on the default variant: SplitWouldChange must be false")
	}
}

func TestRunTakeover_SplitWritesPartitionedEntries(t *testing.T) {
	// Isolate HOME: preset pi variants resolve under it.
	t.Setenv("HOME", t.TempDir())
	home, _ := os.UserHomeDir()
	piFile := filepath.Join(home, ".pi", "agent", "models.json")
	if err := os.MkdirAll(filepath.Dir(piFile), 0o700); err != nil {
		t.Fatal(err)
	}
	original := `{"providers":{"user-entry":{"api":"anthropic-messages"}}}`
	if err := os.WriteFile(piFile, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := mixedNativeCfg()
	cfg.Listen = "127.0.0.1:15721"
	bakDir := filepath.Join(t.TempDir(), ".mp")

	if err := takeover.RunTakeover(cfg, "pi", bakDir, takeover.ModelFacts{SourceDefault: -1, Routes: routesOf(cfg)}, t.TempDir(), takeover.ModeSplit); err != nil {
		t.Fatalf("RunTakeover(pi, split): %v", err)
	}

	data, err := os.ReadFile(piFile)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	// Both natively-spoken protocols got their own entry; the user's
	// pre-existing entry survived; the openai variant (nothing exclusively
	// openai-native... zhipu IS openai-native) — glm-5.3 must sit under the
	// openai entry, claude-x under the anthropic entry, gpt-5.4-mini under
	// responses.
	for _, want := range []string{`"user-entry"`, `"model-proxy"`, `"model-proxy-openai"`, `"model-proxy-responses"`,
		`"anthropic-messages"`, `"openai-completions"`, `"openai-responses"`,
		`"claude-x"`, `"glm-5.3"`, `"gpt-5.4-mini"`} {
		if !strings.Contains(text, want) {
			t.Errorf("split result missing %s:\n%s", want, text)
		}
	}
	// Models are partitioned, not duplicated: each exposed model appears
	// exactly once as a model id.
	for _, m := range []string{`"id": "claude-x"`, `"id": "glm-5.3"`, `"id": "gpt-5.4-mini"`} {
		if n := strings.Count(text, m); n != 1 {
			t.Errorf("model %s appears %d times, want exactly 1 (partitioned):\n%s", m, n, text)
		}
	}
	// Two-phase backup: BOTH variant backups hold the ORIGINAL file — an
	// interleaved backup→rewrite would have captured the pi rewrite into
	// pi-openai.bak.
	for _, name := range []string{"pi", "pi-openai", "pi-responses"} {
		b, err := os.ReadFile(filepath.Join(bakDir, name+".bak"))
		if err != nil {
			t.Fatalf("missing backup %s.bak: %v", name, err)
		}
		if string(b) != original {
			t.Errorf("backup %s.bak = %s, want the original file (two-phase backup)", name, b)
		}
	}
}

func routesOf(cfg *configdomain.Config) map[string][]configdomain.RouteTarget {
	return routing.RouteTable(cfg)
}

// --- protocol-pinned unified mode (--mode <protocol>) ---

func TestResolveClients_ProtocolModePinsRegardlessOfCoverage(t *testing.T) {
	// Everything is anthropic-native, but --mode openai pins the openai
	// variant anyway; the note must say pinned and list what will convert.
	cfg := cfgWith(
		map[string]configdomain.Provider{
			"claude-up": {AnthropicBaseURL: "https://c/anthropic/v1", Models: []string{"claude-x"}},
		},
		nil)
	clients, err := takeover.ResolveClientsMode(cfg, "pi", t.TempDir(), "openai")
	if err != nil {
		t.Fatalf("ResolveClientsMode(pi, openai): %v", err)
	}
	if got := namesOf(clients); len(got) != 1 || got[0] != "pi-openai" {
		t.Fatalf("protocol mode resolved %v, want [pi-openai]", got)
	}
	if !strings.Contains(clients[0].Note, "pinned") || !strings.Contains(clients[0].Note, "claude-x") {
		t.Errorf("note must say pinned and list converting models: %q", clients[0].Note)
	}
}

func TestResolveClients_ProtocolModeFallsBackWhenNoVariant(t *testing.T) {
	// A family without an anthropic variant (user-defined) falls back to the
	// unified auto-selection — "没有就用默认方式".
	dir := t.TempDir()
	body := `file: /tmp/x.json
format: json
client: famx
protocol: openai
json:
  set: {a: b}
`
	if err := os.WriteFile(filepath.Join(dir, "famx.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	body2 := `file: /tmp/y.json
format: json
client: famx
protocol: responses
json:
  set: {a: b}
`
	if err := os.WriteFile(filepath.Join(dir, "famx-r.yaml"), []byte(body2), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := cfgWith(
		map[string]configdomain.Provider{"zhipu": {OpenAIBaseURL: "https://z/v1", Models: []string{"glm-5.3"}}},
		nil)
	clients, err := takeover.ResolveClientsMode(cfg, "famx", dir, "anthropic")
	if err != nil {
		t.Fatalf("ResolveClientsMode(famx, anthropic): %v", err)
	}
	if got := namesOf(clients); len(got) != 1 || got[0] != "famx" {
		t.Fatalf("fallback resolved %v, want [famx] (openai variant, best coverage)", got)
	}
	if !strings.Contains(clients[0].Note, "no anthropic variant") {
		t.Errorf("fallback must be explained in the note: %q", clients[0].Note)
	}
}

func TestResolveClients_ProtocolModeAllMixesPinAndFallback(t *testing.T) {
	// `all --mode openai`: pi/opencode pin their openai variants;
	// single-protocol families (claude, codex, kimi, gemini-cli) have no
	// choice to make and stay on their one template.
	clients, err := takeover.ResolveClientsMode(mixedNativeCfg(), "all", t.TempDir(), "openai")
	if err != nil {
		t.Fatalf("ResolveClientsMode(all, openai): %v", err)
	}
	got := map[string]bool{}
	for _, c := range clients {
		got[c.Name] = true
	}
	for _, want := range []string{"pi-openai", "opencode-openai", "claude", "codex", "kimi", "gemini-cli"} {
		if !got[want] {
			t.Errorf("all --mode openai missing %s in %v", want, namesOf(clients))
		}
	}
	if got["pi"] || got["opencode"] {
		t.Errorf("families with an openai variant must pin it, got %v", namesOf(clients))
	}
}

func TestResolveClients_ProtocolModeExactNameMismatchFails(t *testing.T) {
	// An exact template name that speaks a different protocol than the
	// pinned one is a contradiction — fail closed, don't silently pick.
	_, err := takeover.ResolveClientsMode(mixedNativeCfg(), "pi-responses", t.TempDir(), "openai")
	if err == nil || !strings.Contains(err.Error(), "protocol") {
		t.Fatalf("exact name + mismatched protocol mode: want error, got %v", err)
	}
	clients, err := takeover.ResolveClientsMode(mixedNativeCfg(), "pi-openai", t.TempDir(), "openai")
	if err != nil || len(clients) != 1 || clients[0].Name != "pi-openai" {
		t.Fatalf("exact name + matching protocol mode = %v, %v, want [pi-openai]", namesOf(clients), err)
	}
}

func TestResolveClients_UnknownModeFails(t *testing.T) {
	if _, err := takeover.ResolveClientsMode(&configdomain.Config{}, "pi", t.TempDir(), "bogus"); err == nil ||
		!strings.Contains(err.Error(), "unknown takeover mode") {
		t.Fatalf("unknown mode: want error, got %v", err)
	}
}
