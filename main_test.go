package main

import (
	"testing"

	appdomain "model-proxy/internal/app"
)

// TestNewApplicationInjectsBuildVersion (A1 regression): the release build
// stamps the root `version` via -ldflags "-X main.version=…", but nothing
// forwarded it to internal/app.Version, so /api/status and `serve status`
// always reported the package default "dev". newApplication must copy the
// build-time value across.
func TestNewApplicationInjectsBuildVersion(t *testing.T) {
	savedRoot := version
	savedApp := appdomain.Version
	t.Cleanup(func() {
		version = savedRoot
		appdomain.Version = savedApp
	})

	version = "9.9.9-test"
	appdomain.Version = "dev"

	newApplication()

	if appdomain.Version != version {
		t.Fatalf("appdomain.Version = %q, want build-time root version %q", appdomain.Version, version)
	}
}
