// Package framework owns the CLI-wide helpers shared by every command
// package: --config resolution and positional extraction, process-level
// config loading (LoadCmdConfig), and terminal number formatting (CompactNum,
// Plural). Environment reads (HOME) happen per call so tests can isolate
// them.
package framework

import (
	"model-proxy/internal/accounts"
	"os"
	"path/filepath"
	"strings"
)

// ConfigPath resolves the config file path: --config flag > ~/.model-proxy/
// config.yaml > ./config.yaml. The first existing file wins; if none exists,
// "./config.yaml" is returned so LoadConfig reports a clear "not found".
func ConfigPath(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--config" || a == "-config" {
			if i+1 < len(args) {
				return args[i+1]
			}
		}
		if strings.HasPrefix(a, "--config=") {
			return strings.TrimPrefix(a, "--config=")
		}
	}
	home := ""
	if h, err := os.UserHomeDir(); err == nil {
		home = h
	}
	if home != "" {
		p := filepath.Join(home, ".model-proxy", "config.yaml")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "config.yaml"
}

// Positional returns the first non-flag positional arg (skipping --config and
// its value).
func Positional(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--config" || a == "-config" {
			i++
			continue
		}
		if len(a) > 8 && a[:8] == "--config" {
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		return a
	}
	return ""
}

// FlagStringValue scans args for a `--name value` or `--name=value` flag and
// returns its value ("" if absent).
func FlagStringValue(args []string, flag string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(a, flag+"=") {
			return strings.TrimPrefix(a, flag+"=")
		}
	}
	return ""
}

// HasFlagValue reports whether args contains `flag` (either form).
func HasFlagValue(args []string, flag string) bool {
	for _, a := range args {
		if a == flag || strings.HasPrefix(a, flag+"=") {
			return true
		}
	}
	return false
}

// Plural returns sing for n==1 else plur.
func Plural(n int, sing, plur string) string {
	if n == 1 {
		return sing
	}
	return plur
}

// HomeDir resolves the user home directory (credential files live under
// ~/.model-proxy).
func HomeDir() string {
	return accounts.HomeDir()
}

// AuthFilePath returns the credential file path for a provider name
// (<home>/.model-proxy/<name>_<suffix>.json).
func AuthFilePath(providerName, suffix string) string {
	return accounts.AuthFilePath(providerName, suffix)
}

// PositionalArgs returns every non-flag positional arg in order: --flag value
// pairs and --flag=value forms are skipped (used by pin/unpin/replay to pull
// <route> [<provider>] / <id>).
func PositionalArgs(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--config" {
			i++ // skip its value
			continue
		}
		if strings.HasPrefix(a, "--") {
			if !strings.Contains(a, "=") {
				i++ // skip --flag value
			}
			continue
		}
		out = append(out, a)
	}
	return out
}
