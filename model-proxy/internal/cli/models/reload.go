package models

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	configdomain "model-proxy/internal/config"
)

// configPath scans args for --config (same shape as the root CLI helper).
func configPath(args []string) string {
	for i, a := range args {
		if (a == "--config" || a == "-config") && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(a, "--config=") {
			return strings.TrimPrefix(a, "--config=")
		}
	}
	return "config.yaml"
}

// pidFilePath mirrors the daemon's pid naming (<log>.pid).
func pidFilePath(logFile string) string {
	if strings.HasSuffix(logFile, ".log") {
		return strings.TrimSuffix(logFile, ".log") + ".pid"
	}
	return logFile + ".pid"
}

// maybeReloadDaemon SIGHUPs a running daemon after a config write so the new
// model set takes effect without a manual `serve reload`. Best-effort: silent
// when no daemon runs, the pid file is stale, or the signal can't be delivered.
func maybeReloadDaemon(cfg *configdomain.Config) {
	logFile := cfg.LogFile
	if logFile == "" {
		logFile = filepath.Join(os.TempDir(), "model-proxy.log")
	}
	pidPath := pidFilePath(logFile)
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
	if err := proc.Signal(syscall.SIGHUP); err == nil {
		fmt.Println("  " + cGray(fmt.Sprintf("(signaled serve to reload: pid=%d)", pid)))
	}
}
