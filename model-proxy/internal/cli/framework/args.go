// Package framework owns CLI-wide arg parsing helpers shared by every command
// wrapper: --config resolution and positional extraction. Environment reads
// (HOME) happen per call so tests can isolate them.
package framework

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	configdomain "model-proxy/internal/config"
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

// ProviderNames returns sorted config provider names for error messages.
func ProviderNames(cfg *configdomain.Config) string {
	names := make([]string, 0, len(cfg.Providers))
	for n := range cfg.Providers {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// Mask redacts a credential/id for display: short secrets fully masked,
// longer ones show first 2 + … + last 2. Never log raw secrets.
func Mask(s string) string {
	if s == "" {
		return "(empty)"
	}
	const minReveal = 8
	if len(s) < minReveal {
		return "****"
	}
	return s[:2] + "…" + s[len(s)-2:]
}

// Plural returns sing for n==1 else plur.
func Plural(n int, sing, plur string) string {
	if n == 1 {
		return sing
	}
	return plur
}

// ReadFile/WriteFile are thin os wrappers kept for the remaining CLI callers.
func ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

func WriteFile(path string, data []byte, mode os.FileMode) error {
	return os.WriteFile(path, data, mode)
}

// RuntimeOS returns runtime.GOOS (browser-launch dispatch).
func RuntimeOS() string { return runtime.GOOS }

// RunCmd starts a process without waiting (browser launchers, daemon spawn).
func RunCmd(name string, args ...string) error {
	return exec.Command(name, args...).Start()
}

// EnvOrEmpty returns os.Getenv (named for call-site readability).
func EnvOrEmpty(k string) string { return os.Getenv(k) }

// HomeDir resolves the user home directory (credential files live under
// ~/.model-proxy).
func HomeDir() string {
	h, _ := os.UserHomeDir()
	return h
}

// AuthFilePath returns the credential file path for a provider name
// (<home>/.model-proxy/<name>_<suffix>.json).
func AuthFilePath(providerName, suffix string) string {
	return filepath.Join(HomeDir(), ".model-proxy", providerName+"_"+suffix+".json")
}
