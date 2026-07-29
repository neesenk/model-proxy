package main

import (
	"go/ast"
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

	t.Run("process entry leaves Proxy background tasks to proxyLifecycle", func(t *testing.T) {
		forbiddenCalls := map[string]bool{
			"initStats": true, "initRequestLog": true, "statsFlushLoop": true,
		}
		// Chained internal components: <x>.reqLog.Run(), <x>.flusher.flush().
		forbiddenChains := [][2]string{
			{"reqLog", "Run"}, {"reqLog", "Shutdown"}, {"flusher", "flush"},
		}
		for _, path := range []string{"daemon.go", "cli_serve.go", "cli_daemon.go"} {
			file, fileSet := parseGoFile(t, path)
			for _, violation := range forbiddenCallSites(file, fileSet, forbiddenCalls, forbiddenChains) {
				t.Errorf("%s bypasses proxyLifecycle: %s", path, violation)
			}
		}

		serve, _ := parseGoFile(t, "cli_serve.go")
		if got := namedCallCount(serve, "startRuntimeServices"); got != 1 {
			t.Errorf("cli_serve.go startRuntimeServices calls = %d, want exactly 1", got)
		}
		if got := callCountOnIdent(serve, "p", "startRuntimeServices"); got != 1 {
			t.Errorf("cli_serve.go p.startRuntimeServices calls = %d, want exactly 1", got)
		}
		if got := selectorCountNamed(serve, "startRuntimeServices"); got != 1 {
			t.Errorf("cli_serve.go startRuntimeServices selector uses = %d, want exactly 1 direct call", got)
		}
		if got := selectorCountOnIdent(serve, "p", "startRuntimeServices"); got != 1 {
			t.Errorf("cli_serve.go p.startRuntimeServices selector uses = %d, want exactly 1", got)
		}
		if got := namedCallCount(serve, "Close"); got != 1 {
			t.Errorf("cli_serve.go direct Close calls = %d, want exactly 1 listener-error cleanup", got)
		}
		if got := callCountOnIdent(serve, "p", "Close"); got != 1 {
			t.Errorf("cli_serve.go direct p.Close calls = %d, want exactly 1", got)
		}
		if got := selectorCountNamed(serve, "Close"); got != 2 {
			t.Errorf("cli_serve.go Close selector uses = %d, want direct cleanup plus final callback", got)
		}
		if got := selectorCountOnIdent(serve, "p", "Close"); got != 2 {
			t.Errorf("cli_serve.go p.Close selector uses = %d, want exactly 2", got)
		}
		if got := callCountWithLastSelector(serve, "serveHTTPUntilShutdown", "p", "Close"); got != 1 {
			t.Errorf("serveHTTPUntilShutdown(..., p.Close) calls = %d, want exactly 1 final-flush callback", got)
		}
	})

}

func callCountOnIdent(file *ast.File, receiver, method string) int {
	count := 0
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != method {
			return true
		}
		identifier, ok := selector.X.(*ast.Ident)
		if ok && identifier.Name == receiver {
			count++
		}
		return true
	})
	return count
}

func callCountWithLastSelector(file *ast.File, function, receiver, method string) int {
	count := 0
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		identifier, ok := call.Fun.(*ast.Ident)
		if !ok || identifier.Name != function {
			return true
		}
		selector, ok := call.Args[len(call.Args)-1].(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != method {
			return true
		}
		base, ok := selector.X.(*ast.Ident)
		if ok && base.Name == receiver {
			count++
		}
		return true
	})
	return count
}

func selectorCountNamed(file *ast.File, method string) int {
	count := 0
	ast.Inspect(file, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if ok && selector.Sel.Name == method {
			count++
		}
		return true
	})
	return count
}

func selectorCountOnIdent(file *ast.File, receiver, method string) int {
	count := 0
	ast.Inspect(file, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != method {
			return true
		}
		base, ok := selector.X.(*ast.Ident)
		if ok && base.Name == receiver {
			count++
		}
		return true
	})
	return count
}
