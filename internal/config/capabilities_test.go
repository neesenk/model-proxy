package config

import (
	"strings"
	"testing"
)

// TestValidate_Capabilities verifies that capability overrides use known
// values and refer only to models declared by the same provider.
func TestValidate_Capabilities(t *testing.T) {
	newConfig := func(caps map[string][]string) *Config {
		return &Config{
			Listen: "127.0.0.1:1",
			Providers: map[string]Provider{
				"cx": {
					OpenAIBaseURL: "https://x",
					Provider:      "codex",
					Models:        []string{"gpt-5.5"},
					Capabilities:  caps,
				},
			},
			Routes: map[string][]RouteTarget{
				"gpt": {{Provider: "cx", Model: "gpt-5.5"}},
			},
		}
	}
	if err := newConfig(map[string][]string{"gpt-5.5": {"image", "tools"}}).validate(); err != nil {
		t.Errorf("valid capabilities should be accepted, got %v", err)
	}
	if err := newConfig(map[string][]string{"gpt-5.5": {}}).validate(); err != nil {
		t.Errorf("empty capabilities list should be accepted, got %v", err)
	}
	if err := newConfig(nil).validate(); err != nil {
		t.Errorf("nil capabilities should be accepted, got %v", err)
	}
	err := newConfig(map[string][]string{"gpt-5.5": {"vision"}}).validate()
	if err == nil || !strings.Contains(err.Error(), "unknown capability") {
		t.Errorf("invalid capability value: want 'unknown capability' error, got %v", err)
	}
	err = newConfig(map[string][]string{"gpt-5.6": {"image"}}).validate()
	if err == nil || !strings.Contains(err.Error(), "not in its models: list") {
		t.Errorf("unknown capabilities key: want 'not in its models: list' error, got %v", err)
	}
}
