package main

import (
	"os"

	cliframework "model-proxy/internal/cli/framework"
)

// File/env/process helpers delegate to internal/cli/framework (single owner);
// aliases keep the remaining root callers compiling during migration.
func readFile(path string) ([]byte, error) { return cliframework.ReadFile(path) }
func envOrEmpty(k string) string           { return cliframework.EnvOrEmpty(k) }
func writeFile(path string, data []byte, mode os.FileMode) error {
	return cliframework.WriteFile(path, data, mode)
}
func runtimeOS() string                        { return cliframework.RuntimeOS() }
func runCmd(name string, args ...string) error { return cliframework.RunCmd(name, args...) }
func mask(s string) string                     { return cliframework.Mask(s) }

func homeDir() string { return cliframework.HomeDir() }

// authFilePath returns the credential file path for a provider name.
// OAuth providers use <name>_oauth_auth.json; apikey providers use <name>_apikey.json.
func authFilePath(providerName, suffix string) string {
	return cliframework.AuthFilePath(providerName, suffix)
}
