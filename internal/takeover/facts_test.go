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
	facts := takeover.ModelFactsFor(decisionsAndChatConfig(), "", home, "", takeover.ModeUnified)
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
	facts := takeover.ModelFactsFor(cfg, "pi", home, "", takeover.ModeUnified)

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
