package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"model-proxy/internal/app"
	"model-proxy/internal/provider"
	"testing"
)

// grabStdout captures everything written to os.Stdout during fn.
func grabStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() { os.Stdout = orig }()
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	w.Close()
	return <-done
}

// writeTempConfig writes a YAML config body into a temp dir.
func writeTempConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// setPoolHome isolates account storage for CLI tests.
func setPoolHome(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".model-proxy"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", dir)
}

// writePoolFile writes a plural credential pool for `name` with the given keys.
func writePoolFile(t *testing.T, name, providerID string, keys ...string) {
	t.Helper()
	pool := app.CredentialPool{Version: 1}
	for _, key := range keys {
		pool.Accounts = append(pool.Accounts, app.PoolAccount{
			ID:    app.AccountIDFor(providerID, app.AccountCred{APIKey: key}),
			Label: key, APIKey: key, AddedAt: "2026-07-08",
		})
	}
	if err := app.SavePool(name, providerID, pool); err != nil {
		t.Fatal(err)
	}
}

// writeZhipuPoolConfig writes a one-provider config with a custom usage_url.
func writeZhipuPoolConfig(t *testing.T, usageURL string) string {
	t.Helper()
	return writeTempConfig(t, fmt.Sprintf(`listen: 127.0.0.1:15721
providers:
  zhipu:
    openai_base_url: https://zhipu.invalid/api/paas/v4
    provider_id: zhipu
    usage_url: %s
routes: {}
`, usageURL))
}

// testProviderID gives CLI wire-record tests a credential-independent upstream.
const testProviderID = "test-static"

func init() {
	provider.Register(testProviderID, func(cfg *provider.Config, providerName string) (provider.Provider, error) {
		staticCfg := *cfg
		staticCfg.ProviderID = "static"
		return provider.New(&staticCfg, providerName)
	})
}

// minimalConfig is the shared one-provider CLI fixture.
const minimalConfig = `listen: 127.0.0.1:15721
providers:
  aqp:
    openai_base_url: https://example.invalid/compass-api/v1
    anthropic_base_url: https://example.invalid/compass-api
    provider_id: aqp
    aqp_mint_url: https://example.invalid/api/v1/cqp/ccswitch/api_key/get_or_generate
    models:
      - glm-5.2
routes:
  glm-5.2:
    - {provider: aqp, model: glm-5.2, priority: 1}
`
