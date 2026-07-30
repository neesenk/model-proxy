package main

import (
	clilogin "model-proxy/internal/cli/login"
)

// AqpClient aliases keep root login/web call sites compiling while login
// migrates into internal/cli/login.
type AqpClient = clilogin.AqpClient

func newAqpClient(storePath string) *AqpClient {
	return clilogin.NewAqpClient(storePath)
}

const loginCompletePath = clilogin.LoginCompletePath
