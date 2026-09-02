// Package clitest owns the subprocess CLI test harness shared by the cli
// command package tests: commands that call os.Exit / log.Fatal cannot be
// driven in-process (they would kill the test binary), so tests re-run the
// test binary as a subprocess via the TestHelperProcess trick. Each command
// package defines its own tiny TestHelperProcess that delegates here with the
// handlers it owns:
//
//	func TestHelperProcess(t *testing.T) {
//		clitest.HelperProcess(t, map[string]func([]string){"usage": RunUsage})
//	}
//
// RunCLI / RunCLIWithHome / RunCLIWithStdin are the parent-side runners. Every
// run pins HOME (and TMPDIR, so MaybeReloadDaemon never signals a developer's
// live daemon) to temp dirs — the subprocess never touches the real
// ~/.model-proxy.
//
// The package is test-support only: production command packages must not
// import it.
package clitest

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// HelperProcess is the subprocess entrypoint. It is selected by
// -test.run=TestHelperProcess and reads the MP_CLI_* env vars to decide which
// handler to invoke with the args passed via MP_CLI_ARGS. When the env marker
// is absent (an ordinary `go test` run) it skips.
func HelperProcess(t *testing.T, handlers map[string]func([]string)) {
	t.Helper()
	if os.Getenv("MP_CLI_HELPER") != "1" {
		t.Skip("not a helper subprocess")
	}
	os.Exit(runHelper(os.LookupEnv, os.Args, handlers))
}

// runHelper resolves the dispatch inputs from env/argv and runs the selected
// handler, returning the process exit code. Split from HelperProcess so the
// dispatch logic is testable in-process (a handler that returns normally maps
// to exit 0; handlers may also os.Exit themselves, like the real commands).
func runHelper(lookupEnv func(string) (string, bool), argv []string, handlers map[string]func([]string)) int {
	// Args after "--" are the user's CLI args.
	args := argv[len(argv)-1:]
	// When go test passes its own flags, the real args land in MP_CLI_ARGS.
	// Presence (even empty) wins over the argv fallback: an empty value means
	// "no CLI args", e.g. a bare `shadow` probing its usage path.
	if a, ok := lookupEnv("MP_CLI_ARGS"); ok {
		args = strings.Fields(a)
	}
	// Optional CWD override for handlers that touch the working directory
	// (`config init` writes ./config.yaml).
	if d, ok := lookupEnv("MP_CLI_CWD"); ok && d != "" {
		if err := os.Chdir(d); err != nil {
			fmt.Fprintf(os.Stderr, "helper chdir %s: %v\n", d, err)
			return 2
		}
	}
	cmd, _ := lookupEnv("MP_SUBCMD")
	run, ok := handlers[cmd]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown MP_SUBCMD %q\n", cmd)
		return 2
	}
	run(args)
	// Handlers that reach here without exiting return 0.
	return 0
}

// RunCLI runs the given CLI subcommand in a subprocess with --config pointing
// at cfgPath, returning stdout, stderr, and the exit code. extraArgs are the
// args after the subcommand (e.g. a provider name, or "check" for `config`).
//
// HOME is pinned to a fresh temp dir so the subprocess never touches the real
// ~/.model-proxy (credential/config lookups land in isolation). Tests that
// need to pre-populate the credential dir should use RunCLIWithHome.
func RunCLI(t *testing.T, subcmd, cfgPath string, extraArgs ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	return RunCLIWithHome(t, "", subcmd, cfgPath, extraArgs...)
}

// RunCLIWithHome is like RunCLI but pins HOME to `home` instead of a fresh
// temp dir, so a test can pre-create credential files under
// <home>/.model-proxy/ and assert on them (e.g. logout removing the apikey
// file). If home is "" a fresh temp dir is used (same isolation as RunCLI).
func RunCLIWithHome(t *testing.T, home, subcmd, cfgPath string, extraArgs ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	return runCLICommand(t, nil, home, "", subcmd, cfgPath, extraArgs...)
}

// RunCLIWithStdin is like RunCLIWithHome but pipes `stdin` into the subprocess
// (needed for interactive prompts, which read os.Stdin).
func RunCLIWithStdin(t *testing.T, stdin, home, subcmd, cfgPath string, extraArgs ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	return runCLICommand(t, strings.NewReader(stdin), home, "", subcmd, cfgPath, extraArgs...)
}

// RunCLIInCWD runs the given subcommand in a subprocess with its working
// directory pinned to dir (for commands like `config init` that write
// ./config.yaml). No --config flag is added; args are the full CLI arg list.
func RunCLIInCWD(t *testing.T, dir, subcmd string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	return runCLICommand(t, nil, "", dir, subcmd, "", args...)
}

// runCLICommand is the shared parent-side subprocess runner.
func runCLICommand(t *testing.T, stdin *strings.Reader, home, cwd, subcmd, cfgPath string, extraArgs ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	tb := os.Args[0]
	// Build the args the handler receives. extraArgs first (positional args
	// like a provider name or "check"), then --config last. The handlers
	// resolve the subcommand and the config path by scanning the FULL arg list
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
	if cwd != "" {
		cmd.Env = append(cmd.Env, "MP_CLI_CWD="+cwd)
	}
	if home == "" {
		home = t.TempDir()
	}
	if stdin != nil {
		cmd.Stdin = stdin
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

// SetStdin replaces os.Stdin with a pipe yielding s, restored on cleanup.
func SetStdin(t *testing.T, s string) {
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
