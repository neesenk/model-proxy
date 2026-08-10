package doctor

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"model-proxy/internal/app"
	configdomain "model-proxy/internal/config"
)

// setPoolHome isolates account storage for doctor tests.
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

// writeTempConfig writes a YAML config body into a temp dir.
func writeTempConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// mustCfg loads the config at path or fails.
func mustCfg(t *testing.T, path string) *configdomain.Config {
	t.Helper()
	cfg, err := configdomain.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// grabStdout captures os.Stdout during fn.
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

// minimalConfig is the shared one-provider doctor fixture.
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
