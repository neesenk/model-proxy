package main

import (
	cliserve "model-proxy/internal/cli/serve"
)

// serveArgs/parseServeArgs delegate to internal/cli/serve; aliases keep root
// call sites compiling while daemon/serve orchestration migrates.
type serveArgs = cliserve.Args

func parseServeArgs(args []string) serveArgs { return cliserve.ParseArgs(args) }
