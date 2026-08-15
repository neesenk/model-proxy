// Package serve owns the serve/daemon process helpers: flag parsing, log/pid
// file paths, and the daemon liveness probe. The supervisor/worker lifecycle
// stays with the application composition root.
package serve

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
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

// ClaimForegroundPidFile claims the pid file derived from logFile for the
// calling (foreground serve) process: it writes the caller's pid and returns
// the path when the file is free, and returns "" when a LIVE process already
// owns it (a running daemon, typically) — overwriting would steal
// `serve stop`/`reload` from that process, and removing the file on exit
// would orphan it. A stale pid file is cleaned up by ReadLivePid and claimed.
func ClaimForegroundPidFile(logFile string) string {
	if live := ReadLivePid(logFile); live != 0 && live != os.Getpid() {
		return ""
	}
	path := PidFilePath(logFile)
	if err := WritePidFile(path, os.Getpid()); err != nil {
		return ""
	}
	return path
}

// ReleasePidFileIfOwned removes the pid file only while it still names the
// calling process: a daemon may have (re)started and replaced it while the
// foreground serve ran, and deleting that file would orphan the daemon.
func ReleasePidFileIfOwned(path string) {
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid != os.Getpid() {
		return
	}
	os.Remove(path)
}

// parsePidFileContents extracts the leading decimal pid from raw pid-file
// bytes, stopping at the first non-digit (the trailing newline and anything
// after it are ignored). It is the single parser shared by every pid-file
// reader in this package. It returns 0 when the content has no leading digits
// and when the digits overflow int: a hand-rolled accumulate loop would wrap
// to an arbitrary number that could name a live, foreign process, so an
// over-long number is an invalid pid, not a wrapped one.
func parsePidFileContents(raw []byte) int {
	pid := 0
	for _, c := range raw {
		if c < '0' || c > '9' {
			break
		}
		d := int(c - '0')
		if pid > (math.MaxInt-d)/10 {
			return 0 // overflow guard
		}
		pid = pid*10 + d
	}
	return pid
}

// MaybeReloadDaemon SIGHUPs a running daemon after a config/credential write
// so the change takes effect without a manual `serve reload`. Best-effort:
// silent when no daemon runs, the pid file is stale, or the signal fails.
// It takes the caller's FULL command args so a daemon started with
// `--log-file` is found at the pid file that flag derives — deriving from
// Args{} would silently miss it (same resolution rule as CmdReload).
func MaybeReloadDaemon(args []string, cfg *configdomain.Config) {
	logFile := ResolveLogFile(ParseArgs(args), cfg)
	pidPath := PidFilePath(logFile)
	pidStr, err := os.ReadFile(pidPath)
	if err != nil {
		return // no pid file → no daemon running
	}
	pid := parsePidFileContents(pidStr)
	if pid <= 0 {
		return
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		// EPERM means the process EXISTS but is not signalable by us — treat
		// it as live and skip the reload signal (it would fail too).
		if errors.Is(err, os.ErrPermission) {
			return
		}
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
	pid := parsePidFileContents(pidStr)
	if pid <= 0 {
		return 0
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return 0
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		// EPERM = the process exists but is not signalable by us: it is alive
		// (e.g. a daemon owned by another user), NOT stale.
		if errors.Is(err, os.ErrPermission) {
			return pid
		}
		// Stale pid file — clean it up so the next start isn't confused.
		os.Remove(pidPath)
		return 0
	}
	return pid
}
