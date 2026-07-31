package main

import (
	"fmt"
	"log"
	cliframework "model-proxy/internal/cli/framework"
	"os"

	clilogin "model-proxy/internal/cli/login"
)

// cmdLogin is the process-level wrapper: config loading and arg parsing stay
// here; provider login flows live in internal/cli/login.
func cmdLogin(args []string) {
	cfg, err := LoadConfig(cliframework.ConfigPath(args))
	if err != nil {
		log.Fatal(err)
	}
	provName := cliframework.Positional(args)
	if provName == "" {
		fmt.Println("usage: model-proxy login <provider> [--label <name>] [--replace]")
		fmt.Println("available providers:")
		for name, p := range cfg.Providers {
			fmt.Printf("  %s (provider=%s)\n", name, p.Provider)
		}
		return
	}
	prov, ok := cfg.Providers[provName]
	if !ok {
		log.Fatalf("unknown provider %q; available: %s", provName, providerNames(cfg))
	}
	label := cliframework.FlagStringValue(args, "--label")
	replace := cliframework.HasFlagValue(args, "--replace")

	switch prov.Provider {
	case "aqp":
		if err := clilogin.RunLogin(cfg, provName); err != nil {
			log.Fatalf("login failed: %v", err)
		}
	case "codex":
		cmdCodexLogin(provName)
	case "zcode":
		fmt.Println("Opening BigModel login to fetch a Coding Plan API key…")
		if err := clilogin.OpenBrowser("https://bigmodel.cn/login"); err != nil {
			fmt.Fprintf(os.Stderr, "(could not open browser: %v — open https://bigmodel.cn/login manually)\n", err)
		}
		if err := clilogin.RunApiKeyLoginWithInput(cfg, provName, prov, "", label, replace); err != nil {
			log.Fatalf("login failed: %v", err)
		}
	default:
		var err error
		if prov.Provider == "volcengine" {
			err = runVolcengineLoginWithInput(cfg, provName, prov, "", "", "", label, replace)
		} else {
			err = clilogin.RunApiKeyLoginWithInput(cfg, provName, prov, "", label, replace)
		}
		if err != nil {
			log.Fatalf("login failed: %v", err)
		}
	}
	// After ANY successful login, signal a running serve to hot-reload.
	maybeReloadDaemon(args)
}

// Aliases for the remaining root callers (web layer, tests) while login
// migrates fully into internal/cli/login.
type LoopbackServer = clilogin.LoopbackServer

var (
	withNextCallback   = clilogin.WithNextCallback
	waitForLoginSignal = clilogin.WaitForLoginSignal
	openBrowser        = clilogin.OpenBrowser
)

func NewLoopbackServer() (*LoopbackServer, error) { return clilogin.NewLoopbackServer() }

func runApiKeyLogin(cfg *Config, provName string, prov Provider) error {
	return clilogin.RunApiKeyLogin(cfg, provName, prov)
}

func runApiKeyLoginWithInput(cfg *Config, provName string, prov Provider, in, label string, replace bool) error {
	return clilogin.RunApiKeyLoginWithInput(cfg, provName, prov, in, label, replace)
}

func apiKeyValidationURL(prov Provider) string { return clilogin.ApiKeyValidationURL(prov) }

func addApikeyAccount(cfg *Config, name string, prov Provider, cred accountCred, label string, replace bool) (string, error) {
	return clilogin.AddApikeyAccount(cfg, name, prov, cred, label, replace)
}

func removeApikeyAccount(name, providerID, id string) error {
	return clilogin.RemoveApikeyAccount(name, providerID, id)
}

func runLogin(cfg *Config, provName string) error { return clilogin.RunLogin(cfg, provName) }
