package main

import clilogin "model-proxy/internal/cli/login"

// cmdLogin delegates to internal/cli/login.CmdLogin.
func cmdLogin(args []string) {
	clilogin.CmdLogin(args)
}
