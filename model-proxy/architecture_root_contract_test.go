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

	t.Run("web.go depends only on explicit Proxy capabilities", func(t *testing.T) {
		f, fset := parseGoFile(t, "web.go")
		fields := namedStructFields(t, f, "webServer")
		if got := simpleTypeName(fields["reads"]); got != "proxyReadView" {
			t.Errorf("webServer.reads type = %q, want proxyReadView", got)
		}
		if got := simpleTypeName(fields["admin"]); got != "proxyAdminCommands" {
			t.Errorf("webServer.admin type = %q, want proxyAdminCommands", got)
		}
		if got := simpleTypeName(fields["tasks"]); got != "*webTaskOwner" {
			t.Errorf("webServer.tasks type = %q, want *webTaskOwner", got)
		}
		for name, typ := range fields {
			if typeContainsIdent(typ, "Proxy") {
				t.Errorf("webServer.%s must not retain *Proxy", name)
			}
		}
		for _, v := range directSelectorSites(f, fset, "w", "p") {
			t.Errorf("web.go retains forbidden direct Proxy access: %s", v)
		}

		allowed := map[string]map[string]bool{
			"reads": {
				"agentStats": true, "analytics": true, "config": true,
				"dashboard": true, "fusion": true, "logFile": true,
				"pins": true, "pricing": true, "providerConfig": true,
				"providerConfigs": true, "requestLogDirectory": true,
				"stats": true, "tokenUsage": true,
			},
			"admin": {
				"accountProbe": true, "clearPin": true, "refreshQuota": true,
				"reload": true, "resetHealthAndPersist": true,
				"resetStats": true, "setPin": true,
			},
		}
		for _, v := range unexpectedPortAccesses(f, fset, "w", allowed) {
			t.Errorf("web.go uses a capability outside the allowlist: %s", v)
		}
		if got := goStatementCount(f); got != 0 {
			t.Errorf("web.go starts %d bare goroutine(s); use webTaskOwner.run", got)
		}
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

	t.Run("fusion.go reuses targetPlan helpers instead of duplicating them", func(t *testing.T) {
		f, fset := parseGoFile(t, "fusion.go")
		for _, spec := range f.Imports {
			if strings.Trim(spec.Path.Value, `"`) == "model-proxy/internal/protocol" {
				t.Error("fusion.go must not import internal/protocol directly; use targetPlan adapters")
			}
		}
		forbiddenCalls := map[string]bool{
			"providerConfig": true, "resolvedBackendProto": true,
			"convertRequestFor": true, "catalogSnapshot": true,
		}
		for _, v := range forbiddenCallSites(f, fset, forbiddenCalls, nil) {
			t.Errorf("fusion.go bypasses targetPlan: %s", v)
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
