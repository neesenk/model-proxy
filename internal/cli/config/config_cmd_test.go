package config

import (
	"model-proxy/internal/cli/clitest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"model-proxy/internal/accounts"
	configdomain "model-proxy/internal/config"
)

// --- cmdConfig init: writes config.yaml in the CWD ---

// Non-TTY regression: piped/redirected stdin (scripts, subprocesses) must keep
// the original static behavior — the full annotated template, byte-identical,
// with the exact "wrote config.yaml" stdout line. The subprocess's stdin is
// /dev/null, which is NOT a terminal (the wizard only triggers on a real tty).
func TestCLI_ConfigInit(t *testing.T) {
	dir := t.TempDir()
	stdout, _, code := clitest.RunCLIInCWD(t, dir, "config", "init")
	if code != 0 {
		t.Fatalf("config init exit=%d want 0", code)
	}
	if stdout != "wrote config.yaml\n" {
		t.Errorf("stdout = %q, want exactly %q", stdout, "wrote config.yaml\n")
	}
	data, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatalf("config init did not write config.yaml: %v", err)
	}
	if string(data) != configdomain.DefaultConfigYAML {
		t.Errorf("non-TTY config init must write the static template verbatim")
	}
}

// Regression: `config init` used to unconditionally overwrite ./config.yaml,
// silently destroying a live config. An existing file must be refused (exit
// non-zero) and kept byte-identical.
func TestCLI_ConfigInitRefusesExistingFile(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "config.yaml")
	original := []byte("listen: 127.0.0.1:15999\nproviders: {}\n")
	if err := os.WriteFile(existing, original, 0o644); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := clitest.RunCLIInCWD(t, dir, "config", "init")
	if code == 0 {
		t.Fatalf("config init over an existing file exited 0, want non-zero\n%s", stdout)
	}
	if !strings.Contains(stderr, "already exists") {
		t.Errorf("stderr missing refusal message:\n%s", stderr)
	}
	got, err := os.ReadFile(existing)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Fatalf("existing config.yaml was modified:\n got %q\nwant %q", got, original)
	}
}

// Companion: init in an empty directory still writes the template.
func TestCLI_ConfigInitFreshDirectory(t *testing.T) {
	dir := t.TempDir()
	stdout, _, code := clitest.RunCLIInCWD(t, dir, "config", "init")
	if code != 0 {
		t.Fatalf("config init in empty dir exit=%d want 0\n%s", code, stdout)
	}
	data, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatalf("config init did not write config.yaml: %v", err)
	}
	if !strings.Contains(string(data), "providers:") {
		t.Errorf("config init wrote unexpected content:\n%s", data)
	}
}

// --- cmdConfig print: prints listen + providers + routes ---

func TestCLI_ConfigPrint(t *testing.T) {
	cfgPath := clitest.WriteTempConfig(t, clitest.MinimalConfig)
	// Run in-process: cmdConfig print resolves the config by scanning the FULL
	// args for --config.
	out := clitest.GrabStdout(t, func() {
		CmdConfigRun([]string{"print", "--config", cfgPath})
	})
	if !strings.Contains(out, "listen:") || !strings.Contains(out, "aqp") {
		t.Errorf("config print missing content:\n%s", out)
	}
}

