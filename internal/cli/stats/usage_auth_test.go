package stats

import (
	"strings"
	"testing"

	"model-proxy/internal/cli/clitest"
)

func TestCLI_UsageUnknownProvider(t *testing.T) {
	cfg := clitest.WriteTempConfig(t, clitest.MinimalConfig)
	_, stderr, code := clitest.RunCLI(t, "usage", cfg, "nope")
	if code == 0 {
		t.Error("usage nope: exit=0 want non-zero")
	}
	if !strings.Contains(stderr, "unknown provider") {
		t.Errorf("usage nope stderr missing 'unknown provider':\n%s", stderr)
	}
}

func TestCLI_UsageNotLoggedIn(t *testing.T) {
	cfg := clitest.WriteTempConfig(t, clitest.MinimalConfig)
	// HOME is isolated by runCLI → no aqp_oauth_auth.json exists.
	stdout, _, code := clitest.RunCLI(t, "usage", cfg, "aqp")
	if code != 0 {
		t.Fatalf("usage aqp (not logged in) exit=%d want 0", code)
	}
	if !strings.Contains(stdout, "Not logged in") {
		t.Errorf("usage aqp stdout missing 'Not logged in':\n%s", stdout)
	}
}

func TestCLI_UsageAllNotLoggedIn(t *testing.T) {
	cfg := clitest.WriteTempConfig(t, clitest.MinimalConfig)
	stdout, _, code := clitest.RunCLI(t, "usage", cfg)
	if code != 0 {
		t.Fatalf("usage (all, not logged in) exit=%d want 0", code)
	}
	if !strings.Contains(stdout, "Not logged in") {
		t.Errorf("usage (all) stdout missing 'Not logged in':\n%s", stdout)
	}
	// clitest.MinimalConfig has a single provider → NO divider (dividers separate
	// multiple blocks; never before the first/only).
	if strings.Contains(stdout, "────────") {
		t.Errorf("single-provider usage should have no divider:\n%s", stdout)
	}
}

// TestCLI_UsageAllDividersBetweenOnly: `usage` (no arg) with N providers prints
// a divider BETWEEN blocks only — exactly N-1 dividers, none at the very start.
func TestCLI_UsageAllDividersBetweenOnly(t *testing.T) {
	body := "listen: 127.0.0.1:0\nproviders:\n  alpha:\n    provider_id: zhipu\n    openai_base_url: http://x\n  beta:\n    provider_id: deepseek\n    openai_base_url: http://x\n"
	cfg := clitest.WriteTempConfig(t, body)
	stdout, _, code := clitest.RunCLI(t, "usage", cfg)
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
