package main

import (
	"os"
	"path/filepath"
)

// homeDir resolves the user home directory (credential files live under
// ~/.model-proxy). accounts/proxy layers use it directly.
func homeDir() string {
	h, _ := os.UserHomeDir()
	return h
}

// authFilePath returns the credential file path for a provider name
// (<home>/.model-proxy/<name>_<suffix>.json).
func authFilePath(providerName, suffix string) string {
	return filepath.Join(homeDir(), ".model-proxy", providerName+"_"+suffix+".json")
}
