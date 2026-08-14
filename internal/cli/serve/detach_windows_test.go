//go:build windows

package serve_test

import (
	"testing"

	serve "model-proxy/internal/cli/serve"
)

func TestSysProcAttrDetachWindows(t *testing.T) {
	if got := serve.SysProcAttrDetach(); got != nil {
		t.Fatalf("SysProcAttrDetach() = %#v, want nil on Windows", got)
	}
}
