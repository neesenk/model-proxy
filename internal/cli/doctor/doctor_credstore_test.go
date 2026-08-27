package doctor

import (
	"strings"
	"testing"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/credstore"
)

// TestDoctorShowsCredentialStoreBackend pins the S1 observability closeout:
// doctor names the resolved credstore backend so a degraded keychain fallback
// (auto → file on a headless box) is visible instead of silent. Test binaries
// resolve to file mode (credstore hermeticity guard), which is what the
// assertion pins — the keychain arm renders through the same call site.
func TestDoctorShowsCredentialStoreBackend(t *testing.T) {
	cfg, err := configdomain.LoadConfigFromBytes("config.yaml", []byte(minimalDoctorConfig))
	if err != nil {
		t.Fatalf("config rejected: %v", err)
	}
	if got := credstore.ResolvedMode(); got != credstore.ModeFile {
		t.Fatalf("test binary must resolve file mode (got %q) — the hermeticity guard broke", got)
	}
	out := captureStdout(t, func() { DoctorWithCfg(cfg) })
	if !strings.Contains(out, "credentials: file") {
		t.Errorf("doctor output missing credentials backend line:\n%s", out)
	}
	if !strings.Contains(out, "~/.model-proxy") {
		t.Errorf("file-mode backend must name the storage location:\n%s", out)
	}
}

const minimalDoctorConfig = `
listen: 127.0.0.1:15721
log_level: info
providers:
  zhipu:
    provider_id: zhipu
    openai_base_url: https://open.bigmodel.cn/api/paas/v4
    models: [glm-5.2]
`
