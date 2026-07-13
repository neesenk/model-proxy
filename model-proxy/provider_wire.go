package main

import (
	"os"
	"path/filepath"
)

// ---- Provider callback wrappers ----
// Login/Logout wrappers for the provider callback interface (LoginFn/LogoutFn),
// kept until Phase 5 moves login/logout into the provider structs. The Usage
// display moved to the provider's Usage() method in Phase 3, so the show*UsageData
// wrappers are gone.

func runCodexLogin(cfg *Config) error {
	cmdCodexLogin([]string{})
	return nil
}

func clearCodexAuth(cfg *Config) error {
	path := authFilePath("codex", "oauth_auth")
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func clearApiKey(providerName string) error {
	path := filepath.Join(homeDir(), ".model-proxy", providerName+"_apikey.json")
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// runVolcengineLoginErr wraps runVolcengineLogin (which prompts for API key +
// AK/SK) for the provider callback interface.
func runVolcengineLoginErr(cfg *Config, provName string, prov Provider) error {
	return runVolcengineLogin(cfg, provName, prov)
}

// runApiKeyLoginErr wraps runApiKeyLogin (now returns error) for the provider
// callback interface.
func runApiKeyLoginErr(cfg *Config, provName string, prov Provider) error {
	return runApiKeyLogin(cfg, provName, prov)
}
