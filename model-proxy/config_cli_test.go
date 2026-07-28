package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- printConfigProviders: reads config, lists providers ---

func TestPrintConfigProviders(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfgPath, []byte(`listen: 127.0.0.1:15721
providers:
  zhipu:
    openai_base_url: https://open.bigmodel.cn/api/paas/v4
    provider_id: zhipu
    models:
      - glm-5.2
routes:
  glm-5.2:
    - {provider: zhipu, model: glm-5.2}
`), 0o644)

	out := grabStdout(t, func() { printConfigProviders([]string{"--config", cfgPath}) })
	if !strings.Contains(out, "zhipu") {
		t.Errorf("printConfigProviders missing zhipu:\n%s", out)
	}
	if !strings.Contains(out, "provider=zhipu") {
		t.Errorf("printConfigProviders missing provider_id:\n%s", out)
	}
}

func TestPrintConfigProviders_NoConfig(t *testing.T) {
	// Missing config → LoadConfig errors → early return (no output, no panic).
	out := grabStdout(t, func() {
		printConfigProviders([]string{"--config", filepath.Join(t.TempDir(), "nope.yaml")})
	})
	if strings.TrimSpace(out) != "" {
		t.Errorf("printConfigProviders(missing config) should print nothing: %q", out)
	}
}
