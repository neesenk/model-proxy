//go:build !windows

package main

import (
	"syscall"

	cliserve "model-proxy/internal/cli/serve"
)

// sysProcAttrDetach delegates to cliserve.SysProcAttrDetach.
func sysProcAttrDetach() *syscall.SysProcAttr { return cliserve.SysProcAttrDetach() }
