package config

import (
	"strings"
	"testing"
)

const guardTestBaseYAML = `listen: 127.0.0.1:0
providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://x}
routes:
  glm: [{provider: zhipu, model: glm}]
`

func TestGuardSecretsDefaultIsLog(t *testing.T) {
	cfg, err := LoadConfigFromBytes("test", []byte(guardTestBaseYAML))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Guard.SecretsAction(); got != "log" {
		t.Errorf("SecretsAction() = %q, want default %q", got, "log")
	}
}

func TestGuardSecretsValidActions(t *testing.T) {
	for _, action := range []string{"log", "redact", "block", "off"} {
		cfg, err := LoadConfigFromBytes("test", []byte(guardTestBaseYAML+"guard: {secrets: "+action+"}\n"))
		if err != nil {
			t.Fatalf("secrets=%s: %v", action, err)
		}
		if got := cfg.Guard.SecretsAction(); got != action {
			t.Errorf("SecretsAction() = %q, want %q", got, action)
		}
	}
}

func TestGuardSecretsInvalidAction(t *testing.T) {
	_, err := LoadConfigFromBytes("test", []byte(guardTestBaseYAML+"guard: {secrets: drop}\n"))
	if err == nil || !strings.Contains(err.Error(), "guard.secrets") {
		t.Errorf("invalid action: err = %v, want a guard.secrets error", err)
	}
}
