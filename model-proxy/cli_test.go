package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// cli_test.go covers CLI subcommands that call os.Exit / log.Fatal — they
// cannot be tested in-process (they'd kill the test binary). Instead we run
// the test binary as a subprocess via the TestHelperProcess trick: a hidden
// test entrypoint re-dispatches into the real CLI handler, and the parent
// test asserts on stdout/stderr/exit-code.
//
// Usage data (showXxxUsage) needs real upstreams + credential files, so it's
// not covered here end-to-end; the showXxxUsage parsers are covered by the
// quota_test.go parse tests instead.

// -- subprocess dispatcher --------------------------------------------------

// TestHelperProcess is the subprocess entrypoint. It is selected by
// -test.run=TestHelperProcess and reads MP_SUBCMD env vars to decide which
// CLI handler to invoke with the args passed after "--".
func TestHelperProcess(t *testing.T) {
	if os.Getenv("MP_CLI_HELPER") != "1" {
		t.Skip("not a helper subprocess")
	}
	// Args after "--" are the user's CLI args.
	args := os.Args[len(os.Args)-1:]
	// When go test passes its own flags, the real args land in MP_CLI_ARGS.
	if a := os.Getenv("MP_CLI_ARGS"); a != "" {
		args = strings.Fields(a)
	}
	cmd := os.Getenv("MP_SUBCMD")
	switch cmd {
	case "models":
		cmdModels(args)
	case "doctor":
		cmdDoctor(args)
	case "schedule":
		cmdSchedule(args)
	case "config":
		cmdConfig(args)
	case "usage":
		cmdUsage(args)
	case "takeover":
		cmdTakeover(args)
	case "restore":
		cmdRestore(args)
	case "logout":
		cmdLogout(args)
	case "stop":
		cmdStop(args)
	case "reload":
		cmdReload(args)
	case "login":
		cmdLogin(args)
	default:
		fmt.Fprintf(os.Stderr, "unknown MP_SUBCMD %q\n", cmd)
		os.Exit(2)
	}
	// Handlers that reach here without exiting return 0.
	os.Exit(0)
}

// runCLI runs the given CLI subcommand in a subprocess with --config pointing
// at cfgPath, returning stdout, stderr, and the exit code. extraArgs are the
// args after the subcommand (e.g. a provider name, or "check" for `config`).
//
// HOME is pinned to a fresh temp dir so the subprocess never touches the real
// ~/.model-proxy (credential/config lookups land in isolation). Tests that need
// to pre-populate the credential dir should use runCLIWithHome.
func runCLI(t *testing.T, subcmd, cfgPath string, extraArgs ...string) (stdout, stderr string, exitCode int) {
	return runCLIWithHome(t, "", subcmd, cfgPath, extraArgs...)
}

// runCLIWithHome is like runCLI but pins HOME to `home` instead of a fresh temp
// dir, so a test can pre-create credential files under <home>/.model-proxy/ and
// assert on them (e.g. logout removing the apikey file). If home is "" a fresh
// temp dir is used (same isolation as runCLI).
func runCLIWithHome(t *testing.T, home, subcmd, cfgPath string, extraArgs ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	tb := os.Args[0]
	// Build the args the handler receives. extraArgs first (positional args like
	// a provider name or "check"), then --config last. This ordering matters:
	//   - cmdConfig reads args[0] as its subcommand ("check"), and configPath
	//     scans args[1:] for --config.
	//   - cmdModels/cmdUsage use positional()/nonFlagArgs(), which skip --config
	//     and its value wherever they appear.
	cliArgs := append([]string{}, extraArgs...)
	if cfgPath != "" {
		cliArgs = append(cliArgs, "--config", cfgPath)
	}

	cmd := exec.Command(tb, "-test.run=TestHelperProcess", "--", subcmd)
	cmd.Env = append(os.Environ(),
		"MP_CLI_HELPER=1",
		"MP_SUBCMD="+subcmd,
		"MP_CLI_ARGS="+strings.Join(cliArgs, " "),
	)
	if home == "" {
		home = t.TempDir()
	}
	var out, errB bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errB
	// Pin HOME (isolation from real ~/.model-proxy) and strip color so
	// assertions don't depend on a tty.
	cmd.Env = append(cmd.Env, "HOME="+home, "NO_COLOR=1", "TERM=dumb")
	err := cmd.Run()
	exitCode = 0
	if ee, ok := err.(*exec.ExitError); ok {
		exitCode = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("runCLI %s: %v", subcmd, err)
	}
	t.Logf("runCLI %s: exit=%d\n--- stdout ---\n%s\n--- stderr ---\n%s", subcmd, exitCode, out.String(), errB.String())
	return out.String(), errB.String(), exitCode
}

