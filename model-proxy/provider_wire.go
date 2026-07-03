package main

import (
	"os"
	"path/filepath"
)

// ---- Provider callback wrappers ----
// These wrap existing main-package functions so provider.Provider can call them
// via the callback interface without importing package main.

func showCompassUsageData(cfg *Config) (any, error) {
	showCompassUsage(cfg)
	return nil, nil
}

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

func showCodexUsageData(cfg *Config, prov Provider) (any, error) {
	showCodexUsage(cfg, prov)
	return nil, nil
}

func clearApiKey(providerName string) error {
	path := filepath.Join(homeDir(), ".model-proxy", providerName+"_apikey.json")
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func showZhipuUsageData(cfg *Config, providerName string, prov Provider) (any, error) {
	showGenericUsage(cfg, providerName, prov)
	return nil, nil
}

// runApiKeyLoginErr wraps runApiKeyLogin (which calls log.Fatal) to return error.
func runApiKeyLoginErr(cfg *Config, provName string, prov Provider) error {
	// runApiKeyLogin calls log.Fatal on error, so it never returns.
	// We recover from the fatal to convert it to an error.
	defer func() {}()
	runApiKeyLogin(cfg, provName, prov)
	return nil
}
