package takeover_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	config "model-proxy/internal/config"
	"model-proxy/internal/takeover"
)

// facts_test.go covers ModelFactsFor's chat-reachability filter: models no
// target can serve over anthropic|openai|responses (decisions-only providers
// like typesafe's jev) must not ride into client configs — the decisions
// protocol has no chat conversion, so such entries could only ever fail.

func decisionsAndChatConfig() *config.Config {
	return &config.Config{
		Listen: "127.0.0.1:15721",
		Providers: map[string]config.Provider{
			"zhipu":    {OpenAIBaseURL: "https://z/v1", Models: []string{"glm-5.3"}},
			"typesafe": {Provider: "typesafe", DecisionsBaseURL: "https://ts/v1", Models: []string{"jev-1.13.0"}},
		},
	}
}

func TestModelFactsFor_ExcludesDecisionsOnlyModels(t *testing.T) {
	home := t.TempDir()
	facts := takeover.ModelFactsFor(decisionsAndChatConfig(), "", home, "", takeover.ModeUnified, nil)
	if _, ok := facts.Routes["jev-1.13.0"]; ok {
		t.Errorf("decisions-only model must be dropped from facts.Routes: %v", facts.Routes)
	}
	if _, ok := facts.Routes["glm-5.3"]; !ok {
		t.Errorf("chat model must stay in facts.Routes: %v", facts.Routes)
	}
	if len(facts.Unreachable) != 1 || facts.Unreachable[0] != "jev-1.13.0" {
		t.Fatalf("facts.Unreachable = %v, want [jev-1.13.0]", facts.Unreachable)
	}
}

// TestModelFactsFor_ExcludesOperatorDisabledModels is the regression for the
// bug where takeover's model list still included disabled models: the
// operator disabled-model override (disabled_models.json, the same set
// /v1/models hides by) must drop fully-disabled routes from facts.Routes —
// writing them into a client config hands the agent a dead entry. A
// partially disabled route (any live target) survives.
func TestModelFactsFor_ExcludesOperatorDisabledModels(t *testing.T) {
	cfg := &config.Config{
		Listen: "127.0.0.1:15721",
		Providers: map[string]config.Provider{
			"zhipu":  {OpenAIBaseURL: "https://z/v1", Models: []string{"glm-5.3", "glm-4.7"}},
			"resell": {OpenAIBaseURL: "https://r/v1", Models: []string{"glm-4.7", "glm-4.6"}},
		},
	}
	facts := takeover.ModelFactsFor(cfg, "", t.TempDir(), "", takeover.ModeUnified,
		map[string][]string{"zhipu": {"glm-5.3", "glm-4.7"}, "resell": {"glm-4.7"}})
	if _, ok := facts.Routes["glm-5.3"]; ok {
		t.Errorf("fully disabled model must be dropped from facts.Routes: %v", facts.Routes)
	}
	// glm-4.7 is disabled at BOTH providers → fully disabled, dropped.
	if _, ok := facts.Routes["glm-4.7"]; ok {
		t.Errorf("model disabled at every target must be dropped from facts.Routes: %v", facts.Routes)
	}
	// glm-4.6 is disabled nowhere → stays.
	if _, ok := facts.Routes["glm-4.6"]; !ok {
		t.Errorf("live model must stay in facts.Routes: %v", facts.Routes)
	}
	if len(facts.Disabled) != 2 || facts.Disabled[0] != "glm-4.7" || facts.Disabled[1] != "glm-5.3" {
		t.Fatalf("facts.Disabled = %v, want [glm-4.7 glm-5.3]", facts.Disabled)
	}

	// Partial disable: one live target keeps the route.
	facts = takeover.ModelFactsFor(cfg, "", t.TempDir(), "", takeover.ModeUnified,
		map[string][]string{"zhipu": {"glm-4.7"}})
	if _, ok := facts.Routes["glm-4.7"]; !ok {
		t.Errorf("partially disabled model must stay (live target remains): %v", facts.Routes)
	}
	if len(facts.Disabled) != 0 {
		t.Errorf("facts.Disabled = %v, want empty for a partial disable", facts.Disabled)
	}
}