// writeTempConfig writes a minimal valid config to a temp file and returns its path.
func writeTempConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/config.yaml"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const minimalConfig = `listen: 127.0.0.1:15721
providers:
  compass:
    openai_base_url: https://example.invalid/compass-api/v1
    anthropic_base_url: https://example.invalid/compass-api
    provider_id: compass
    cqp_mint_url: https://example.invalid/api/v1/cqp/ccswitch/api_key/get_or_generate
    models:
      glm-5.2: {context: 1048576, output: 131072, modalities: {input: [text], output: [text]}}
routes:
  glm-5.2:
    - {provider: compass, model: glm-5.2, priority: 1}
`

// -- C1: `models` lists all exposed models from config ---

func TestCLI_ModelsListsAll(t *testing.T) {
	cfg := writeTempConfig(t, minimalConfig)
	stdout, _, code := runCLI(t, "models", cfg)
	if code != 0 {
		t.Fatalf("models exit=%d want 0", code)
	}
	if !strings.Contains(stdout, "glm-5.2") {
		t.Errorf("models output missing glm-5.2:\n%s", stdout)
	}
	if !strings.Contains(stdout, "compass") {
		t.Errorf("models output missing provider name compass:\n%s", stdout)
	}
}

// -- C2: `models <provider>` lists one provider's models ---

func TestCLI_ModelsOneProvider(t *testing.T) {
	cfg := writeTempConfig(t, minimalConfig)
	stdout, _, code := runCLI(t, "models", cfg, "compass")
	if code != 0 {
		t.Fatalf("models compass exit=%d want 0", code)
	}
	if !strings.Contains(stdout, "glm-5.2") {
		t.Errorf("models compass output missing glm-5.2:\n%s", stdout)
	}
}

// -- C3: `models <unknown>` exits non-zero with an error ---

func TestCLI_ModelsUnknownProviderExits(t *testing.T) {
	cfg := writeTempConfig(t, minimalConfig)
	_, stderr, code := runCLI(t, "models", cfg, "nope")
	if code == 0 {
		t.Error("models nope: exit=0 want non-zero")
	}
	if !strings.Contains(stderr, "unknown provider") {
		t.Errorf("models nope stderr missing 'unknown provider':\n%s", stderr)
	}
}

// -- C4: `doctor` on a valid config prints "config valid" ---

func TestCLI_DoctorValidConfig(t *testing.T) {
	cfg := writeTempConfig(t, minimalConfig)
	stdout, _, code := runCLI(t, "doctor", cfg)
	if code != 0 {
		t.Fatalf("doctor exit=%d want 0", code)
	}
	if !strings.Contains(stdout, "config valid") {
		t.Errorf("doctor output missing 'config valid':\n%s", stdout)
	}
}

// -- C5: `doctor` on an invalid config exits non-zero ---

func TestCLI_DoctorInvalidConfig(t *testing.T) {
	bad := `listen: 127.0.0.1:15721
providers:
  compass:
    openai_base_url: https://x
    provider_id: compass
    anthropic_base_url: https://x/v1   # invalid: ends with /v1
    models: {}
routes: {}
`
	cfg := writeTempConfig(t, bad)
	stdout, _, code := runCLI(t, "doctor", cfg)
	if code == 0 {
		t.Error("doctor invalid config: exit=0 want non-zero")
	}
	if !strings.Contains(stdout, "config invalid") {
		t.Errorf("doctor invalid stdout missing 'config invalid':\n%s", stdout)
	}
}

// -- C6: `schedule` with no daemon running exits non-zero with a reach error ---

func TestCLI_ScheduleNoDaemon(t *testing.T) {
	// Use a port nothing is listening on to guarantee "cannot reach daemon".
	cfg := writeTempConfig(t, "listen: 127.0.0.1:1\nproviders:\n  compass:\n    openai_base_url: https://x\n    provider_id: compass\n    models:\n      m: {context: 1, output: 1, modalities: {input: [text], output: [text]}}\nroutes:\n  m:\n    - {provider: compass, model: m}\n")
	_, stderr, code := runCLI(t, "schedule", cfg)
	if code == 0 {
		t.Error("schedule no daemon: exit=0 want non-zero")
	}
	// Assert the specific "cannot reach" message (not a 3-way OR that a panic
	// stack trace would pass).
	if !strings.Contains(stderr, "cannot reach") {
		t.Errorf("schedule no-daemon stderr missing 'cannot reach':\n%s", stderr)
	}
}

// -- C7: `config check` on a valid config exits 0 ---

func TestCLI_ConfigCheck(t *testing.T) {
	cfg := writeTempConfig(t, minimalConfig)
	stdout, _, code := runCLI(t, "config", cfg, "check")
	if code != 0 {
		t.Fatalf("config check exit=%d want 0", code)
	}
	if !strings.Contains(stdout, "compass") {
		t.Errorf("config check output missing provider compass:\n%s", stdout)
	}
}

// -- C8: `config` with no subcommand exits non-zero ---

func TestCLI_ConfigNoSubcommand(t *testing.T) {
	cfg := writeTempConfig(t, minimalConfig)
	_, _, code := runCLI(t, "config", cfg)
	if code == 0 {
		t.Error("config (no subcommand): exit=0 want non-zero")
	}
}
