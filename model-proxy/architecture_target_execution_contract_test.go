package main

import (
	"go/ast"
	"testing"
)

// TestTargetExecutionArchitecture protects the next layer below targetexec.Plan:
// targetexec.Attempt is a deliberately small data contract, the executor has no
// composition-root escape hatch, Fusion's client-facing synthesis reuses that
// executor, and pool resolution reaches health only through resolverState.
func TestTargetExecutionArchitecture(t *testing.T) {
	rootPackage, _ := parseGoPackage(t, "internal/app")

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
					"RuntimeSnapshot": true, "targetPlan": true,
				},
			},
			{
				name: "Exchange",
				want: map[string]bool{"Request": true, "Writer": true, "Body": true},
				forbidden: map[string]bool{
					"Proxy": true, "Config": true, "Store": true, "any": true,
					"RuntimeSnapshot": true, "targetPlan": true,
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
					"RuntimeSnapshot": true, "targetPlan": true,
				},
			},
			{
				name: "Policy",
				want: map[string]bool{"Force": true, "LastTarget": true, "ContextRetry": true},
				forbidden: map[string]bool{
					"Proxy": true, "Config": true, "Store": true, "any": true,
					"RuntimeSnapshot": true, "targetPlan": true,
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
			"RuntimeSnapshot": true, "targetPlan": true,
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
		constructorCalls := importedFunctionReferenceSitesAcrossProduction(
			t,
			"model-proxy/internal/targetexec",
			"NewAttempt",
		)
		if len(constructorCalls) != 1 ||
			constructorCalls[0].file != "internal/app/dispatch_context.go" || constructorCalls[0].function != "newTargetAttempt" {
			t.Errorf("targetexec.NewAttempt production reference sites = %v, want only dispatch_context.go:newTargetAttempt direct call", constructorCalls)
		}
		if sites := packageLocalFunctionCallSites(t, "internal/targetexec", "NewAttempt"); len(sites) != 0 {
			t.Errorf("targetexec package-local NewAttempt calls = %v, want none outside the root factory", sites)
		}
		serveOnce := namedMethod(t, rootPackage, "Proxy", "serveOnce")
		if !assignedFactoryValueExecuted(serveOnce.Body, "newTargetAttempt", "targetExecutor", "Execute") {
			t.Error("Proxy.serveOnce must pass the attempt assigned from newTargetAttempt to targetexec.Executor.Execute")
		}
		if !executorRuntimeBoundToAttempt(serveOnce.Body, "targetExecutor", "Execute") {
			t.Error("Proxy.serveOnce must bind targetExecutor to the exact attempt.Runtime generation")
		}
		executePos := firstNamedCallPos(serveOnce.Body, "Execute")
		shadowPos := firstNamedCallPos(serveOnce.Body, "dispatchShadowAfterCommit")
		if !executePos.IsValid() || !shadowPos.IsValid() || shadowPos <= executePos {
			t.Errorf("Proxy.serveOnce must dispatch post-commit Shadow after execute (execute=%v shadow=%v)", executePos, shadowPos)
		}

		fusionFile, fusionSet := parseGoFile(t, "internal/app/fusion.go")
		if got := compositeLiteralSites(fusionFile, "Attempt"); len(got) != 0 {
			t.Errorf("fusion.go must call newTargetAttempt, not construct targetexec.Attempt: %s", describeNodes(fusionSet, got, "Attempt literal"))
		}
		synth := namedMethod(t, fusionFile, "Proxy", "callFusionSynthesizer")
		if !assignedFactoryValueExecuted(synth.Body, "newTargetAttempt", "targetExecutor", "Execute") {
			t.Error("callFusionSynthesizer must pass the attempt assigned from newTargetAttempt to targetexec.Executor.Execute")
		}
		if !executorRuntimeBoundToAttempt(synth.Body, "targetExecutor", "Execute") {
			t.Error("callFusionSynthesizer must bind targetExecutor to the exact attempt.Runtime generation")
		}
	})

	t.Run("targetexec Executor owns IO without root scheduling reload or Web access", func(t *testing.T) {
		f, fset := parseGoFile(t, "internal/targetexec/executor.go")
		executorFields := namedStructFields(t, f, "Executor")
		wantFields := map[string]bool{
			"Client": true, "State": true, "Effects": true, "Responses": true,
		}
		if got := structContractViolations(executorFields, wantFields, map[string]bool{
			"Proxy": true, "RuntimeSnapshot": true, "targetPlan": true, "any": true,
		}); len(got) != 0 {
			t.Errorf("targetexec.Executor contract violations: %v", got)
		}
		execute := namedMethod(t, f, "Executor", "Execute")
		if functionSignatureContainsIdent(execute, "Proxy") {
			t.Error("targetexec.Executor.Execute must not receive *Proxy")
		}
		forbidden := map[string]bool{
			"schedule": true, "reload": true, "SnapshotRuntime": true,
			"NewWebServer": true, "serveHTTPUntilShutdown": true,
			"StartRuntimeServices": true, "contextOverflowRetry": true,
			"runShadow": true, "dispatchShadowAfterCommit": true,
			"shouldSample": true, "runBeforeLogDrain": true,
		}
		if got := forbiddenCallSites(f, fset, forbidden, nil); len(got) != 0 {
			t.Errorf("internal/targetexec/executor.go crossed orchestration boundary: %v", got)
		}
		forbiddenTypes := map[string]bool{
			"Proxy": true, "proxyLifecycle": true, "shadowRuntime": true,
			"ShadowTarget": true, "RuntimeSnapshot": true, "targetPlan": true,
		}
		if got := forbiddenIdentifierSites(f, fset, forbiddenTypes); len(got) != 0 {
			t.Errorf("internal/targetexec/executor.go retained forbidden owner/orchestration types: %v", got)
		}
		allowedImports := cloneBoolMap(internalRepositoryImportPolicy()["model-proxy/internal/targetexec"])
		delete(allowedImports, "model-proxy/provider") // executor.go must not build providers directly.
		if got := unexpectedRepositoryImports(f, allowedImports); len(got) != 0 {
			t.Errorf("internal/targetexec/executor.go imports outside its execution leaves: %v", got)
		}

		adapter, adapterSet := parseGoFile(t, "internal/app/targetexec_adapter.go")
		factory := namedMethod(t, adapter, "Proxy", "targetExecutor")
		adapterForbidden := map[string]bool{
			"Do": true, "ConvertRequest": true, "ConvertResponse": true,
			"ConvertSSE": true, "flushCopy": true, "contextOverflowRetry": true,
			"dispatchShadowAfterCommit": true, "runShadow": true,
		}
		if got := forbiddenCallSites(factory.Body, adapterSet, adapterForbidden, nil); len(got) != 0 {
			t.Errorf("targetExecutor factory contains execution/orchestration logic: %v", got)
		}
	})

	t.Run("Fusion synthesizer delegates client delivery to target executor", func(t *testing.T) {
		f, fset := parseGoFile(t, "internal/app/fusion.go")
		synth := namedMethod(t, f, "Proxy", "callFusionSynthesizer")
		if !assignedFactoryValueExecuted(synth.Body, "newTargetAttempt", "targetExecutor", "Execute") {
			t.Error("callFusionSynthesizer must execute the exact value returned by newTargetAttempt")
		}
		if !executorRuntimeBoundToAttempt(synth.Body, "targetExecutor", "Execute") {
			t.Error("callFusionSynthesizer must execute with the same attempt.Runtime generation")
		}
		if got := namedCallCountInNode(synth.Body, "Do"); got != 0 {
			t.Errorf("callFusionSynthesizer must not add a parallel client.Do pipeline: found %d call(s)", got)
		}
		forbiddenShadow := map[string]bool{"runShadow": true, "dispatchShadowAfterCommit": true}
		if got := forbiddenCallSites(synth.Body, fset, forbiddenShadow, nil); len(got) != 0 {
			t.Errorf("callFusionSynthesizer must not dispatch Shadow: %v", got)
		}
	})

	t.Run("resolver depends on ResolverState instead of Proxy", func(t *testing.T) {
		// The resolver struct and its narrow state port now live in
		// internal/routing; the root file only aliases them.
		f, _ := parseGoFile(t, "internal/routing/resolver.go")
		fields := namedStructFields(t, f, "Resolver")
		if got := simpleTypeName(fields["state"]); got != "ResolverState" {
			t.Errorf("Resolver.state type = %q, want ResolverState", got)
		}
		for name, typ := range fields {
			if typeContainsIdent(typ, "Proxy") {
				t.Errorf("Resolver.%s retains forbidden Proxy dependency", name)
			}
		}
		factory := namedFunction(t, f, "NewResolver")
		if functionSignatureContainsIdent(factory, "Proxy") {
			t.Error("NewResolver must receive ResolverState, not *Proxy")
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
					selector.Sel.Name == "Generation" {
					bound++
				}
				return true
			})
			return calls, bound
		}
		fusion, _ := parseGoFile(t, "internal/app/fusion.go")
		if calls, bound := countGenerationBoundResolvers(fusion); calls != 2 || bound != calls {
			t.Errorf("Fusion resolver calls must bind runtime generation: calls=%d bound=%d", calls, bound)
		}
		shadow := namedMethod(t, rootPackage, "Proxy", "runShadow")
		if calls, bound := countGenerationBoundResolvers(shadow.Body); calls != 1 || bound != calls {
			t.Errorf("Shadow resolver call must bind runtime generation: calls=%d bound=%d", calls, bound)
		}
	})
}
