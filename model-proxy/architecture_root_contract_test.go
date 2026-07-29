package main

import (
	"strings"
	"testing"
)

// TestArchitectureRootBoundaries keeps composition-root ownership narrow:
// Web uses explicit capabilities, account probes capture one generation, Fusion
// shares target planning, and daemon background work enters proxyLifecycle.
func TestArchitectureRootBoundaries(t *testing.T) {
	rootPackage, rootSet := parseGoPackage(t, ".")
	_ = rootSet

	t.Run("root semantic package view spans source files", func(t *testing.T) {
		if fields := namedStructFields(t, rootPackage, "Proxy"); len(fields) == 0 {
			t.Fatal("root package view did not find Proxy fields")
		}
		// Proxy is currently declared in proxy.go while this method lives in
		// proxy_lifecycle.go. Finding both proves semantic guards are no longer
		// coupled to one physical composition-root file.
		_ = namedMethod(t, rootPackage, "Proxy", "startRuntimeServices")
	})

	t.Run("account probe captures one runtime generation", func(t *testing.T) {
		admin, _ := parseGoFile(t, "proxy_admin_commands.go")
		if got := methodCallCount(admin, "accountProbe", "snapshotRuntime"); got != 1 {
			t.Errorf("proxyAdminCommands.accountProbe snapshotRuntime calls = %d, want exactly 1", got)
		}
		readView, _ := parseGoFile(t, "proxy_read_view.go")
		if methodDeclared(readView, "runtimeProvider") {
			t.Error("proxyReadView.runtimeProvider must not exist; account probes use one runtimeSnapshot")
		}
	})

	t.Run("fusion.go reuses targetexec Plan helpers instead of duplicating them", func(t *testing.T) {
		f, fset := parseGoFile(t, "fusion.go")
		for _, spec := range f.Imports {
			if strings.Trim(spec.Path.Value, `"`) == "model-proxy/internal/protocol" {
				t.Error("fusion.go must not import internal/protocol directly; use targetexec.Plan")
			}
		}
		forbiddenCalls := map[string]bool{
			"providerConfig": true, "resolvedBackendProto": true,
			"convertRequestFor": true, "catalogSnapshot": true,
		}
		for _, v := range forbiddenCallSites(f, fset, forbiddenCalls, nil) {
			t.Errorf("fusion.go bypasses targetexec.Plan: %s", v)
		}
	})

	t.Run("daemon.go leaves Proxy background tasks to proxyLifecycle", func(t *testing.T) {
		f, fset := parseGoFile(t, "daemon.go")
		forbiddenCalls := map[string]bool{
			"initStats": true, "initRequestLog": true, "statsFlushLoop": true,
		}
		// Chained internal components: <x>.reqLog.Run(), <x>.flusher.flush().
		forbiddenChains := [][2]string{
			{"reqLog", "Run"}, {"reqLog", "Shutdown"}, {"flusher", "flush"},
		}
		for _, v := range forbiddenCallSites(f, fset, forbiddenCalls, forbiddenChains) {
			t.Errorf("daemon.go bypasses proxyLifecycle: %s", v)
		}
	})

}
