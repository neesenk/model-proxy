package serve

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	configdomain "model-proxy/internal/config"
)

// parsePidFileContents is the shared pid-file parser: leading digits only,
// invalid (and over-long/overflowing) numbers yield 0 so they can never name a
// wrapped-around foreign pid.
func TestParsePidFileContents(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int
	}{
		{"plain pid", "4242\n", 4242},
		{"no newline", "4242", 4242},
		{"stops at non-digit", "123abc\n", 123},
		{"non-numeric", "not-a-pid\n", 0},
		{"empty", "", 0},
		{"zero", "0\n", 0},
		{"negative sign stops parse", "-5\n", 0},
		// 20 nines overflows int64 on 64-bit (max ~9.22e18): must be 0, not a
		// wrapped value that could point at a live process.
		{"overflow", strings.Repeat("9", 20) + "\n", 0},
		{"overflow no newline", strings.Repeat("9", 25), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parsePidFileContents([]byte(tc.raw)); got != tc.want {
				t.Fatalf("parsePidFileContents(%q) = %d, want %d", tc.raw, got, tc.want)
			}
		})
	}
}

// Regression: MaybeReloadDaemon derived its pid path from Args{} (no flags), so
// a daemon started with `--log-file` was never found — reload silently no-opped
// after every login/logout. It must resolve the pid file from the FULL args.
func TestMaybeReloadDaemon_ResolvesPidFileFromArgs(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "daemon.log")
	pidPath := PidFilePath(logFile)

	// A definitely-dead pid (spawned and reaped): MaybeReloadDaemon must find
	// the pid file at the --log-file-derived path and clean the stale entry.
	// With the old Args{} default the path resolved to the temp-dir default
	// and this stale file survived untouched.
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pidPath, []byte(fmt.Sprintf("%d\n", dead.ProcessState.Pid())), 0o644); err != nil {
		t.Fatal(err)
	}

	MaybeReloadDaemon([]string{"--log-file", logFile}, &configdomain.Config{})

	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Fatalf("stale pid file at --log-file path survived: stat err=%v", err)
	}
}

func TestParseServeArgs(t *testing.T) {
	// All cases use an explicit --config so the result doesn't depend on whether
	// ~/.model-proxy/config.yaml exists on the test host.
	cases := []struct {
		name string
		args []string
		want Args
	}{
		{"bare config", []string{"--config", "c.yaml"}, Args{Config: "c.yaml"}},
		{"config=", []string{"--config=/x.yaml"}, Args{Config: "/x.yaml"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseArgs(tc.args)
			if got != tc.want {
				t.Errorf("got %+v want %+v", got, tc.want)
			}
		})
	}
}

func TestParseServeArgs_Extra(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		wantCfg string
		wantLog string
	}{
		{"empty", []string{}, "config.yaml", ""},
		{"config only", []string{"--config", "x.yaml"}, "x.yaml", ""},
		{"log-file value", []string{"--log-file", "/tmp/x.log"}, "config.yaml", "/tmp/x.log"},
		{"log-file= form", []string{"--log-file=/tmp/x.log"}, "config.yaml", "/tmp/x.log"},
		{"both", []string{"--config", "x.yaml", "--log-file", "/tmp/x.log"}, "x.yaml", "/tmp/x.log"},
		{"log-file at end without value", []string{"--log-file"}, "config.yaml", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sa := ParseArgs(tc.args)
			if sa.Config != tc.wantCfg {
				t.Errorf("config=%q want %q", sa.Config, tc.wantCfg)
			}
			if sa.LogFile != tc.wantLog {
				t.Errorf("logFile=%q want %q", sa.LogFile, tc.wantLog)
			}
		})
	}
}
