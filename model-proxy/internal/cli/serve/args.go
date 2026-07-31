// Package serve owns the serve/daemon process helpers: flag parsing, log/pid
// file paths, and the daemon liveness probe. The supervisor/worker lifecycle
// stays with the application composition root.
package serve

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
