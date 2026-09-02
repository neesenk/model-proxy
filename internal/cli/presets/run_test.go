package presets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"model-proxy/internal/cli/clitest"
)

// TestRunPresetsListExitCode drives the process entry for `presets list`
// (RunPresets os.Exits, so it runs in a subprocess).
func TestRunPresetsListExitCode(t *testing.T) {
	stdout, _, code := clitest.RunCLI(t, "presets", "", "list")
	if code != 0 {
		t.Fatalf("presets list exit = %d, want 0 (output: %s)", code, stdout)
	}
	if !strings.Contains(stdout, "Available provider presets:") {
		t.Errorf("presets list output missing catalog header:\n%s", stdout)
	}
}

// TestRunAddMissingConfig drives the process entry for `add` against a
// nonexistent config file: it must fail with exit 1 and point at
// `config init`, on every platform.
func TestRunAddMissingConfig(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.yaml")
	_, stderr, code := clitest.RunCLI(t, "add", missing, "zhipu")
	if code != 1 {
		t.Fatalf("add with missing config exit = %d, want 1 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stderr, "config init") {
		t.Errorf("add with missing config stderr = %q, want config init hint", stderr)
	}
}

// TestRunAddUsageAndUnknownPreset covers the non-interactive refusal paths
// (subprocess stdin is never a tty): no preset name prints usage, an unknown
// preset name is rejected — both exit 1 on every platform.
func TestRunAddUsageAndUnknownPreset(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, cfg, "listen: 127.0.0.1:17834\nproviders: {}\n")

	_, stderr, code := clitest.RunCLI(t, "add", cfg)
	if code != 1 || !strings.Contains(stderr, "usage: model-proxy add") {
		t.Fatalf("add without preset exit = %d, stderr = %q, want usage refusal", code, stderr)
	}

	_, stderr, code = clitest.RunCLI(t, "add", cfg, "no-such-preset")
	if code != 1 || !strings.Contains(stderr, `unknown preset "no-such-preset"`) {
		t.Fatalf("add unknown preset exit = %d, stderr = %q, want unknown-preset refusal", code, stderr)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
