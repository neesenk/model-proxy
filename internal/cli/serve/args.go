// Package serve owns the serve/daemon process helpers: flag parsing, log/pid
// file paths, and the daemon liveness probe. The supervisor/worker lifecycle
// stays with the application composition root.
package serve

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	configdomain "model-proxy/internal/config"
)

// Args holds parsed `serve` flags.
type Args struct {
	Config  string
	LogFile string // --log-file override
}

// ParseArgs scans `serve` flags. config resolution mirrors the CLI-wide
// --config scanning (last occurrence wins is not needed: first wins, matching
// the root helper's behavior for duplicated flags).
func ParseArgs(args []string) Args {
	sa := Args{Config: "config.yaml"}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--config" || a == "-config":
			if i+1 < len(args) {
				sa.Config = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "--config="):
			sa.Config = strings.TrimPrefix(a, "--config=")
		case strings.HasPrefix(a, "-config="):
			sa.Config = strings.TrimPrefix(a, "-config=")
		case a == "--log-file":
			if i+1 < len(args) {
				sa.LogFile = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "--log-file="):
			sa.LogFile = strings.TrimPrefix(a, "--log-file=")
		}
	}
	return sa
}

// ResolveLogFile picks the log file path: --log-file flag > config log_file >
// default (the OS temp dir — runtime artifacts belong there, not under the
// config dir). Returns "" only if the temp dir can't be resolved.
func ResolveLogFile(sa Args, cfg *configdomain.Config) string {
	if sa.LogFile != "" {
		return configdomain.ExpandPath(sa.LogFile)
	}
	if cfg != nil && cfg.LogFile != "" {
		return cfg.LogFile
	}
	return filepath.Join(os.TempDir(), "model-proxy.log")
}

// OpenLogFile opens (creating parent dirs) a log file for append.
func OpenLogFile(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}

// PidFilePath derives the pid file path from the log file path.
func PidFilePath(logFile string) string {
	if strings.HasSuffix(logFile, ".log") {
		return strings.TrimSuffix(logFile, ".log") + ".pid"
	}
	return logFile + ".pid"
}

// WritePidFile writes pid as a single line.
func WritePidFile(path string, pid int) error {
	return os.WriteFile(path, []byte(fmt.Sprintf("%d\n", pid)), 0o644)
}

// MaybeReloadDaemon SIGHUPs a running daemon after a config/credential write
// so the change takes effect without a manual `serve reload`. Best-effort:
// silent when no daemon runs, the pid file is stale, or the signal fails.
func MaybeReloadDaemon(cfg *configdomain.Config) {
	logFile := ResolveLogFile(Args{}, cfg)
	pidPath := PidFilePath(logFile)
	pidStr, err := os.ReadFile(pidPath)
	if err != nil {
		return // no pid file → no daemon running
	}
	var pid int
	for _, c := range pidStr {
		if c < '0' || c > '9' {
			break
		}
		pid = pid*10 + int(c-'0')
	}
	if pid <= 0 {
		return
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		// Stale pid file — clean it up so the next start isn't confused.
		os.Remove(pidPath)
		return
	}
	_ = proc.Signal(syscall.SIGHUP)
}

// ReadLivePid reads the pid file derived from logFile and returns the pid of
// the live process it names. It returns 0 when no pid file exists, the file is
// unreadable/invalid, or the process is gone (a stale pid file is removed in
// that last case). Shared by daemonize's pre-start guard and the stop/reload
// commands so the "is a daemon already running?" check is one implementation.
func ReadLivePid(logFile string) int {
	pidPath := PidFilePath(logFile)
	pidStr, err := os.ReadFile(pidPath)
	if err != nil {
		return 0
	}
	var pid int
	for _, c := range pidStr {
		if c < '0' || c > '9' {
			break
		}
		pid = pid*10 + int(c-'0')
	}
	if pid <= 0 {
		return 0
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return 0
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		// Stale pid file — clean it up so the next start isn't confused.
		os.Remove(pidPath)
		return 0
	}
	return pid
}
