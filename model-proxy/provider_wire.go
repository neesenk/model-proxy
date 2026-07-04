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

// runVolcengineLoginErr wraps runVolcengineLogin (which prompts for API key +
// AK/SK) for the provider callback interface.
func runVolcengineLoginErr(cfg *Config, provName string, prov Provider) error {
	return runVolcengineLogin(cfg, provName, prov)
}

func showZhipuUsageData(cfg *Config, providerName string, prov Provider) (any, error) {
	showGenericUsage(cfg, providerName, prov)
	return nil, nil
}

func showDeepseekUsageData(cfg *Config, providerName string, prov Provider) (any, error) {
	showDeepseekUsage(cfg, providerName, prov)
	return nil, nil
}

// showVolcengineUsageData shows the Agent Plan state (configured models + a note
// that GetAFPUsage needs AK/SK + V4 signing). See showVolcengineUsage.
func showVolcengineUsageData(cfg *Config, providerName string, prov Provider) (any, error) {
	showVolcengineUsage(cfg, providerName, prov)
	return nil, nil
}

// runApiKeyLoginErr wraps runApiKeyLogin (now returns error) for the provider
// callback interface.
func runApiKeyLoginErr(cfg *Config, provName string, prov Provider) error {
	return runApiKeyLogin(cfg, provName, prov)
}
