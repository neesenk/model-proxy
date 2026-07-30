package main

import (
	clilogin "model-proxy/internal/cli/login"
)

// volcengine login core lives in internal/cli/login; wrappers keep root CLI
// and tests compiling while the remaining login code migrates.
func runVolcengineLoginWithInput(cfg *Config, provName string, prov Provider, inKey, inAK, inSK, label string, replace bool) error {
	return clilogin.RunVolcengineLoginWithInput(cfg, provName, prov, inKey, inAK, inSK, label, replace)
}

func addVolcengineAccount(cfg *Config, name string, prov Provider, cred accountCred, label string, replace bool) (string, error) {
	return clilogin.AddVolcengineAccount(cfg, name, prov, cred, label, replace)
}
