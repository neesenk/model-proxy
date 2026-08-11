package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// -- T4: `logout <unknown>` exits non-zero with "unknown provider" ---

func TestCLI_LogoutUnknownProvider(t *testing.T) {
	cfg := writeTempConfig(t, minimalConfig)
	_, stderr, code := runCLI(t, "logout", cfg, "nope")
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
	cfg := writeTempConfig(t, `listen: 127.0.0.1:15721
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

	stdout, _, code := runCLIWithHome(t, home, "logout", cfg, "zhipu")
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
	cfg := writeTempConfig(t, minimalConfig)
	stdout, _, code := runCLI(t, "logout", cfg)
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

func TestCLI_UsageUnknownProvider(t *testing.T) {
	cfg := writeTempConfig(t, minimalConfig)
	_, stderr, code := runCLI(t, "usage", cfg, "nope")
	if code == 0 {
		t.Error("usage nope: exit=0 want non-zero")
	}
	if !strings.Contains(stderr, "unknown provider") {
		t.Errorf("usage nope stderr missing 'unknown provider':\n%s", stderr)
	}
}

// -- T8: `usage <provider>` when not logged in prints "Not logged in" (exit 0) ---

func TestCLI_UsageNotLoggedIn(t *testing.T) {
	cfg := writeTempConfig(t, minimalConfig)
	// HOME is isolated by runCLI → no aqp_oauth_auth.json exists.
	stdout, _, code := runCLI(t, "usage", cfg, "aqp")
	if code != 0 {
		t.Fatalf("usage aqp (not logged in) exit=%d want 0", code)
	}
	if !strings.Contains(stdout, "Not logged in") {
		t.Errorf("usage aqp stdout missing 'Not logged in':\n%s", stdout)
	}
}

// -- T9: `usage` (no provider) iterates all providers, each "Not logged in" ---

func TestCLI_UsageAllNotLoggedIn(t *testing.T) {
	cfg := writeTempConfig(t, minimalConfig)
	stdout, _, code := runCLI(t, "usage", cfg)
	if code != 0 {
		t.Fatalf("usage (all, not logged in) exit=%d want 0", code)
	}
	if !strings.Contains(stdout, "Not logged in") {
		t.Errorf("usage (all) stdout missing 'Not logged in':\n%s", stdout)
	}
	// minimalConfig has a single provider → NO divider (dividers separate
	// multiple blocks; never before the first/only).
	if strings.Contains(stdout, "────────") {
		t.Errorf("single-provider usage should have no divider:\n%s", stdout)
	}
}

// TestCLI_UsageAllDividersBetweenOnly: `usage` (no arg) with N providers prints
// a divider BETWEEN blocks only — exactly N-1 dividers, none at the very start.
func TestCLI_UsageAllDividersBetweenOnly(t *testing.T) {
	body := "listen: 127.0.0.1:0\nproviders:\n  alpha:\n    provider_id: zhipu\n    openai_base_url: http://x\n  beta:\n    provider_id: deepseek\n    openai_base_url: http://x\n"
	cfg := writeTempConfig(t, body)
	stdout, _, code := runCLI(t, "usage", cfg)
	if code != 0 {
		t.Fatalf("usage (all) exit=%d", code)
	}
	// Two providers, neither logged in → exactly 1 divider between them.
	if c := strings.Count(stdout, UsageDivider); c != 1 {
		t.Errorf("want exactly 1 divider between 2 providers, got %d:\n%s", c, stdout)
	}
	// Must not start with the divider.
	if strings.HasPrefix(strings.TrimLeft(stdout, "\n"), UsageDivider) {
		t.Errorf("usage output should not start with a divider:\n%s", stdout)
	}
}