// TestPruneDisabledRoutes_NilOverrideIsNoop pins the nil/empty contract:
// without an override (first run, no toggles ever) routes pass through
// unchanged, including empty-target routes.
func TestPruneDisabledRoutes_NilOverrideIsNoop(t *testing.T) {
	routes := map[string][]config.RouteTarget{
		"m1": {{Provider: "p", Model: "a"}},
		"m2": {},
	}
	kept, dropped := takeover.PruneDisabledRoutes(routes, nil)
	if len(kept) != 2 || len(dropped) != 0 {
		t.Fatalf("kept=%v dropped=%v, want unchanged", kept, dropped)
	}
}

// TestDisabledModelsForHome verifies the CLI's offline source: the loader
// reads the daemon's persisted disabled_models.json from the home seam; a
// missing file means no override, a malformed one degrades to unfiltered.
func TestDisabledModelsForHome(t *testing.T) {
	home := t.TempDir()
	if got := takeover.DisabledModelsForHome(home); got != nil {
		t.Fatalf("missing file = %v, want nil", got)
	}
	dir := filepath.Join(home, ".model-proxy")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "disabled_models.json"),
		[]byte(`{"version":1,"disabled":{"zhipu":["glm-5.3"]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got := takeover.DisabledModelsForHome(home)
	if len(got) != 1 || len(got["zhipu"]) != 1 || got["zhipu"][0] != "glm-5.3" {
		t.Fatalf("DisabledModelsForHome = %v, want zhipu:[glm-5.3]", got)
	}
	// Malformed file: warn and continue unfiltered (nil), never error.
	if err := os.WriteFile(filepath.Join(dir, "disabled_models.json"), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := takeover.DisabledModelsForHome(home); got != nil {
		t.Fatalf("malformed file = %v, want nil (unfiltered)", got)
	}
}

// TestModelFactsFor_ExcludesNotLoggedInProviders is the regression for the
// operator report that takeover's model list included models whose ONLY
// provider has no account (e.g. grok-4.7 on a never-logged-in opencode-go):
// the effective routing table drops such routes (authNotReady/expandTarget),
// so takeover must too — writing them hands the agent a dead picker entry.
// A model with ANY authenticated target survives; when NOTHING is logged in
// the prune abstains (fresh `config init` still writes model lists).
func TestModelFactsFor_ExcludesNotLoggedInProviders(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Log zhipu in (legacy singular key file) so the authenticated set is
	// non-empty and the prune engages.
	if err := os.MkdirAll(filepath.Join(home, ".model-proxy"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".model-proxy", "zhipu_apikey.json"),
		[]byte(`{"api_key":"SOLO"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Listen: "127.0.0.1:15721",
		Providers: map[string]config.Provider{
			"zhipu": {OpenAIBaseURL: "https://z/v1", Provider: "zhipu", Models: []string{"glm-5.3", "shared"}},
			"ghost": {OpenAIBaseURL: "https://g/v1", Provider: "opencode-go", Models: []string{"grok-only", "shared"}},
		},
	}
	facts := takeover.ModelFactsFor(cfg, "", home, "", takeover.ModeUnified, nil)
	if _, ok := facts.Routes["grok-only"]; ok {
		t.Errorf("model served only by a not-logged-in provider must be dropped: %v", facts.Routes)
	}
	if _, ok := facts.Routes["shared"]; !ok {
		t.Errorf("model with an authenticated target must stay (failover): %v", facts.Routes)
	}
	if _, ok := facts.Routes["glm-5.3"]; !ok {
		t.Errorf("logged-in provider's model must stay: %v", facts.Routes)
	}
	if len(facts.NotLoggedIn) != 1 || facts.NotLoggedIn[0] != "grok-only" {
		t.Fatalf("facts.NotLoggedIn = %v, want [grok-only]", facts.NotLoggedIn)
	}

	// Fresh-setup abstain: with NO provider logged in anywhere, the model
	// list is kept whole (`config init` writes configs before the first
	// login; login + re-takeover fills them).
	fresh := t.TempDir()
	t.Setenv("HOME", fresh)
	facts = takeover.ModelFactsFor(cfg, "", fresh, "", takeover.ModeUnified, nil)
	for _, m := range []string{"grok-only", "shared", "glm-5.3"} {
		if _, ok := facts.Routes[m]; !ok {
			t.Errorf("fresh setup must keep %q (no logins yet): %v", m, facts.Routes)
		}
	}
	if len(facts.NotLoggedIn) != 0 {
		t.Errorf("facts.NotLoggedIn = %v, want empty on abstain", facts.NotLoggedIn)
	}
}

