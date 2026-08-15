package serve_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	serve "model-proxy/internal/cli/serve"
	configdomain "model-proxy/internal/config"
)

func TestResolveLogFile(t *testing.T) {
	cfg := &configdomain.Config{LogFile: "/from/config.log"}
	sa := serve.Args{Config: "/etc/model-proxy/config.yaml"}

	// config used
	if got := serve.ResolveLogFile(sa, cfg); got != "/from/config.log" {
		t.Errorf("config: got %q", got)
	}
	// default: the OS temp dir (runtime artifacts), e.g. /tmp on Linux, $TMPDIR on macOS
	wantDefault := filepath.Join(os.TempDir(), "model-proxy.log")
	if got := serve.ResolveLogFile(serve.Args{Config: "/etc/model-proxy/config.yaml"}, &configdomain.Config{}); got != wantDefault {
		t.Errorf("default: got %q want %q", got, wantDefault)
	}
}

func TestPidFilePath(t *testing.T) {
	for _, tc := range []struct {
		name    string
		logFile string
		want    string
	}{
		{name: "log suffix", logFile: "/var/log/model-proxy.log", want: "/var/log/model-proxy.pid"},
		{name: "non-log suffix", logFile: "/var/run/model-proxy", want: "/var/run/model-proxy.pid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := serve.PidFilePath(tc.logFile); got != tc.want {
				t.Fatalf("PidFilePath(%q) = %q, want %q", tc.logFile, got, tc.want)
			}
		})
	}
}

func TestOpenLogFileCreatesParentAndAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "model-proxy.log")

	first, err := serve.OpenLogFile(path)
	if err != nil {
		t.Fatalf("OpenLogFile(first): %v", err)
	}
	if _, err := first.WriteString("first\n"); err != nil {
		first.Close()
		t.Fatalf("write first log: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first log: %v", err)
	}

	second, err := serve.OpenLogFile(path)
	if err != nil {
		t.Fatalf("OpenLogFile(second): %v", err)
	}
	if _, err := second.WriteString("second\n"); err != nil {
		second.Close()
		t.Fatalf("write second log: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("close second log: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if want := "first\nsecond\n"; string(got) != want {
		t.Fatalf("log content = %q, want %q", got, want)
	}
}

func TestWritePidFileExactContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model-proxy.pid")
	if err := serve.WritePidFile(path, 4242); err != nil {
		t.Fatalf("WritePidFile: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pid file: %v", err)
	}
	if want := "4242\n"; string(got) != want {
		t.Fatalf("pid file content = %q, want %q", got, want)
	}
}

func TestDaemonizeRefusesSecondDaemon(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "model-proxy.log")
	pidFile := serve.PidFilePath(logFile)
	if err := serve.WritePidFile(pidFile, os.Getpid()); err != nil {
		t.Fatalf("write live pid: %v", err)
	}

	configPath := filepath.Join(dir, "config.yaml")
	loadCalls := 0
	err := serve.Daemonize(serve.DaemonEnv{
		LoadConfig: func(path string) (*configdomain.Config, error) {
			loadCalls++
			if path != configPath {
				t.Fatalf("LoadConfig path = %q, want %q", path, configPath)
			}
			return &configdomain.Config{LogFile: logFile}, nil
		},
		// An invalid executable is deliberate: the live-pid guard must return
		// before any child-process launch is attempted.
		Executable: filepath.Join(dir, "must-not-execute"),
	}, serve.Args{Config: configPath})
	if err == nil {
		t.Fatal("Daemonize returned nil, want already-running error")
	}
	wantPID := os.Getpid()
	if !strings.Contains(err.Error(), "already running") || !strings.Contains(err.Error(), fmt.Sprintf("pid=%d", wantPID)) {
		t.Fatalf("Daemonize error = %q, want already-running error with pid %d", err, wantPID)
	}
	if loadCalls != 1 {
		t.Fatalf("LoadConfig calls = %d, want 1", loadCalls)
	}
	if _, err := os.Stat(logFile); !os.IsNotExist(err) {
		t.Fatalf("log file was opened before live-pid guard: stat err=%v", err)
	}
	got, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read guarded pid file: %v", err)
	}
	if want := fmt.Sprintf("%d\n", wantPID); string(got) != want {
		t.Fatalf("guarded pid file = %q, want %q", got, want)
	}
}

// Regression (double-start window): after the pre-start guard passes, Daemonize
// claims the pid file with an atomic O_EXCL create. The placeholder must be
// removed again when the supervisor fails to spawn — otherwise a leftover file
// naming the exited parent looks like a (stale) daemon and confuses the next
// start/stop.
func TestDaemonizeRemovesPlaceholderOnSpawnFailure(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "model-proxy.log")
	configPath := filepath.Join(dir, "config.yaml")
	pidFile := serve.PidFilePath(logFile)

	// A definitely-dead pid: the pre-start guard cleans the stale file, then
	// the O_EXCL claim takes the now-free path.
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	if err := serve.WritePidFile(pidFile, dead.ProcessState.Pid()); err != nil {
		t.Fatal(err)
	}

	err := serve.Daemonize(serve.DaemonEnv{
		LoadConfig: func(path string) (*configdomain.Config, error) {
			if path != configPath {
				t.Fatalf("LoadConfig path = %q, want %q", path, configPath)
			}
			return &configdomain.Config{LogFile: logFile}, nil
		},
		// Deliberately invalid executable: the claim must happen, then the
		// spawn must fail and the placeholder be rolled back.
		Executable: filepath.Join(dir, "must-not-execute"),
	}, serve.Args{Config: configPath})
	if err == nil || !strings.Contains(err.Error(), "start supervisor") {
		t.Fatalf("Daemonize err = %v, want start-supervisor failure", err)
	}
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Fatalf("pid file placeholder survived a failed spawn: %v", err)
	}
	if _, err := os.Stat(logFile); err != nil {
		t.Fatalf("log file not opened after claim: %v", err)
	}
}

// The O_EXCL claim must fail loudly when the pid path is unusable for a reason
// other than a live daemon (here: the path is a directory) — silent proceed
// would resurrect the double-start window the claim exists to close.
func TestDaemonizeClaimFailsOnUnusablePidPath(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "model-proxy.log")
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.Mkdir(serve.PidFilePath(logFile), 0o755); err != nil {
		t.Fatal(err)
	}

	err := serve.Daemonize(serve.DaemonEnv{
		LoadConfig: func(path string) (*configdomain.Config, error) {
			return &configdomain.Config{LogFile: logFile}, nil
		},
		Executable: filepath.Join(dir, "must-not-execute"),
	}, serve.Args{Config: configPath})
	if err == nil || !strings.Contains(err.Error(), "claim pid file") {
		t.Fatalf("Daemonize err = %v, want claim-pid-file failure", err)
	}
	if _, err := os.Stat(logFile); !os.IsNotExist(err) {
		t.Fatalf("log file opened despite claim failure: %v", err)
	}
}