// TestCLI_ConfigCheckGuardSummary: `config check` ends the summary with the
// effective guard settings — actions/toggles, audit path (default derived
// from HOME), and the custom pattern/path extensions counted and named. The
// built-in rule tables are reported as embedded, not counted, so the CLI
// stays free of an internal/guard dependency edge.
func TestCLI_ConfigCheckGuardSummary(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfgPath := clitest.WriteTempConfig(t, clitest.MinimalConfig+`guard:
  secrets: block
  audit_path: `+filepath.Join(home, "sec", "security.log")+`
  extra_patterns:
    - {name: myvendor_key, regex: '\bmv-[A-Za-z0-9]{32,}', literal: mv-}
    - {name: other_token, regex: '\bok-[A-Za-z0-9]{32,}'}
  extra_paths:
    - ~/.company/secrets
`)
	out := clitest.GrabStdout(t, func() {
		CmdConfigRun([]string{"check", "--config", cfgPath})
	})
	for _, want := range []string{
		"guard: secrets=block known_secrets=true decode=true paths=log audit=true",
		"audit_path: " + filepath.Join(home, "sec", "security.log"),
		"patterns: built-in tables (embedded) + 2 custom (myvendor_key, other_token)",
		"extra_paths: 1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("config check guard summary missing %q:\n%s", want, out)
		}
	}

	// Defaults: no guard block -> all defaults effective, audit_path derived
	// from HOME, no custom extensions.
	out = clitest.GrabStdout(t, func() {
		CmdConfigRun([]string{"check", "--config", clitest.WriteTempConfig(t, clitest.MinimalConfig)})
	})
	for _, want := range []string{
		"guard: secrets=log known_secrets=true decode=true paths=log audit=true",
		"audit_path: " + filepath.Join(home, ".model-proxy", "security.log"),
		"patterns: built-in tables (embedded) + 0 custom",
		"extra_paths: 0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("config check default guard summary missing %q:\n%s", want, out)
		}
	}
}

// Regression: `model-proxy config --config X check` used to drop the leading
// --config (ConfigPath(args[1:]) plus args[0]-based dispatch), so it failed
// with "unknown config subcommand" or loaded the wrong config. The flag must
// be honored wherever it appears, like every other command.
func TestCLI_ConfigCheckFlagBeforeSubcommand(t *testing.T) {
	cfgPath := clitest.WriteTempConfig(t, clitest.MinimalConfig)
	out := clitest.GrabStdout(t, func() {
		CmdConfigRun([]string{"--config", cfgPath, "check"})
	})
	if !strings.Contains(out, "config valid") {
		t.Errorf("config check with leading --config missing success line:\n%s", out)
	}
	if !strings.Contains(out, "listen:") {
		t.Errorf("config check with leading --config missing listen line:\n%s", out)
	}
}

// TestCLI_ConfigCheckCredentialsSummary: `config check` reports both
// credential stores' effective mode + source, and flags the one divergence
// possible after switch convergence — MP_CRED_STORE overriding only the OAuth
// side while pools follow config.
func TestCLI_ConfigCheckCredentialsSummary(t *testing.T) {
	cfgPath := clitest.WriteTempConfig(t, "credentials: keychain\n"+clitest.MinimalConfig)
	// CmdConfigRun applies the config's credentials mode process-wide; restore
	// the file default so later tests in this package are unaffected.
	t.Cleanup(func() { accounts.SetProcessCredentialsMode("file") })

	// Env override on the OAuth side only → visible mismatch line.
	t.Setenv("MP_CRED_STORE", "file")
	out := clitest.GrabStdout(t, func() {
		CmdConfigRun([]string{"check", "--config", cfgPath})
	})
	for _, want := range []string{
		"credentials: pools=keychain (config credentials:) oauth=file (env MP_CRED_STORE)",
		"credentials mode mismatch",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("config check credentials summary missing %q:\n%s", want, out)
		}
	}

	// No env override: config drives both sides — no mismatch line. (The
	// test-binary guard keeps the OAuth side resolved to file/default.)
	t.Setenv("MP_CRED_STORE", "")
	out = clitest.GrabStdout(t, func() {
		CmdConfigRun([]string{"check", "--config", cfgPath})
	})
	if !strings.Contains(out, "credentials: pools=keychain (config credentials:)") {
		t.Errorf("config check credentials summary missing pools line:\n%s", out)
	}
	if strings.Contains(out, "credentials mode mismatch") {
		t.Errorf("config check must not flag a mismatch without an env override:\n%s", out)
	}
}
