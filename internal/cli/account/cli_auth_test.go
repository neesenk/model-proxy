package account

import (
	"model-proxy/internal/cli/clitest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// -- T4: `logout <unknown>` exits non-zero with "unknown provider" ---

func TestCLI_LogoutUnknownProvider(t *testing.T) {
	cfg := clitest.WriteTempConfig(t, clitest.MinimalConfig)
	_, stderr, code := clitest.RunCLI(t, "logout", cfg, "nope")
	if code == 0 {
		t.Error("logout nope: exit=0 want non-zero")
	}
	if !strings.Contains(stderr, "unknown provider") {
		t.Errorf("logout nope stderr missing 'unknown provider':\n%s", stderr)
	}
}

// -- T5: `logout <provider>` removes the credential file ---

func TestCLI_LogoutRemovesCredFile(t *testing.T) {
	home := t.TempDir()
	cfg := clitest.WriteTempConfig(t, `listen: 127.0.0.1:15721
providers:
  zhipu:
    openai_base_url: https://example.invalid/api/paas/v4
    provider_id: zhipu
    usage_url: https://example.invalid/api/monitor/usage/quota/limit
    models:
      - glm-5.2
routes:
  glm-5.2:
    - {provider: zhipu, model: glm-5.2, priority: 1}
`)
	// Pre-create the apikey credential file in the isolated HOME.
	credDir := filepath.Join(home, ".model-proxy")
	if err := os.MkdirAll(credDir, 0o700); err != nil {
		t.Fatal(err)
	}
	credFile := filepath.Join(credDir, "zhipu_apikey.json")
	if err := os.WriteFile(credFile, []byte(`{"api_key":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, _, code := clitest.RunCLIWithHome(t, home, "logout", cfg, "zhipu")
	if code != 0 {
		t.Fatalf("logout zhipu exit=%d want 0\n--- stdout ---\n%s", code, stdout)
	}
	if !strings.Contains(stdout, "Logged out") {
		t.Errorf("logout zhipu stdout missing 'Logged out':\n%s", stdout)
	}
	if _, err := os.Stat(credFile); !os.IsNotExist(err) {
		t.Errorf("cred file still exists after logout (stat err=%v)", err)
	}
}

// -- T6: `logout` with no provider prints usage and exits 0 ---

func TestCLI_LogoutNoProvider(t *testing.T) {
	cfg := clitest.WriteTempConfig(t, clitest.MinimalConfig)
	stdout, _, code := clitest.RunCLI(t, "logout", cfg)
	if code != 0 {
		t.Fatalf("logout (no provider) exit=%d want 0", code)
	}
	if !strings.Contains(stdout, "usage: model-proxy logout") {
		t.Errorf("logout (no provider) stdout missing usage line:\n%s", stdout)
	}
	if !strings.Contains(stdout, "aqp") {
		t.Errorf("logout (no provider) stdout missing provider list entry aqp:\n%s", stdout)
	}
}

// -- T7: `usage <unknown>` exits non-zero with "unknown provider" ---

// -- T8: `usage <provider>` when not logged in prints "Not logged in" (exit 0) ---

// -- T9: `usage` (no provider) iterates all providers, each "Not logged in" ---
