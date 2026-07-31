package main

import (
	"model-proxy/provider"
)

// Log-stream color delegates to provider/display (single owner for terminal
// display rules). Aliases keep runtime-log call sites compiling.
func cl(code, s string) string { return provider.LogColor(code, s) }

func statusColor(status int, s string) string { return provider.StatusColor(status, s) }

// Stdout color helpers (delegate to provider/display.go).

// Format helpers (delegate to provider/display.go).
