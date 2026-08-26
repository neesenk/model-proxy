package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	configdomain "model-proxy/internal/config"
)

// --- cmdConfig init: writes config.yaml in the CWD ---

// Non-TTY regression: piped/redirected stdin (scripts, subprocesses) must keep
// the original static behavior — the full annotated template, byte-identical,
// with the exact "wrote config.yaml" stdout line. The subprocess's stdin is
// /dev/null, which is NOT a terminal (the wizard only triggers on a real tty).
func TestCLI_ConfigInit(t *testing.T) {
	dir := t.TempDir()
	stdout, _, code := runCLIInCWD(t, dir, "init")
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

// runCLIInCWD runs `config <args...>` in a subprocess with its working
// directory pinned to dir. Needed because `config init` both writes ./config.yaml
// and os.Exit(1)s on refusal — neither can be driven in-process.
func runCLIInCWD(t *testing.T, dir string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess", "--", "config")
	cmd.Env = append(os.Environ(),
		"MP_CLI_HELPER=1",
		"MP_SUBCMD=config",
		"MP_CLI_ARGS="+strings.Join(args, " "),
		"MP_CLI_CWD="+dir,
		"HOME="+t.TempDir(),
		"NO_COLOR=1",
		"TERM=dumb",
	)
	var out, errB bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errB
	err := cmd.Run()
	exitCode = 0
	if ee, ok := err.(*exec.ExitError); ok {
		exitCode = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("runCLIInCWD config: %v", err)
	}
	t.Logf("runCLIInCWD config %v: exit=%d\n--- stdout ---\n%s\n--- stderr ---\n%s", args, exitCode, out.String(), errB.String())
	return out.String(), errB.String(), exitCode
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

	stdout, stderr, code := runCLIInCWD(t, dir, "init")
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
	stdout, _, code := runCLIInCWD(t, dir, "init")
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
	cfgPath := writeTempConfig(t, minimalConfig)
	// Run in-process: cmdConfig print resolves the config by scanning the FULL
	// args for --config.
	out := grabStdout(t, func() {
		RunConfig([]string{"print", "--config", cfgPath})
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
	cfgPath := writeTempConfig(t, minimalConfig+`guard:
  secrets: block
  audit_path: `+filepath.Join(home, "sec", "security.log")+`
  extra_patterns:
    - {name: myvendor_key, regex: '\bmv-[A-Za-z0-9]{32,}', literal: mv-}
    - {name: other_token, regex: '\bok-[A-Za-z0-9]{32,}'}
  extra_paths:
    - ~/.company/secrets
`)
	out := grabStdout(t, func() {
		RunConfig([]string{"check", "--config", cfgPath})
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
	out = grabStdout(t, func() {
		RunConfig([]string{"check", "--config", writeTempConfig(t, minimalConfig)})
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
	cfgPath := writeTempConfig(t, minimalConfig)
	out := grabStdout(t, func() {
		RunConfig([]string{"--config", cfgPath, "check"})
	})
	if !strings.Contains(out, "config valid") {
		t.Errorf("config check with leading --config missing success line:\n%s", out)
	}
	if !strings.Contains(out, "listen:") {
		t.Errorf("config check with leading --config missing listen line:\n%s", out)
	}
}
