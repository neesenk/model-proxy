package main

import (
	"go/ast"
	"path/filepath"
	"testing"
)

// TestTargetExecutionArchitecture protects the next layer below targetexec.Plan:
// targetexec.Attempt is a deliberately small data contract, the executor has no
// composition-root escape hatch, Fusion's client-facing synthesis reuses that
// executor, and pool resolution reaches health only through resolverState.
func TestTargetExecutionArchitecture(t *testing.T) {
	rootPackage, _ := parseGoPackage(t, ".")

	t.Run("targetexec Attempt has only its five execution contract fields", func(t *testing.T) {
		f, _ := parseGoFile(t, "internal/targetexec/attempt.go")
		fields := namedStructFields(t, f, "Attempt")
		want := map[string]bool{
			"runtime": true, "plan": true, "exchange": true, "scope": true, "policy": true,
		}
		if len(fields) != len(want) {
			t.Errorf("targetexec.Attempt fields = %v, want exactly %v", sortedFieldNames(fields), sortedBoolNames(want))
		}
		for name := range fields {
			if !want[name] {
				t.Errorf("targetexec.Attempt must not retain %q outside its five-field contract", name)
			}
		}
		for name := range want {
			if _, ok := fields[name]; !ok {
				t.Errorf("targetexec.Attempt missing required contract field %q", name)
			}
		}
		for name, typ := range fields {
			if typeContainsIdent(typ, "any") {
				t.Errorf("targetexec.Attempt.%s must remain strongly typed", name)
			}
		}
	})

	t.Run("attempt groups keep exact fields and no owner duplication", func(t *testing.T) {
		f, _ := parseGoFile(t, "internal/targetexec/attempt.go")
		contracts := []struct {
			name      string
			want      map[string]bool
			forbidden map[string]bool
		}{
			{
				name: "Runtime",
				want: map[string]bool{"Scheduling": true, "Generation": true, "Cache": true},
				forbidden: map[string]bool{
					"Proxy": true, "Config": true, "any": true,
					"runtimeSnapshot": true, "targetPlan": true,
				},
			},
			{
				name: "Exchange",
				want: map[string]bool{"Request": true, "Writer": true, "Body": true},
				forbidden: map[string]bool{
					"Proxy": true, "Config": true, "Store": true, "any": true,
					"runtimeSnapshot": true, "targetPlan": true,
				},
			},
			{
				name: "Scope",
				want: map[string]bool{
					"CalledModel": true, "Agent": true, "CacheKey": true, "Log": true,
					"ResponseContext": true, "ResponsesHistory": true, "ResponsesSession": true,
				},
				// ResponsesHistory is intentionally []any because it carries
				// protocol JSON; the group still cannot regain root owners.
				forbidden: map[string]bool{
					"Proxy": true, "Config": true, "Store": true,
					"runtimeSnapshot": true, "targetPlan": true,
				},
			},
			{
				name: "Policy",
				want: map[string]bool{"Force": true, "LastTarget": true, "ContextRetry": true},
				forbidden: map[string]bool{
					"Proxy": true, "Config": true, "Store": true, "any": true,
					"runtimeSnapshot": true, "targetPlan": true,
				},
			},
		}
		for _, contract := range contracts {
			if got := structContractViolations(namedStructFields(t, f, contract.name), contract.want, contract.forbidden); len(got) != 0 {
				t.Errorf("%s contract violations: %v", contract.name, got)
			}
		}
	})

	t.Run("targetexec Commit exposes only the post-commit request body", func(t *testing.T) {
		f, _ := parseGoFile(t, "internal/targetexec/attempt.go")
		want := map[string]bool{"requestBody": true}
		forbiddenTypes := map[string]bool{
			"Proxy": true, "Config": true, "Store": true,
			"runtimeSnapshot": true, "targetPlan": true,
			"proxyLifecycle": true, "shadowRuntime": true,
		}
		if got := structContractViolations(namedStructFields(t, f, "Commit"), want, forbiddenTypes); len(got) != 0 {
			t.Errorf("targetexec.Commit contract violations: %v", got)
		}
	})

	t.Run("only targetexec NewAttempt may construct Attempt", func(t *testing.T) {
		factoryCount := 0
		literalSites := []string{}
		paths := productionGoFilesIn(t, "internal/targetexec")
		for _, path := range paths {
			f, fset := parseGoFile(t, path)
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				if fn.Name.Name == "NewAttempt" {
					factoryCount++
				}
				for _, site := range compositeLiteralSites(fn.Body, "Attempt") {
					if fn.Name.Name != "NewAttempt" {
						literalSites = append(literalSites, describe(fset, site, fn.Name.Name+" constructs targetexec.Attempt"))
					}
				}
			}
		}
		if factoryCount != 1 {
			t.Errorf("targetexec.NewAttempt declarations = %d, want exactly 1", factoryCount)
		}
		if len(literalSites) != 0 {
			t.Errorf("targetexec.Attempt composite literals must be confined to targetexec.NewAttempt: %v", literalSites)
		}
		var constructorCalls []importedFunctionCallSite
		for _, path := range productionGoFilesRecursively(t, ".") {
			f, fset := parseGoFile(t, path)
			constructorCalls = append(constructorCalls, importedFunctionCallSites(
				f,
				fset,
				path,
				"model-proxy/internal/targetexec",
				"NewAttempt",
			)...)
		}
		if len(constructorCalls) != 1 {
			t.Errorf("targetexec.NewAttempt production call sites = %v, want exactly dispatch_context.go:newTargetAttempt", constructorCalls)
		} else if site := constructorCalls[0]; filepath.Base(site.file) != "dispatch_context.go" || site.function != "newTargetAttempt" {
			t.Errorf("targetexec.NewAttempt call site = %v, want dispatch_context.go:newTargetAttempt", site)
		}
		serveOnce := namedMethod(t, rootPackage, "Proxy", "serveOnce")
		if !assignedFactoryValueExecuted(serveOnce.Body, "newTargetAttempt", "targetExecutor", "execute") {
			t.Error("Proxy.serveOnce must pass the attempt assigned from newTargetAttempt to targetExecutor().execute")
		}
		executePos := firstNamedCallPos(serveOnce.Body, "execute")
		shadowPos := firstNamedCallPos(serveOnce.Body, "dispatchShadowAfterCommit")
		if !executePos.IsValid() || !shadowPos.IsValid() || shadowPos <= executePos {
			t.Errorf("Proxy.serveOnce must dispatch post-commit Shadow after execute (execute=%v shadow=%v)", executePos, shadowPos)
		}

		fusionFile, fusionSet := parseGoFile(t, "fusion.go")
		if got := compositeLiteralSites(fusionFile, "Attempt"); len(got) != 0 {
			t.Errorf("fusion.go must call newTargetAttempt, not construct targetexec.Attempt: %s", describeNodes(fusionSet, got, "Attempt literal"))
		}
		synth := namedMethod(t, fusionFile, "Proxy", "callFusionSynthesizer")
		if !assignedFactoryValueExecuted(synth.Body, "newTargetAttempt", "targetExecutor", "execute") {
			t.Error("callFusionSynthesizer must pass the attempt assigned from newTargetAttempt to targetExecutor().execute")
		}
	})

	t.Run("attemptExecutor cannot regain Proxy scheduling reload or Web access", func(t *testing.T) {
		f, fset := parseGoFile(t, "attempt_executor.go")
		for name, typ := range namedStructFields(t, f, "attemptExecutor") {
			if typeContainsIdent(typ, "Proxy") {
				t.Errorf("attemptExecutor.%s retains forbidden Proxy dependency", name)
			}
		}
		execute := namedMethod(t, f, "attemptExecutor", "execute")
		if functionSignatureContainsIdent(execute, "Proxy") {
			t.Error("attemptExecutor.execute must not receive *Proxy")
		}
		forbidden := map[string]bool{
			"schedule": true, "reload": true, "snapshotRuntime": true,
			"newWebServer": true, "serveHTTPUntilShutdown": true,
			"startRuntimeServices": true, "contextOverflowRetry": true,
			"runShadow": true, "dispatchShadowAfterCommit": true,
			"shouldSample": true, "runBeforeLogDrain": true,
		}
		if got := forbiddenCallSites(f, fset, forbidden, nil); len(got) != 0 {
			t.Errorf("attempt_executor.go crossed orchestration boundary: %v", got)
		}
		forbiddenTypes := map[string]bool{
			"proxyLifecycle": true, "shadowRuntime": true, "ShadowTarget": true,
		}
		if got := forbiddenIdentifierSites(f, fset, forbiddenTypes); len(got) != 0 {
			t.Errorf("attempt_executor.go retained forbidden owner/orchestration types: %v", got)
		}
		proxyMethods := receiverMethodNames(t, productionGoFiles(t), "Proxy")
		targetExecutor := namedMethod(t, f, "Proxy", "targetExecutor")
		if got := methodValueInjections(targetExecutor.Body, "attemptExecutor", "p", proxyMethods); len(got) != 0 {
			t.Errorf("targetExecutor injects Proxy method values into attemptExecutor: %v", describeExprNodes(fset, got, "Proxy method value"))
		}
	})

	t.Run("Fusion synthesizer delegates client delivery to target executor", func(t *testing.T) {
		f, fset := parseGoFile(t, "fusion.go")
		synth := namedMethod(t, f, "Proxy", "callFusionSynthesizer")
		if !assignedFactoryValueExecuted(synth.Body, "newTargetAttempt", "targetExecutor", "execute") {
			t.Error("callFusionSynthesizer must execute the exact value returned by newTargetAttempt")
		}
		if got := namedCallCountInNode(synth.Body, "Do"); got != 0 {
			t.Errorf("callFusionSynthesizer must not add a parallel client.Do pipeline: found %d call(s)", got)
		}
		forbiddenShadow := map[string]bool{"runShadow": true, "dispatchShadowAfterCommit": true}
		if got := forbiddenCallSites(synth.Body, fset, forbiddenShadow, nil); len(got) != 0 {
			t.Errorf("callFusionSynthesizer must not dispatch Shadow: %v", got)
		}
	})

	t.Run("resolver depends on resolverState instead of Proxy", func(t *testing.T) {
		f, _ := parseGoFile(t, "resolve.go")
		fields := namedStructFields(t, f, "resolver")
		if got := simpleTypeName(fields["state"]); got != "resolverState" {
			t.Errorf("resolver.state type = %q, want resolverState", got)
		}
		for name, typ := range fields {
			if typeContainsIdent(typ, "Proxy") {
				t.Errorf("resolver.%s retains forbidden Proxy dependency", name)
			}
		}
		factory := namedFunction(t, f, "newResolver")
		if functionSignatureContainsIdent(factory, "Proxy") {
			t.Error("newResolver must receive resolverState, not *Proxy")
		}

		countGenerationBoundResolvers := func(node ast.Node) (calls, bound int) {
			t.Helper()
			ast.Inspect(node, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok || callableName(call.Fun) != "newResolver" {
					return true
				}
				calls++
				if len(call.Args) == 0 {
					return true
				}
				if selector, ok := call.Args[len(call.Args)-1].(*ast.SelectorExpr); ok &&
					selector.Sel.Name == "generation" {
					bound++
				}
				return true
			})
			return calls, bound
		}
		fusion, _ := parseGoFile(t, "fusion.go")
		if calls, bound := countGenerationBoundResolvers(fusion); calls != 2 || bound != calls {
			t.Errorf("Fusion resolver calls must bind runtime generation: calls=%d bound=%d", calls, bound)
		}
		shadow := namedMethod(t, rootPackage, "Proxy", "runShadow")
		if calls, bound := countGenerationBoundResolvers(shadow.Body); calls != 1 || bound != calls {
			t.Errorf("Shadow resolver call must bind runtime generation: calls=%d bound=%d", calls, bound)
		}
	})
}
