package framework

import (
	"os"
	"path/filepath"
	"testing"

	"model-proxy/internal/accounts"
)

func TestCompactNum(t *testing.T) {
	cases := []struct {
		in   uint64
		want string
	}{
		{0, "0"},
		{5, "5"},
		{567, "567"},
		{1000, "1k"},
		{1234, "1.2k"},
		{450000, "450k"},
		{1000000, "1M"},
		{1200000, "1.2M"},
		{1000000000, "1B"},
		{5600000000, "5.6B"},
		{999, "999"},
		{950000, "950k"},
		{999949, "999.9k"},
		{999999, "1M"},
		{999999999, "1B"},
	}
	for _, c := range cases {
		if got := CompactNum(c.in); got != c.want {
			t.Errorf("CompactNum(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestLoadCmdConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	body := `listen: 127.0.0.1:15721
providers:
  aqp:
    openai_base_url: https://example.invalid/compass-api/v1
    provider_id: aqp
    models: [glm-5.2]
routes: {}
`
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// LoadCmdConfig applies the credentials mode process-wide; restore the
	// file default so later tests in this package are unaffected.
	t.Cleanup(func() { accounts.SetProcessCredentialsMode("file") })
	cfg := LoadCmdConfig([]string{"--config", cfgPath})
	if cfg.Listen != "127.0.0.1:15721" {
		t.Errorf("Listen = %q, want 127.0.0.1:15721", cfg.Listen)
	}
}
