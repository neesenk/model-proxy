package cli

import (
	"bytes"
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

	var buf bytes.Buffer
	PrintConfigProvidersTo(&buf, []string{"--config", cfgPath})
	out := buf.String()
	if !strings.Contains(out, "zhipu") {
		t.Errorf("printConfigProviders missing zhipu:\n%s", out)
	}
	if !strings.Contains(out, "provider=zhipu") {
		t.Errorf("printConfigProviders missing provider_id:\n%s", out)
	}
}

func TestPrintConfigProviders_NoConfig(t *testing.T) {
	// Missing config → LoadConfig errors → early return (no output, no panic).
	var buf bytes.Buffer
	PrintConfigProvidersTo(&buf, []string{"--config", filepath.Join(t.TempDir(), "nope.yaml")})
	if strings.TrimSpace(buf.String()) != "" {
		t.Errorf("PrintConfigProviders(missing config) should print nothing: %q", buf.String())
	}
}
