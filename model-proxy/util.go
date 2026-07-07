package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

func readFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

func envOrEmpty(k string) string { return os.Getenv(k) }

func writeFile(path string, data []byte, mode os.FileMode) error {
	return os.WriteFile(path, data, mode)
}

func runtimeOS() string { return runtime.GOOS }

func runCmd(name string, args ...string) error {
	c := exec.Command(name, args...)
	return c.Start()
}

// mask redacts a secret for logging. Keeps the first 2 and last 2 chars (so a
// value is still identifiable in debug), replacing the middle with "…". Short
// or empty values become "****" so they never leak verbatim. Used for cookies,
// API keys, tokens — never log raw secrets.
func mask(s string) string {
	if s == "" {
		return "(empty)"
	}
	// Short secrets: don't reveal even partial — full mask.
	const minReveal = 8
	if len(s) < minReveal {
		return "****"
	}
	return s[:2] + "…" + s[len(s)-2:]
}

func homeDir() string {
	h, _ := os.UserHomeDir()
	return h
}

// authFilePath returns the credential file path for a provider name.
// OAuth providers use <name>_oauth_auth.json; apikey providers use <name>_apikey.json.
func authFilePath(providerName, suffix string) string {
	return filepath.Join(homeDir(), ".model-proxy", providerName+"_"+suffix+".json")
}

// flagStringValue scans args for a `--name value` or `--name=value` flag and
// returns its value ("" if absent, or if the flag is the last arg with no
// value). Used by cmdLogin for --label. Mirrors parseServeArgs' scanning style.
func flagStringValue(args []string, flag string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == flag {
			if i+1 < len(args) {
				return args[i+1]
			}
			return ""
		}
		if strings.HasPrefix(a, flag+"=") {
			return strings.TrimPrefix(a, flag+"=")
		}
	}
	return ""
}

// hasFlagValue reports whether the boolean flag is present anywhere in args.
// Used by cmdLogin for --replace. Unlike flagStringValue, the flag need not be
// at a specific position; presence is enough.
func hasFlagValue(args []string, flag string) bool {
	for _, a := range args {
		if a == flag || strings.HasPrefix(a, flag+"=") {
			return true
		}
	}
	return false
}
