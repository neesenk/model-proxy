package accounts

import (
	"os"
	"path/filepath"
)

// HomeDir resolves the user home directory (credential files live under
// ~/.model-proxy). Centralizing it here keeps every account store on the same
// seam so tests can isolate with t.Setenv("HOME", ...).
func HomeDir() string {
	h, _ := os.UserHomeDir()
	return h
}

// AuthFilePath returns the credential file path for a provider name
// (<home>/.model-proxy/<name>_<suffix>.json).
func AuthFilePath(providerName, suffix string) string {
	return filepath.Join(HomeDir(), ".model-proxy", providerName+"_"+suffix+".json")
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
