package main

import (
	"go/ast"
	"testing"
)

// TestTargetExecutionArchitecture protects the next layer below targetPlan:
// targetAttempt is a deliberately small data contract, the executor has no
// composition-root escape hatch, Fusion's client-facing synthesis reuses that
// executor, and pool resolution reaches health only through resolverState.
func TestTargetExecutionArchitecture(t *testing.T) {
	rootPackage, _ := parseGoPackage(t, ".")

	t.Run("targetAttempt has only its five execution contract fields", func(t *testing.T) {
		f, _ := parseGoFile(t, "dispatch_context.go")
		fields := namedStructFields(t, f, "targetAttempt")
		want := map[string]bool{
			"runtime": true, "plan": true, "exchange": true, "scope": true, "policy": true,
		}
		if len(fields) != len(want) {
			t.Errorf("targetAttempt fields = %v, want exactly %v", sortedFieldNames(fields), sortedBoolNames(want))
		}
		for name := range fields {
			if !want[name] {
				t.Errorf("targetAttempt must not retain %q outside its five-field contract", name)
			}
		}
		for name := range want {
			if _, ok := fields[name]; !ok {
				t.Errorf("targetAttempt missing required contract field %q", name)
			}
		}
	})

	t.Run("attempt groups keep exact fields and no owner duplication", func(t *testing.T) {
		f, _ := parseGoFile(t, "dispatch_context.go")
		contracts := []struct {
			name string
			want map[string]bool
		}{
			{
				name: "attemptExchange",
				want: map[string]bool{"request": true, "writer": true, "body": true},
			},
			{
				name: "attemptScope",
				want: map[string]bool{
					"calledModel": true, "agent": true, "cacheKey": true, "log": true,
					"responseContext": true, "responsesHistory": true, "responsesSession": true,
				},
			},
			{
				name: "attemptPolicy",
				want: map[string]bool{"force": true, "lastTarget": true, "contextRetry": true},
			},
		}
		forbiddenTypes := map[string]bool{
			"Proxy": true, "Config": true, "Store": true,
			"runtimeSnapshot": true, "targetPlan": true,
		}
		for _, contract := range contracts {
			if got := structContractViolations(namedStructFields(t, f, contract.name), contract.want, forbiddenTypes); len(got) != 0 {
				t.Errorf("%s contract violations: %v", contract.name, got)
			}
		}
	})

	t.Run("attemptCommit exposes only the post-commit request body", func(t *testing.T) {
		f, _ := parseGoFile(t, "attempt_executor.go")
		want := map[string]bool{"requestBody": true}
		forbiddenTypes := map[string]bool{
			"Proxy": true, "Config": true, "Store": true,
			"runtimeSnapshot": true, "targetPlan": true,
			"proxyLifecycle": true, "shadowRuntime": true,
		}
		if got := structContractViolations(namedStructFields(t, f, "attemptCommit"), want, forbiddenTypes); len(got) != 0 {
			t.Errorf("attemptCommit contract violations: %v", got)
		}
	})

	t.Run("only newTargetAttempt may construct targetAttempt", func(t *testing.T) {
		factoryCount := 0
		literalSites := []string{}
		for _, path := range productionGoFiles(t) {
			f, fset := parseGoFile(t, path)
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				if fn.Name.Name == "newTargetAttempt" {
					factoryCount++
				}
				for _, site := range compositeLiteralSites(fn.Body, "targetAttempt") {
					if fn.Name.Name != "newTargetAttempt" {
						literalSites = append(literalSites, describe(fset, site, fn.Name.Name+" constructs targetAttempt"))
					}
				}
			}
		}
		if factoryCount != 1 {
			t.Errorf("newTargetAttempt declarations = %d, want exactly 1", factoryCount)
		}
		if len(literalSites) != 0 {
			t.Errorf("targetAttempt composite literals must be confined to newTargetAttempt: %v", literalSites)
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
		if got := compositeLiteralSites(fusionFile, "targetAttempt"); len(got) != 0 {
			t.Errorf("fusion.go must call newTargetAttempt, not construct targetAttempt: %s", describeNodes(fusionSet, got, "targetAttempt literal"))
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
