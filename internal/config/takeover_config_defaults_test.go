package config

import (
	"path/filepath"
	"testing"
)

// --- takeover defaults: omitting the block fills standard paths + provider_id ---

func TestTakeoverDefaults_OmitBlock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home) // expandPath expands ~ against $HOME
	cfg, err := LoadConfigFromBytes("test", []byte(`
listen: 127.0.0.1:15721
providers:
  aqp:
    provider_id: aqp
    openai_base_url: https://example.invalid/compass-api/v1
    models:
      - glm-5.2
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// All four paths default to each client's standard location, ~ expanded.
	if cfg.Takeover.Claude != filepath.Join(home, ".claude/settings.json") {
		t.Errorf("claude default=%q want %s", cfg.Takeover.Claude, filepath.Join(home, ".claude/settings.json"))
	}
	if cfg.Takeover.Opencode != filepath.Join(home, ".config/opencode/opencode.json") {
		t.Errorf("opencode default=%q want under $HOME/.config/opencode", cfg.Takeover.Opencode)
	}
	if cfg.Takeover.Codex != filepath.Join(home, ".codex/config.toml") {
		t.Errorf("codex default=%q want under $HOME/.codex", cfg.Takeover.Codex)
	}
	if cfg.Takeover.Pi != filepath.Join(home, ".pi/agent/models.json") {
		t.Errorf("pi default=%q want under $HOME/.pi/agent", cfg.Takeover.Pi)
	}
	if cfg.Takeover.Kimi != filepath.Join(home, ".kimi/config.toml") {
		t.Errorf("kimi default=%q want under $HOME/.kimi", cfg.Takeover.Kimi)
	}
	if cfg.Takeover.ProviderID != "model-proxy" {
		t.Errorf("provider_id default=%q want model-proxy", cfg.Takeover.ProviderID)
	}
	// proxy_url still defaults from listen (existing behavior, unchanged).
	if cfg.Takeover.ProxyURL != "http://127.0.0.1:15721" {
		t.Errorf("proxy_url default=%q want http://127.0.0.1:15721", cfg.Takeover.ProxyURL)
	}
}

// --- takeover defaults: an explicit value wins over the default ---

func TestTakeoverDefaults_ExplicitOverride(t *testing.T) {
	custom := filepath.Join(t.TempDir(), "my-claude.json")
	cfg, err := LoadConfigFromBytes("test", []byte(`
listen: 127.0.0.1:15721
takeover:
  claude: `+custom+`
  provider_id: custom-id
providers:
  aqp:
    provider_id: aqp
    openai_base_url: https://example.invalid/compass-api/v1
    models:
      - glm-5.2
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Takeover.Claude != custom {
		t.Errorf("claude=%q want explicit %q", cfg.Takeover.Claude, custom)
	}
	if cfg.Takeover.ProviderID != "custom-id" {
		t.Errorf("provider_id=%q want custom-id", cfg.Takeover.ProviderID)
	}
	// Non-overridden fields still get defaults (here: opencode path).
	if cfg.Takeover.Opencode == "" {
		t.Errorf("opencode should default when only claude is overridden")
	}
}