// TestPruneUnauthenticatedRoutes_FusionPasses pins the expandTarget mirror:
// the "fusion" orchestration pseudo-provider carries no credentials of its
// own and must never be pruned by the auth filter.
func TestPruneUnauthenticatedRoutes_FusionPasses(t *testing.T) {
	routes := map[string][]config.RouteTarget{
		"fuse":  {{Provider: "fusion", Model: "m"}},
		"dead":  {{Provider: "ghost", Model: "m"}},
		"multi": {{Provider: "fusion", Model: "m"}, {Provider: "ghost", Model: "m"}},
	}
	kept, dropped := takeover.PruneUnauthenticatedRoutes(routes, map[string]bool{"zhipu": true})
	if _, ok := kept["fuse"]; !ok {
		t.Errorf("fusion-only route must survive: %v", kept)
	}
	if _, ok := kept["multi"]; !ok {
		t.Errorf("route with a fusion target must survive: %v", kept)
	}
	if _, ok := kept["dead"]; ok {
		t.Errorf("unauthenticated-only route must drop: %v", kept)
	}
	if len(dropped) != 1 || dropped[0] != "dead" {
		t.Fatalf("dropped = %v, want [dead]", dropped)
	}
}

// TestPreviewWrites_OperatorDisabledModelExcluded is the end-to-end
// regression for the bug: a rendered client config must never list a model
// the operator disabled at every target — before the prune it rode the
// model list as a dead entry the proxy 404s.
func TestPreviewWrites_OperatorDisabledModelExcluded(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".pi", "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	modelsJSON := filepath.Join(home, ".pi", "agent", "models.json")
	if err := os.WriteFile(modelsJSON, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Listen: "127.0.0.1:15721",
		Providers: map[string]config.Provider{
			"zhipu": {OpenAIBaseURL: "https://z/v1", Models: []string{"glm-5.3", "glm-4.7"}},
		},
	}
	facts := takeover.ModelFactsFor(cfg, "pi", home, "", takeover.ModeUnified,
		map[string][]string{"zhipu": {"glm-4.7"}})

	writes, err := takeover.PreviewWrites(cfg, "pi", "", takeover.ModeUnified, facts, false, takeover.ScopeAll)
	if err != nil {
		t.Fatal(err)
	}
	var main *takeover.PreviewWrite
	for i := range writes {
		if writes[i].File == modelsJSON {
			main = &writes[i]
		}
	}
	if main == nil {
		t.Fatalf("preview must cover models.json: %+v", writes)
	}
	if strings.Contains(main.Content, "glm-4.7") {
		t.Errorf("models.json preview must not contain the disabled model:\n%s", main.Content)
	}
	if !strings.Contains(main.Content, "glm-5.3") {
		t.Errorf("models.json preview missing the live model:\n%s", main.Content)
	}
}

// TestPreviewWrites_DecisionsOnlyModelExcluded is the end-to-end regression:
// rendered client configs (pi's models.json here) must list chat models and
// never the decisions-only jev — before the filter it rode the default
// variant's model list as a dead entry.
func TestPreviewWrites_DecisionsOnlyModelExcluded(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".pi", "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	modelsJSON := filepath.Join(home, ".pi", "agent", "models.json")
	if err := os.WriteFile(modelsJSON, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := decisionsAndChatConfig()
	facts := takeover.ModelFactsFor(cfg, "pi", home, "", takeover.ModeUnified, nil)

	writes, err := takeover.PreviewWrites(cfg, "pi", "", takeover.ModeUnified, facts, false, takeover.ScopeAll)
	if err != nil {
		t.Fatal(err)
	}
	var main *takeover.PreviewWrite
	for i := range writes {
		if writes[i].File == modelsJSON {
			main = &writes[i]
		}
	}
	if main == nil {
		t.Fatalf("preview must cover models.json: %+v", writes)
	}
	if strings.Contains(main.Content, "jev") {
		t.Errorf("models.json preview must not contain the decisions-only model:\n%s", main.Content)
	}
	if !strings.Contains(main.Content, "glm-5.3") {
		t.Errorf("models.json preview missing the chat model:\n%s", main.Content)
	}
}
