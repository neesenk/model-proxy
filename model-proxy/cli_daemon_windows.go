//go:build windows

package main

import (
	"syscall"

	cliserve "model-proxy/internal/cli/serve"
)

// sysProcAttrDetach delegates to cliserve.SysProcAttrDetach (no-op on Windows).
func sysProcAttrDetach() *syscall.SysProcAttr { return cliserve.SysProcAttrDetach() }
