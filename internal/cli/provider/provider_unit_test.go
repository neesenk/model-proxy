package provider

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"model-proxy/internal/cli/clitest"
)

// runInProcess drives CmdProvider with buffer streams (the pure decision
// paths need no subprocess: flag parsing, refusals, and the y/N reader).
func runInProcess(t *testing.T, home, cfgPath, stdin string, args ...string) (string, string, int) {
	t.Helper()
	t.Setenv("HOME", home)
	var in, out, errBuf bytes.Buffer
	in.WriteString(stdin)
	code := CmdProvider(args, &in, &out, &errBuf)
	return out.String(), errBuf.String(), code
}

func unitFixture(t *testing.T) (string, string) {
	t.Helper()
	cfgPath := clitest.WriteTempConfig(t, `listen: 127.0.0.1:15721
providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://z.example.com/v4, models: [glm-5.2, glm-5.3]}
routes:
  glm-5.2: [{provider: zhipu, model: glm-5.2}]
`)
	return cfgPath, t.TempDir()
}

func TestCmdProviderUsagePaths(t *testing.T) {
	cfgPath, home := unitFixture(t)
	for _, args := range [][]string{
		{},
		{"bogus", "x"},
		{"add"},
		{"rm"},
		{"set"},
	} {
		call := append([]string{"--config", cfgPath}, args...)
		_, stderr, code := runInProcess(t, home, cfgPath, "", call...)
		if code != 1 || !strings.Contains(stderr, "usage:") {
			t.Errorf("args=%v = (%d, %q), want usage refusal", args, code, stderr)
		}
	}
}

func TestAskYesNoReader(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"y\n", true}, {"YES\n", true}, {" yes \n", true},
		{"n\n", false}, {"\n", false}, {"", false}, // EOF
	} {
		var in, out bytes.Buffer
		in.WriteString(tc.in)
		if got := askYesNo(&in, &out, "ok? "); got != tc.want {
			t.Errorf("askYesNo(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestRunAddFlagRefusals(t *testing.T) {
	cfgPath, home := unitFixture(t)
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no endpoint", []string{"add", "acme"}, "--base-url"},
		{"bad billing", []string{"add", "acme", "--base-url", "https://x/v1", "--billing", "monthly"}, "billing"},
		{"bad priority", []string{"add", "acme", "--base-url", "https://x/v1", "--priority", "-1"}, "priority"},
		{"empty env key", []string{"add", "acme", "--base-url", "https://x/v1", "--api-key-env", "MP_EMPTY_FOR_TEST"}, "environment variable"},
	}
	for _, tc := range cases {
		call := append([]string{"--config", cfgPath}, tc.args...)
		_, stderr, code := runInProcess(t, home, cfgPath, "", call...)
		if code != 1 || !strings.Contains(stderr, tc.want) {
			t.Errorf("%s = (%d, %q), want refusal mentioning %q", tc.name, code, stderr, tc.want)
		}
	}
}

func TestRunAddAmbiguityYesProceeds(t *testing.T) {
	cfgPath, home := unitFixture(t)
	// glm-5.3 is unrouted and zhipu serves it: ambiguous, but --yes proceeds
	// (the non-interactive gate only refuses WITHOUT --yes).
	stdout, _, code := runInProcess(t, home, cfgPath, "",
		"--config", cfgPath, "add", "acme", "--base-url", "https://acme.example.com/v1",
		"--model", "glm-5.3", "--no-login", "--yes")
	if code != 0 || !strings.Contains(stdout, "✓ added provider acme") {
		t.Fatalf("ambiguous add with --yes = (%d, %q)", code, stdout)
	}
}

func TestRunSetRefusals(t *testing.T) {
	cfgPath, home := unitFixture(t)
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"unknown provider", []string{"set", "ghost", "--billing", "plan"}, "unknown provider"},
		{"nothing to set", []string{"set", "zhipu"}, "nothing to set"},
		{"bad billing", []string{"set", "zhipu", "--billing", "monthly"}, "billing"},
		{"bad priority", []string{"set", "zhipu", "--priority", "x"}, "priority"},
	}
	for _, tc := range cases {
		call := append([]string{"--config", cfgPath}, tc.args...)
		_, stderr, code := runInProcess(t, home, cfgPath, "", call...)
		if code != 1 || !strings.Contains(stderr, tc.want) {
			t.Errorf("%s = (%d, %q), want refusal mentioning %q", tc.name, code, stderr, tc.want)
		}
	}
}

func TestRunRemoveRefusals(t *testing.T) {
	cfgPath, home := unitFixture(t)
	// Unknown provider names the available set.
	_, stderr, code := runInProcess(t, home, cfgPath, "", "--config", cfgPath, "rm", "ghost", "--yes")
	if code != 1 || !strings.Contains(stderr, "unknown provider") {
		t.Fatalf("rm unknown = (%d, %q)", code, stderr)
	}
	// Non-interactive without --yes refuses even for a real provider.
	_, stderr, code = runInProcess(t, home, cfgPath, "", "--config", cfgPath, "rm", "zhipu")
	if code != 1 || !strings.Contains(stderr, "--yes") {
		t.Fatalf("rm interactive-only = (%d, %q)", code, stderr)
	}
}

func TestConfigGuardMissingFile(t *testing.T) {
	home := t.TempDir()
	missing := filepath.Join(home, "absent.yaml")
	_, stderr, code := runInProcess(t, home, missing, "", "--config", missing, "add", "acme", "--base-url", "https://x/v1")
	if code != 1 || !strings.Contains(stderr, "config.yaml found") {
		t.Fatalf("missing config = (%d, %q)", code, stderr)
	}
}
