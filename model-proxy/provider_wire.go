package main

// ---- Provider callback wrappers ----
// Login-flow wrappers for the LoginFn callback (cmdLogin calls the run* flows
// directly; these adapt them to the cfg.LoginFn signature). Logout is
// provider-owned (file removal) since Phase 5, so the clear* wrappers are gone.

func runCodexLogin(cfg *Config) error {
	cmdCodexLogin([]string{})
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
