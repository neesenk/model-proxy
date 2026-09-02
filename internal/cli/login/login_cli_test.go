package login

import (
	"encoding/json"
	"model-proxy/internal/cli/clitest"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLI_LoginNoProvider(t *testing.T) {
	cfg := clitest.WriteTempConfig(t, minimalConfig)
	stdout, _, code := clitest.RunCLI(t, "login", cfg)
	if code != 0 {
		t.Errorf("login (no provider): exit=%d want 0", code)
	}
	if !strings.Contains(stdout, "usage:") || !strings.Contains(stdout, "aqp") {
		t.Errorf("login no provider missing usage/providers:\n%s", stdout)
	}
}
func TestCLI_LoginUnknownProvider(t *testing.T) {
	cfg := clitest.WriteTempConfig(t, minimalConfig)
	_, stderr, code := clitest.RunCLI(t, "login", cfg, "nope")
	if code == 0 {
		t.Error("login nope: exit=0 want non-zero")
	}
	if !strings.Contains(stderr, "unknown provider") {
		t.Errorf("login nope stderr missing 'unknown provider':\n%s", stderr)
	}
}

// --- login credential import (--from-codex / --from-env) -------------------
//
// Subprocess coverage for the flag wiring in clilogin.CmdLogin. All
// credentials are synthetic fixtures under a pinned temp HOME; the only
// network touched is a loopback httptest validation server.

// loginImportConfig builds a config with a codex provider and a zhipu
// provider whose usage_url points at the given validation server.
func loginImportConfig(usageURL string) string {
	return `listen: 127.0.0.1:15721
providers:
  codex:
    provider_id: codex
    openai_base_url: https://chatgpt.com/backend-api/codex
  zhipu:
    provider_id: zhipu
    openai_base_url: https://example.invalid/v1
    usage_url: ` + usageURL + `
`
}

func TestCLI_LoginFromEnv(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	cfg := clitest.WriteTempConfig(t, loginImportConfig(srv.URL))
	home := t.TempDir()
	t.Setenv("MP_CLI_FROM_ENV_KEY", "env-key-abc")

	stdout, stderr, code := clitest.RunCLIWithHome(t, home, "login", cfg, "zhipu", "--from-env", "MP_CLI_FROM_ENV_KEY", "--label", "env")
	if code != 0 {
		t.Fatalf("login --from-env: exit=%d stderr=%s", code, stderr)
	}
	if strings.Contains(stdout, "env-key-abc") || strings.Contains(stderr, "env-key-abc") {
		t.Errorf("key value must never be echoed:\nstdout=%s\nstderr=%s", stdout, stderr)
	}
	// Pool file written with the env key + label.
	data, err := os.ReadFile(filepath.Join(home, ".model-proxy", "zhipu_apikeys.json"))
	if err != nil {
		t.Fatalf("pool file not written: %v", err)
	}
	var pool struct {
		Accounts []struct {
			Label  string `json:"label"`
			APIKey string `json:"api_key"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(data, &pool); err != nil {
		t.Fatalf("parse pool: %v", err)
	}
	if len(pool.Accounts) != 1 || pool.Accounts[0].APIKey != "env-key-abc" || pool.Accounts[0].Label != "env" {
		t.Fatalf("pool wrong: %+v", pool.Accounts)
	}
}

func TestCLI_LoginFromEnv_MissingVar(t *testing.T) {
	cfg := clitest.WriteTempConfig(t, loginImportConfig(""))
	t.Setenv("MP_CLI_MISSING_KEY", "")
	_, stderr, code := clitest.RunCLI(t, "login", cfg, "zhipu", "--from-env", "MP_CLI_MISSING_KEY")
	if code == 0 {
		t.Fatal("login --from-env with empty var: exit=0 want non-zero")
	}
	if !strings.Contains(stderr, "MP_CLI_MISSING_KEY") {
		t.Errorf("stderr must name the variable:\n%s", stderr)
	}
}

func TestCLI_LoginFromCodex(t *testing.T) {
	cfg := clitest.WriteTempConfig(t, loginImportConfig(""))
	home := t.TempDir()
	codexDir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexDir, 0o700); err != nil {
		t.Fatal(err)
	}
	authJSON := `{"OPENAI_API_KEY": null, "tokens": {"id_token": "FIXTURE-ID", "access_token": "FIXTURE-ACCESS", "refresh_token": "FIXTURE-REFRESH", "account_id": "acct-fixture-1234567890"}, "last_refresh": "2026-08-01T02:03:04Z"}`
	if err := os.WriteFile(filepath.Join(codexDir, "auth.json"), []byte(authJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := clitest.RunCLIWithHome(t, home, "login", cfg, "codex", "--from-codex")
	if code != 0 {
		t.Fatalf("login --from-codex: exit=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "refresh") {
		t.Errorf("stdout missing refresh-on-demand note:\n%s", stdout)
	}
	for _, secret := range []string{"FIXTURE-ID", "FIXTURE-ACCESS", "FIXTURE-REFRESH", "acct-fixture-1234567890"} {
		if strings.Contains(stdout, secret) || strings.Contains(stderr, secret) {
			t.Errorf("output leaks fixture material:\nstdout=%s\nstderr=%s", stdout, stderr)
		}
	}
	data, err := os.ReadFile(filepath.Join(home, ".model-proxy", "codex_oauth_auth.json"))
	if err != nil {
		t.Fatalf("oauth auth file not written: %v", err)
	}
	var af struct {
		AuthMode string `json:"auth_mode"`
		Tokens   struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			IDToken      string `json:"id_token"`
			AccountID    string `json:"account_id"`
		} `json:"tokens"`
		LastRefresh string `json:"last_refresh"`
	}
	if err := json.Unmarshal(data, &af); err != nil {
		t.Fatalf("parse written auth file: %v", err)
	}
	if af.AuthMode != "chatgpt" || af.Tokens.AccessToken != "FIXTURE-ACCESS" ||
		af.Tokens.RefreshToken != "FIXTURE-REFRESH" || af.Tokens.IDToken != "FIXTURE-ID" ||
		af.Tokens.AccountID != "acct-fixture-1234567890" || af.LastRefresh != "2026-08-01T02:03:04Z" {
		t.Fatalf("written auth file wrong: %+v", af)
	}
}

func TestCLI_LoginFromCodex_MissingFile(t *testing.T) {
	cfg := clitest.WriteTempConfig(t, loginImportConfig(""))
	_, stderr, code := clitest.RunCLI(t, "login", cfg, "codex", "--from-codex")
	if code == 0 {
		t.Fatal("login --from-codex without auth.json: exit=0 want non-zero")
	}
	if !strings.Contains(stderr, ".codex/auth.json") {
		t.Errorf("stderr must name the missing file:\n%s", stderr)
	}
}

func TestCLI_LoginFromCodex_WrongProvider(t *testing.T) {
	cfg := clitest.WriteTempConfig(t, loginImportConfig(""))
	_, stderr, code := clitest.RunCLI(t, "login", cfg, "zhipu", "--from-codex")
	if code == 0 {
		t.Fatal("login zhipu --from-codex: exit=0 want non-zero")
	}
	if !strings.Contains(stderr, "--from-codex is only valid for codex") {
		t.Errorf("stderr mismatch:\n%s", stderr)
	}
}
