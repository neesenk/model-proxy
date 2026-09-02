package clitest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"model-proxy/internal/accounts"
)

// GrabStdout captures everything written to os.Stdout during fn.
func GrabStdout(t *testing.T, fn func()) string {
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

// WriteTempConfig writes a YAML config body into a temp dir.
func WriteTempConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// SetPoolHome isolates account storage for CLI tests.
func SetPoolHome(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".model-proxy"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", dir)
}

// WritePoolFile writes a plural credential pool for `name` with the given keys.
func WritePoolFile(t *testing.T, name, providerID string, keys ...string) {
	t.Helper()
	pool := accounts.Pool{Version: 1}
	for _, key := range keys {
		pool.Accounts = append(pool.Accounts, accounts.Account{
			ID:    accounts.AccountID(providerID, accounts.Credentials{APIKey: key}),
			Label: key, APIKey: key, AddedAt: "2026-07-08",
		})
	}
	if err := accounts.NewStore(accounts.HomeDir()).Save(name, providerID, pool); err != nil {
		t.Fatal(err)
	}
}

// WriteZhipuPoolConfig writes a one-provider config with a custom usage_url.
func WriteZhipuPoolConfig(t *testing.T, usageURL string) string {
	t.Helper()
	return WriteTempConfig(t, fmt.Sprintf(`listen: 127.0.0.1:15721
providers:
  zhipu:
    openai_base_url: https://zhipu.invalid/api/paas/v4
    provider_id: zhipu
    usage_url: %s
routes: {}
`, usageURL))
}

// WriteJSON responds with v encoded as JSON at the given status (httptest
// daemon stubs for the Do* command cores).
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// MinimalConfig is the shared one-provider CLI fixture.
const MinimalConfig = `listen: 127.0.0.1:15721
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
