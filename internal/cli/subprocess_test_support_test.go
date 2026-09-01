package cli

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	clilogin "model-proxy/internal/cli/login"
	climodels "model-proxy/internal/cli/models"
	cliserve "model-proxy/internal/cli/serve"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/provider"
)

// This file covers CLI subcommands that call os.Exit / log.Fatal — they
// cannot be tested in-process (they'd kill the test binary). Instead we run
// the test binary as a subprocess via the TestHelperProcess trick: a hidden
// test entrypoint re-dispatches into the real CLI handler, and the parent
// test asserts on stdout/stderr/exit-code.
//
// Usage data (showXxxUsage) needs real upstreams + credential files, so it's
// not covered here end-to-end; the showXxxUsage parsers are covered by the
// quota parse tests instead.

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
	// Optional CWD override for handlers that touch the working directory
	// (`config init` writes ./config.yaml).
	if d := os.Getenv("MP_CLI_CWD"); d != "" {
		if err := os.Chdir(d); err != nil {
			fmt.Fprintf(os.Stderr, "helper chdir %s: %v\n", d, err)
			os.Exit(2)
		}
	}
	cmd := os.Getenv("MP_SUBCMD")
	switch cmd {
	case "models":
		RunModels(args)
	case "doctor":
		RunDoctor(args)
	case "audit":
		RunAudit(args)
	case "test":
		climodels.CmdTest(args)
	case "schedule":
		RunSchedule(args)
	case "config":
		RunConfig(args)
	case "routes":
		RunRoutes(args)
	case "usage":
		RunUsage(args)
	case "takeover":
		RunTakeover(args)
	case "restore":
		RunRestore(args)
	case "logout":
		RunLogout(args)
	case "stop":
		cliserve.CmdStop(cliserve.DaemonEnv{LoadConfig: configdomain.LoadConfig, Executable: os.Args[0], Stdout: os.Stdout, Stderr: os.Stderr}, cliserve.ParseArgs(args), provider.Yellow, provider.Gray, provider.Green)
	case "reload":
		cliserve.CmdReload(cliserve.DaemonEnv{LoadConfig: configdomain.LoadConfig, Executable: os.Args[0], Stdout: os.Stdout, Stderr: os.Stderr}, cliserve.ParseArgs(args), provider.Yellow, provider.Gray, provider.Green)
	case "login":
		clilogin.CmdLogin(args)
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
	// a provider name or "check"), then --config last. The handlers resolve the
	// subcommand and the config path by scanning the FULL arg list
	// (cliframework.Positional / ConfigPath skip --config and its value
	// wherever they appear), so the ordering is only a convention.
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
	// assertions don't depend on a tty. TMPDIR is pinned too: commands like
	// login/add finish with MaybeReloadDaemon, whose default pid file is
	// <TMPDIR>/model-proxy.pid — without this, a developer's live serve
	// daemon on the same machine would get a real SIGHUP from the test.
	cmd.Env = append(cmd.Env, "HOME="+home, "NO_COLOR=1", "TERM=dumb", "TMPDIR="+t.TempDir())
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

// setStdin replaces os.Stdin with a pipe yielding s, restored on cleanup.
func setStdin(t *testing.T, s string) {
	t.Helper()
	orig := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = orig })
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	w.Close()
}

// runCLIWithStdin is like runCLIWithHome but pipes `stdin` into the subprocess
// (needed for the interactive logout prompt, which reads os.Stdin).
func runCLIWithStdin(t *testing.T, stdin, home, subcmd, cfgPath string, extraArgs ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	tb := os.Args[0]
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
	cmd.Stdin = strings.NewReader(stdin)
	var out, errB bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errB
	// TMPDIR isolation: same MaybeReloadDaemon pid-file rationale as
	// runCLIWithHome — never signal a developer's real serve daemon.
	cmd.Env = append(cmd.Env, "HOME="+home, "NO_COLOR=1", "TERM=dumb", "TMPDIR="+t.TempDir())
	err := cmd.Run()
	exitCode = 0
	if ee, ok := err.(*exec.ExitError); ok {
		exitCode = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("runCLIWithStdin %s: %v", subcmd, err)
	}
	t.Logf("runCLIWithStdin %s: exit=%d\n--- stdout ---\n%s\n--- stderr ---\n%s", subcmd, exitCode, out.String(), errB.String())
	return out.String(), errB.String(), exitCode
}
