package config

import (
	"strings"
	"testing"
)

// TestValidate_CatalogAlias verifies that catalog_alias entries refer only to
// models declared by the same provider and never map to an empty catalog id.
func TestValidate_CatalogAlias(t *testing.T) {
	newConfig := func(alias map[string]string) *Config {
		return &Config{
			Listen: "127.0.0.1:1",
			Providers: map[string]Provider{
				"cx": {
					OpenAIBaseURL: "https://x",
					Provider:      "codex",
					Models:        []string{"gpt-5.5"},
					CatalogAlias:  alias,
				},
			},
			Routes: map[string][]RouteTarget{
				"gpt": {{Provider: "cx", Model: "gpt-5.5"}},
			},
		}
	}
	if err := newConfig(map[string]string{"gpt-5.5": "gpt-5"}).validate(); err != nil {
		t.Errorf("valid catalog_alias should be accepted, got %v", err)
	}
	if err := newConfig(nil).validate(); err != nil {
		t.Errorf("nil catalog_alias should be accepted, got %v", err)
	}
	err := newConfig(map[string]string{"gpt-5.5": ""}).validate()
	if err == nil || !strings.Contains(err.Error(), "catalog_alias") || !strings.Contains(err.Error(), "empty") {
		t.Errorf("empty catalog_alias value: want 'catalog_alias … empty' error, got %v", err)
	}
	err = newConfig(map[string]string{"gpt-5.6": "gpt-5"}).validate()
	if err == nil || !strings.Contains(err.Error(), "not in its models: list") {
		t.Errorf("unknown catalog_alias key: want 'not in its models: list' error, got %v", err)
	}
}
