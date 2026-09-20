package archtest

import (
	"go/ast"
	"strings"
	"testing"
)

// TestArchitectureRootBoundaries keeps process and runtime ownership narrow:
// Web uses explicit capabilities, account probes capture one generation, Fusion
// shares target planning, and daemon background work enters proxyLifecycle.
func TestArchitectureRootBoundaries(t *testing.T) {
	rootPackage, rootSet := parseGoPackage(t, "internal/app")
	_ = rootSet

	t.Run("root semantic package view spans source files", func(t *testing.T) {
		if fields := namedStructFields(t, rootPackage, "Proxy"); len(fields) == 0 {
			t.Fatal("root package view did not find Proxy fields")
		}
		// Proxy is currently declared in proxy.go while this method lives in
		// proxy_lifecycle.go. Finding both proves semantic guards are no longer
		// coupled to one physical composition-root file.
		_ = namedMethod(t, rootPackage, "Proxy", "StartRuntimeServices")
	})

	t.Run("account probe captures one runtime generation", func(t *testing.T) {
		commands, _ := parseGoFile(t, "internal/admin/commands.go")
		if got := methodCallCount(commands, "accountProbe", "ProbeRuntime"); got != 1 {
			t.Errorf("admin.Service.accountProbe ProbeRuntime calls = %d, want exactly 1", got)
		}
		read, _ := parseGoFile(t, "internal/admin/read.go")
		if methodDeclared(read, "runtimeProvider") {
			t.Error("admin read side must not grow a per-name runtime provider lookup; account probes use one runtime snapshot")
		}
		ports, _ := parseGoFile(t, "internal/app/web_adapter.go")
		adminPorts := namedMethod(t, ports, "Proxy", "adminPorts")
		if got := namedCallCountInNode(adminPorts.Body, "SnapshotRuntime"); got != 1 {
			t.Errorf("web_adapter.go adminPorts SnapshotRuntime calls = %d, want exactly 1 (the ProbeRuntime port)", got)
		}
	})

	t.Run("model refresh captures one runtime generation", func(t *testing.T) {
		refresh, _ := parseGoFile(t, "internal/admin/models_refresh.go")
		if got := methodCallCount(refresh, "RefreshModels", "ModelRefreshRuntime"); got != 1 {
			t.Errorf("RefreshModels runtime captures = %d, want exactly 1", got)
		}
		for _, method := range []string{"Config", "ProviderImpl", "ProbeHTTPClient"} {
			if got := methodCallCount(refresh, "RefreshModels", method); got != 0 {
				t.Errorf("RefreshModels separately reads %s (%d calls)", method, got)
			}
		}
	})

	t.Run("forward fusion.go reuses targetexec Plan helpers instead of duplicating them", func(t *testing.T) {
		f, fset := parseGoFile(t, "internal/forward/fusion.go")
		for _, spec := range f.Imports {
			if strings.Trim(spec.Path.Value, `"`) == "model-proxy/internal/protocol" {
				t.Error("forward/fusion.go must not import internal/protocol directly; use targetexec.Plan")
			}
		}
		forbiddenCalls := map[string]bool{
			"providerConfig": true, "resolvedBackendProto": true,
			"convertRequestFor": true, "catalogSnapshot": true,
		}
		for _, v := range forbiddenCallSites(f, fset, forbiddenCalls, nil) {
			t.Errorf("forward/fusion.go bypasses targetexec.Plan: %s", v)
		}
	})

	t.Run("application assembly is the only concrete Proxy process wiring", func(t *testing.T) {
		forbiddenCalls := map[string]bool{
			"NewProxy": true, "StartRuntimeServices": true,
			"NewWebServer": true, "initStats": true,
			"initRequestLog": true, "statsFlushLoop": true,
		}
		// Chained internal components: <x>.reqLog.Run(), <x>.flusher.flush().
		forbiddenChains := [][2]string{
			{"reqLog", "Run"}, {"reqLog", "Shutdown"}, {"flusher", "flush"},
		}
		for _, path := range []string{"cli_serve.go"} {
			file, fileSet := parseGoFile(t, path)
			for _, violation := range forbiddenCallSites(file, fileSet, forbiddenCalls, forbiddenChains) {
				t.Errorf("%s bypasses proxyLifecycle: %s", path, violation)
			}
		}

		assembly, _ := parseGoFile(t, "internal/app/runtime.go")
		if got := namedCallCount(assembly, "NewProxy"); got != 1 {
			t.Errorf("internal/app/runtime.go NewProxy calls = %d, want exactly 1", got)
		}
		if got := namedCallCount(assembly, "StartRuntimeServices"); got != 1 {
			t.Errorf("internal/app/runtime.go StartRuntimeServices calls = %d, want exactly 1", got)
		}
		if got := callCountOnIdent(assembly, "proxy", "StartRuntimeServices"); got != 1 {
			t.Errorf("internal/app/runtime.go proxy.StartRuntimeServices calls = %d, want exactly 1", got)
		}
		if got := selectorCountNamed(assembly, "StartRuntimeServices"); got != 1 {
			t.Errorf("internal/app/runtime.go StartRuntimeServices selector uses = %d, want exactly 1 direct call", got)
		}
		if got := selectorCountOnIdent(assembly, "proxy", "StartRuntimeServices"); got != 1 {
			t.Errorf("internal/app/runtime.go proxy.StartRuntimeServices selector uses = %d, want exactly 1", got)
		}
		if got := namedCallCount(assembly, "NewWebServer"); got != 1 {
			t.Errorf("internal/app/runtime.go NewWebServer calls = %d, want exactly 1", got)
		}

		serve, _ := parseGoFile(t, "cli_serve.go")
		if got := namedCallCount(serve, "newApplicationRuntime"); got != 1 {
			t.Errorf("cli_serve.go newApplicationRuntime calls = %d, want exactly 1 concrete assembly hand-off", got)
		}
		if got := selectorCountOnIdent(serve, "runtime", "Close"); got != 2 {
			t.Errorf("cli_serve.go runtime.Close selector uses = %d, want listener-error cleanup plus final callback", got)
		}
		if got := callCountWithLastSelector(serve, "ServeHTTPUntilShutdown", "runtime", "Close"); got != 1 {
			t.Errorf("cliserve.ServeHTTPUntilShutdown(..., runtime.Close) calls = %d, want exactly 1 final-flush callback", got)
		}
	})

	t.Run("Proxy fields are grouped into reload generation and process services", func(t *testing.T) {
		// Proxy's own (non-promoted) fields are exactly the cross-group process
		// state; everything else lives in one of the two embedded groups so the
		// reload swap unit is visible at the type level.
		wantProxyFields := map[string]bool{
			"mu": true, "configGeneration": true,
			"pricingMu": true, "closeOnce": true, "pprofEnabled": true,
			// catalogLoader is the initCatalog seam (production:
			// configdomain.LoadModelsCatalog; tests stub it offline).
			"catalogLoader": true,
		}
		if got := structContractViolations(
			namedStructFields(t, rootPackage, "Proxy"), wantProxyFields, nil,
		); len(got) != 0 {
			t.Errorf("Proxy top-level fields must be exactly the cross-group process state: %v", got)
		}
		embeds := structEmbeddedTypes(t, rootPackage, "Proxy")
		if len(embeds) != 2 || embeds[0] != "generationState" || embeds[1] != "processServices" {
			t.Errorf("Proxy embedded groups = %v, want exactly [generationState processServices]", embeds)
		}

		// generationState is the reload swap unit (proxy_reload.go's locked
		// section is the swap definition); processServices survives reload.
		// New fields must land in the right group, never sprawl onto Proxy.
		wantGeneration := map[string]bool{
			"cfg": true, "providers": true,
			"cache": true, "guardScanner": true,
			"guardPoolSecrets": true, "guardOAuthSecrets": true,
			"secLog": true, "secLogRunning": true,
			"poolIndex": true, "parentOf": true,
			"expandedRoutes": true, "routeKeys": true,
			"derivedRoutes": true, "routeWarnings": true,
			"shadow": true, "adminAuth": true, "apiKeys": true,
		}
		if got := structContractViolations(
			namedStructFields(t, rootPackage, "generationState"), wantGeneration, nil,
		); len(got) != 0 {
			t.Errorf("generationState must hold exactly the reload swap unit: %v", got)
		}
		wantServices := map[string]bool{
			"lifecycle": true, "runtimeState": true, "client": true,
			"quota": true, "metrics": true, "tokens": true, "agents": true,
			"stats": true, "flusher": true, "reqLog": true, "reqLogStarted": true,
			"reqLogIndex": true, "reqLogIndexStarted": true,
			"mcpReqLog": true, "mcpReqLogStarted": true,
			"sessionScan": true, "responsesState": true, "events": true, "adjudication": true,
			"mcpSessions": true, "mcpRR": true, "mcpStdio": true, "mcpStats": true,
			"fusionReg": true, "catalog": true, "budget": true,
			"proxyResolver": true, "transports": true, "transportsMu": true,
			"wireCaps": true, "wireProbe": true,
			"modelCaps": true, "modelCapsPath": true, "cacheStatePath": true,
			"cacheCounters": true, "cachePersistMu": true,
		}
		if got := structContractViolations(
			namedStructFields(t, rootPackage, "processServices"), wantServices, nil,
		); len(got) != 0 {
			t.Errorf("processServices must hold exactly the process-lifetime services: %v", got)
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
		switch fun := call.Fun.(type) {
		case *ast.Ident:
			if fun.Name != function {
				return true
			}
		case *ast.SelectorExpr:
			if fun.Sel.Name != function {
				return true
			}
		default:
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

// structEmbeddedTypes returns the embedded (anonymous) field type names of a
// struct in declaration order. namedStructFields skips embedded fields (they
// have no Names), so group-membership contracts need this separate view.
func structEmbeddedTypes(t *testing.T, f *ast.File, name string) []string {
	t.Helper()
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gen.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok || typeSpec.Name.Name != name {
				continue
			}
			st, ok := typeSpec.Type.(*ast.StructType)
			if !ok {
				t.Fatalf("%s is not a struct", name)
			}
			var embeds []string
			for _, field := range st.Fields.List {
				if len(field.Names) != 0 {
					continue
				}
				ident, ok := field.Type.(*ast.Ident)
				if !ok {
					t.Fatalf("%s embeds non-identifier type %T", name, field.Type)
				}
				embeds = append(embeds, ident.Name)
			}
			return embeds
		}
	}
	t.Fatalf("struct %s not found", name)
	return nil
}
