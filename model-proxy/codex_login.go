package main

import (
	clilogin "model-proxy/internal/cli/login"
)

// codex OAuth types and flows live in internal/cli/login; these aliases keep
// web login and tests compiling while the remaining login code migrates.
type codexLoginServerOptions = clilogin.CodexLoginServerOptions

var (
	requestUserCodeContext       = clilogin.RequestUserCodeContext
	pollForTokenContext          = clilogin.PollForTokenContext
	exchangeCodeForTokensContext = clilogin.ExchangeCodeForTokensContext
)

func cmdCodexLogin(provName string) { clilogin.CmdCodexLogin(provName) }

func defaultCodexLoginOptions() *codexLoginServerOptions {
	options := &codexLoginServerOptions{}
	options.Defaults()
	return options
}
