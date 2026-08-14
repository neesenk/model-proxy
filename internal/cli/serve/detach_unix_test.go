//go:build !windows

package serve_test

import (
	"testing"

	serve "model-proxy/internal/cli/serve"
)

func TestSysProcAttrDetachUnix(t *testing.T) {
	attr := serve.SysProcAttrDetach()
	if attr == nil {
		t.Fatal("SysProcAttrDetach() = nil, want detached process attributes")
	}
	if !attr.Setsid {
		t.Fatal("SysProcAttrDetach().Setsid = false, want true")
	}
}
