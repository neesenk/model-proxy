package main

import (
	"os"
	"path/filepath"
	"testing"
)

// volcengine_creds_test.go covers loadVolcengineCreds + showVolcengineUsage's
// no-AK/SK fallback path (prints configured models + a note).

// --- loadVolcengineCreds: reads {api_key, access_key, secret_key} ---

func TestLoadVolcengineCreds(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	credDir := filepath.Join(home, ".model-proxy")
	os.MkdirAll(credDir, 0o700)
	os.WriteFile(filepath.Join(credDir, "volcengine_apikey.json"),
		[]byte(`{"api_key":"ark-key","access_key":"ak","secret_key":"sk"}`), 0o600)

	c, err := loadVolcengineCreds("volcengine")
	if err != nil {
		t.Fatal(err)
	}
	if c.APIKey != "ark-key" || c.AccessKey != "ak" || c.SecretKey != "sk" {
		t.Errorf("loadVolcengineCreds=%+v", c)
	}
}

func TestLoadVolcengineCreds_Missing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, err := loadVolcengineCreds("volcengine"); err == nil {
		t.Error("loadVolcengineCreds missing file: want error, got nil")
	}
}

func TestLoadVolcengineCreds_BadJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	credDir := filepath.Join(home, ".model-proxy")
	os.MkdirAll(credDir, 0o700)
	os.WriteFile(filepath.Join(credDir, "volcengine_apikey.json"), []byte(`not-json`), 0o600)
	if _, err := loadVolcengineCreds("volcengine"); err == nil {
		t.Error("loadVolcengineCreds bad JSON: want error, got nil")
	}
}

// --- showVolcengineUsage: no AK/SK → prints configured models + note ---

func TestShowVolcengineUsage_NoAKSK(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no cred file
	cfg := &Config{
		Providers: map[string]Provider{
			"volcengine": {
				OpenAIBaseURL: "http://x", Provider: "volcengine",
				Models: map[string]ProviderModel{
					"doubao-seed-2-0-code": {Context: 262144, Output: 32768, Modalities: ProviderModalities{Input: []string{"text"}, Output: []string{"text"}}},
				},
			},
		},
	}
	out := grabStdout(t, func() {
		showVolcengineUsage(cfg, "volcengine", cfg.Providers["volcengine"])
	})
	if !contains(out, "doubao-seed-2-0-code") {
		t.Errorf("showVolcengineUsage missing configured model:\n%s", out)
	}
	if !contains(out, "AK/SK") && !contains(out, "Agent Plan") {
		t.Errorf("showVolcengineUsage missing AK/SK note:\n%s", out)
	}
}
