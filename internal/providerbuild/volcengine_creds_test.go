package providerbuild

import (
	"model-proxy/internal/accounts"
	"os"
	"path/filepath"
	"testing"
)

// volcengine_creds_test.go covers LoadVolcengineCreds (the
// {api_key, access_key, secret_key} triple). Pool login, usage display and
// quota behavior are tested by their owner packages.

// --- LoadVolcengineCreds: reads {api_key, access_key, secret_key} ---

func TestLoadVolcengineCreds(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	credDir := filepath.Join(home, ".model-proxy")
	os.MkdirAll(credDir, 0o700)
	os.WriteFile(filepath.Join(credDir, "volcengine_apikey.json"),
		[]byte(`{"api_key":"ark-key","access_key":"ak","secret_key":"sk"}`), 0o600)

	c, err := LoadVolcengineCreds(accounts.HomeDir(), "volcengine")
	if err != nil {
		t.Fatal(err)
	}
	if c.APIKey != "ark-key" || c.AccessKey != "ak" || c.SecretKey != "sk" {
		t.Errorf("LoadVolcengineCreds=%+v", c)
	}
}

func TestLoadVolcengineCreds_Missing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, err := LoadVolcengineCreds(accounts.HomeDir(), "volcengine"); err == nil {
		t.Error("LoadVolcengineCreds missing file: want error, got nil")
	}
}

func TestLoadVolcengineCreds_BadJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	credDir := filepath.Join(home, ".model-proxy")
	os.MkdirAll(credDir, 0o700)
	os.WriteFile(filepath.Join(credDir, "volcengine_apikey.json"), []byte(`not-json`), 0o600)
	if _, err := LoadVolcengineCreds(accounts.HomeDir(), "volcengine"); err == nil {
		t.Error("LoadVolcengineCreds bad JSON: want error, got nil")
	}
}
