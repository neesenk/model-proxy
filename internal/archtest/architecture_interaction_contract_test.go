package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestArchitectureRootInteractionContracts protects the small set of places
// where root composition deliberately crosses into stateful internal modules.
// Each check scans the whole production package, then verifies the one reviewed
// owner file/function and the local dataflow through that owner.
func TestArchitectureRootInteractionContracts(t *testing.T) {
	t.Run("application runtime is the sole process assembly", func(t *testing.T) {
		for _, contract := range []struct {
			call     string
			function string
			receiver string
		}{
			{call: "NewProxy", function: "NewRuntime"},
			{call: "StartRuntimeServices", function: "NewRuntime", receiver: "proxy"},
			{call: "NewWebServer", function: "NewRuntime"},
		} {
			var sites []interactionSite
			if contract.receiver != "" {
				sites = rootReceiverCallSites(t, contract.receiver, contract.call)
			} else {
				sites = rootFunctionReferenceSites(t, contract.call)
			}
			if len(sites) != 1 || sites[0].file != "runtime.go" || sites[0].function != contract.function {
				t.Errorf("%s production reference sites = %v, want only internal/app/runtime.go:%s direct call", contract.call, sites, contract.function)
			}
		}

		serve, _ := parseGoFile(t, "cli_serve.go")
		if got := namedCallCount(serve, "newApplicationRuntime"); got != 1 {
			t.Errorf("cli_serve.go newApplicationRuntime calls = %d, want one serve handoff", got)
		}
	})

	t.Run("target executor is assembled only by its forward adapter", func(t *testing.T) {
		planSites := importedFunctionCallSitesAcrossProduction(t, "model-proxy/internal/targetexec", "NewPlan")
		if len(planSites) != 1 || planSites[0].file != "internal/forward/plan.go" || planSites[0].function != "planTarget" {
			t.Errorf("targetexec.NewPlan production call sites = %v, want only forward/plan.go:planTarget", planSites)
		}
		if sites := packageLocalFunctionCallSites(t, "internal/targetexec", "NewPlan"); len(sites) != 0 {
			t.Errorf("targetexec package-local NewPlan calls = %v, want none outside the forward plan owner", sites)
		}
		if sites := importedFunctionReferenceSitesAcrossProduction(t, "model-proxy/internal/targetexec", "NewPlan"); len(sites) != 1 ||
			sites[0].file != "internal/forward/plan.go" || sites[0].function != "planTarget" {
			t.Errorf("targetexec.NewPlan production reference sites = %v, want only forward/plan.go:planTarget direct call", sites)
		}

		sites := importedCompositeLiteralSites(t, "model-proxy/internal/targetexec", "Executor")
		if len(sites) != 1 || sites[0].file != "internal/forward/plan.go" || sites[0].function != "targetExecutor" {
			t.Errorf("targetexec.Executor production composites = %v, want only forward/plan.go:targetExecutor", sites)
		}
		if sites := packageLocalCompositeLiteralSites(t, "internal/targetexec", "Executor"); len(sites) != 0 {
			t.Errorf("targetexec package-local Executor composites = %v, want none outside the forward adapter", sites)
		}
		if sites := importedTypeValueReferenceSitesAcrossProduction(t, "model-proxy/internal/targetexec", "Executor"); len(sites) != 1 ||
			sites[0].file != "internal/forward/plan.go" || sites[0].function != "targetExecutor" {
			t.Errorf("targetexec.Executor production value references = %v, want only forward/plan.go:targetExecutor direct composite", sites)
		}
		if sites := importedTypeDeclarationSitesAcrossProduction(t, "model-proxy/internal/targetexec", "Executor"); len(sites) != 0 {
			t.Errorf("targetexec.Executor type aliases = %v, want none", sites)
		}

		adapter, _ := parseGoFile(t, "internal/forward/plan.go")
		factory := namedMethod(t, adapter, "pipeline", "targetExecutor")
		if got := selectorCompositeCountInNode(factory.Body, "targetexec", "GateState"); got != 1 {
			t.Errorf("pipeline.targetExecutor targetexec.GateState composites = %d, want one frozen-state adapter", got)
		}
	})

	t.Run("forward uses its constructed routing planner", func(t *testing.T) {
		forward, _ := parseGoFile(t, "internal/forward/forward.go")
		serveOnce := namedMethod(t, forward, "pipeline", "serveOnce")
		if !constructedPlannerOwnsRoutingCalls(serveOnce.Body) {
			t.Error("pipeline.serveOnce must call ApplyWithProfile and ContextOverflowRetryWithProfile exactly once on the value assigned from requestRoutingPlanner")
		}
	})

	t.Run("normal delivery gates Shadow on its exact committed result", func(t *testing.T) {
		forward, _ := parseGoFile(t, "internal/forward/forward.go")
		serveOnce := namedMethod(t, forward, "pipeline", "serveOnce")
		if !normalDeliveryShadowDispatchValid(serveOnce.Body) {
			t.Error("pipeline.serveOnce must dispatch Shadow only inside the exact executor-result Committed branch and pass that result's Commit")
		}
	})

	t.Run("Shadow dispatch uses one captured generation runtime", func(t *testing.T) {
		shadowFile, _ := parseGoFile(t, "internal/app/proxy_shadow.go")
		dispatch := namedMethod(t, shadowFile, "Proxy", "dispatchShadowAfterCommit")
		if !shadowDispatchUsesCapturedRuntime(dispatch.Body) {
			t.Error("Proxy.dispatchShadowAfterCommit must use one runtime.shadow value for sampling, admission, and runShadow")
		}
	})

	t.Run("fusion engine and shadow runtime have one owner each", func(t *testing.T) {
		fusionSites := importedCompositeLiteralSites(t, "model-proxy/internal/fusion", "Engine")
		if len(fusionSites) != 1 || fusionSites[0].file != "internal/forward/fusion.go" || fusionSites[0].function != "runFusion" {
			t.Errorf("fusion.Engine production composites = %v, want only forward/fusion.go:runFusion", fusionSites)
		}
		if sites := packageLocalCompositeLiteralSites(t, "internal/fusion", "Engine"); len(sites) != 0 {
			t.Errorf("fusion package-local Engine composites = %v, want none outside the forward adapter", sites)
		}
		if sites := importedTypeValueReferenceSitesAcrossProduction(t, "model-proxy/internal/fusion", "Engine"); len(sites) != 1 ||
			sites[0].file != "internal/forward/fusion.go" || sites[0].function != "runFusion" {
			t.Errorf("fusion.Engine production value references = %v, want only forward/fusion.go:runFusion direct composite", sites)
		}
		if sites := importedTypeDeclarationSitesAcrossProduction(t, "model-proxy/internal/fusion", "Engine"); len(sites) != 0 {
			t.Errorf("fusion.Engine type aliases = %v, want none", sites)
		}

		shadowSites := importedFunctionCallSitesAcrossProduction(t, "model-proxy/internal/shadow", "NewRuntime")
		if len(shadowSites) != 2 ||
			shadowSites[0].file != "internal/app/proxy.go" || shadowSites[0].function != "NewProxyWithStatePath" ||
			shadowSites[1].file != "internal/app/proxy_reload.go" || shadowSites[1].function != "Reload" {
			t.Errorf("shadow.NewRuntime production call sites = %v, want constructor and Proxy.Reload only", shadowSites)
		}
		if sites := packageLocalFunctionCallSites(t, "internal/shadow", "NewRuntime"); len(sites) != 0 {
			t.Errorf("shadow package-local NewRuntime calls = %v, want none outside the root generation owners", sites)
		}
		if sites := importedFunctionReferenceSitesAcrossProduction(t, "model-proxy/internal/shadow", "NewRuntime"); len(sites) != 2 ||
			sites[0].file != "internal/app/proxy.go" || sites[0].function != "NewProxyWithStatePath" ||
			sites[1].file != "internal/app/proxy_reload.go" || sites[1].function != "Reload" {
			t.Errorf("shadow.NewRuntime production reference sites = %v, want constructor and Proxy.Reload direct calls only", sites)
		}

		executeSites := importedTypeMethodCallSitesAcrossProduction(t, "model-proxy/internal/shadow", "Runtime", "Execute")
		if len(executeSites) != 1 || executeSites[0].file != "internal/app/proxy_shadow.go" || executeSites[0].function != "runShadow" {
			t.Errorf("shadow Runtime.Execute production call sites = %v, want only proxy_shadow.go:runShadow", executeSites)
		}
		if sites := importedMethodExpressionSitesAcrossProduction(t, "model-proxy/internal/shadow", "Runtime", "Execute"); len(sites) != 0 {
			t.Errorf("shadow Runtime.Execute method expressions = %v, want none", sites)
		}
		if sites := importedTypeDeclarationSitesAcrossProduction(t, "model-proxy/internal/shadow", "Runtime"); len(sites) != 0 {
			t.Errorf("shadow.Runtime type aliases = %v, want none", sites)
		}
		if sites := rootMethodValueReferenceSites(t, "Execute"); len(sites) != 0 {
			t.Errorf("root method-value Execute references = %v, want none; shadow execution must stay a direct call", sites)
		}
	})

	t.Run("web transport is built only through the web adapter", func(t *testing.T) {
		sites := importedFunctionCallSitesAcrossProduction(t, "model-proxy/internal/web", "New")
		if len(sites) != 1 || sites[0].file != "internal/app/web_adapter.go" || sites[0].function != "mustNewWebTransport" {
			t.Errorf("web.New production call sites = %v, want only web_adapter.go:mustNewWebTransport", sites)
		}
		if sites := packageLocalFunctionCallSites(t, "internal/web", "New"); len(sites) != 0 {
			t.Errorf("web package-local New calls = %v, want none outside the root adapter", sites)
		}
		if sites := importedFunctionReferenceSitesAcrossProduction(t, "model-proxy/internal/web", "New"); len(sites) != 1 ||
			sites[0].file != "internal/app/web_adapter.go" || sites[0].function != "mustNewWebTransport" {
			t.Errorf("web.New production reference sites = %v, want only web_adapter.go:mustNewWebTransport direct call", sites)
		}
	})

	t.Run("forward pipeline symbols have one owner", func(t *testing.T) {
		// forward must never reach back into the composition root (also closed
		// structurally by the dependency DAG: app → forward would cycle).
		for _, path := range productionGoFilesIn(t, "internal/forward") {
			file, _ := parseGoFile(t, path)
			for _, spec := range file.Imports {
				if strings.Trim(spec.Path.Value, `"`) == "model-proxy/internal/app" {
					t.Errorf("%s imports internal/app; the pipeline consumes Snapshot/Services/RouteState ports only", path)
				}
			}
		}
		// The app side keeps only the shim (Proxy.forward) and the port
		// adapters; moved pipeline symbols must not be re-declared there.
		moved := map[string]bool{
			"serveOnce": true, "serveRequest": true, "serveState": true, "serveResult": true,
			"requestProfile": true, "writeAllTargetsFailed": true, "statusClientGone": true,
			"runFusion": true, "fusionCtx": true, "fusionAdapter": true,
			"fusionSynthesizerSupportsTools": true, "callFusionLeg": true,
			"callFusionSynthesizer": true, "expandFusionResponses": true,
			"newTargetAttempt": true, "forwardLogCtx": true, "buildRequestLogInput": true,
			"targetPlanInput": true, "planTarget": true, "targetExecutor": true,
			"forcedProviderFromRequest": true,
			"requestRoutingScheduler":   true, "requestRoutingPlanner": true,
			"evaluateRequestGuard": true, "requestGuardDecision": true, "auditGuardHit": true,
		}
		appPackage, _ := parseGoPackage(t, "internal/app")
		for _, decl := range appPackage.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if moved[d.Name.Name] {
					t.Errorf("internal/app re-declares moved pipeline symbol %s", d.Name.Name)
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					if ts, ok := spec.(*ast.TypeSpec); ok && moved[ts.Name.Name] {
						t.Errorf("internal/app re-declares moved pipeline type %s", ts.Name.Name)
					}
				}
			}
		}
	})

	t.Run("application owns command registration and main only crosses the OS boundary", func(t *testing.T) {
		app, _ := parseGoFile(t, "internal/cli/app.go")
		constructor := namedFunction(t, app, "NewApplication")
		// Every entry in the Commands registry literal must carry exactly one
		// ProcessCommand binding and vice versa — the count is derived from the
		// literal itself so adding/removing a command cannot drift past this
		// check via a stale hardcoded number.
		registryEntries := 0
		ast.Inspect(constructor.Body, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok || len(lit.Elts) == 0 {
				return true
			}
			mt, ok := lit.Type.(*ast.MapType)
			if !ok {
				return true
			}
			if ident, ok := mt.Value.(*ast.Ident); ok && ident.Name == "Command" {
				registryEntries = len(lit.Elts)
			}
			return true
		})
		if registryEntries == 0 {
			t.Fatal("no Commands map literal found in NewApplication — registry-count contract is blind")
		}
		if got := namedCallCountInNode(constructor.Body, "ProcessCommand"); got != registryEntries {
			t.Errorf("newApplication: %d ProcessCommand registrations for %d Commands registry entries — every registered command needs a binding and vice versa", got, registryEntries)
		}

		sites := rootFunctionReferenceSites(t, "newApplication")
		if len(sites) != 1 ||
			sites[0].file != "main.go" || sites[0].function != "main" {
			t.Errorf("newApplication production reference sites = %v, want only the main direct call", sites)
		}
	})
}

type interactionSite struct {
	file     string
	function string
}

func (site interactionSite) String() string { return site.file + ":" + site.function }

func rootReceiverCallSites(t *testing.T, receiver, method string) []interactionSite {
	t.Helper()
	var sites []interactionSite
	for _, path := range productionGoFilesRecursively(t, ".") {
		file, _ := parseGoFile(t, path)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			for _, call := range namedCallsInNode(fn.Body, method) {
				if selectorOnIdent(call.Fun, receiver, method) {
					sites = append(sites, interactionSite{file: filepath.Base(path), function: fn.Name.Name})
				}
			}
		}
	}
	return sites
}

func importedFunctionCallSitesAcrossProduction(t *testing.T, importPath, function string) []importedFunctionCallSite {
	t.Helper()
	var sites []importedFunctionCallSite
	for _, path := range productionGoFilesRecursively(t, ".") {
		file, fset := parseGoFile(t, path)
		sites = append(sites, importedFunctionCallSites(file, fset, filepath.ToSlash(path), importPath, function)...)
	}
	return sites
}

func importedCompositeLiteralSites(t *testing.T, importPath, typeName string) []interactionSite {
	t.Helper()
	var sites []interactionSite
	for _, path := range productionGoFilesRecursively(t, ".") {
		file, _ := parseGoFile(t, path)
		localName := importedPackageName(file, importPath)
		if localName == "" || localName == "_" || localName == "." {
			continue
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				if node == nil {
					return true
				}
				literal, ok := node.(*ast.CompositeLit)
				if !ok {
					return true
				}
				selector, selectorOK := literal.Type.(*ast.SelectorExpr)
				if !selectorOK {
					return true
				}
				base, baseOK := selector.X.(*ast.Ident)
				if baseOK && base.Name == localName && selector.Sel.Name == typeName {
					sites = append(sites, interactionSite{file: filepath.ToSlash(path), function: fn.Name.Name})
				}
				return true
			})
		}
	}
	return sites
}

func importedPackageName(file *ast.File, importPath string) string {
	for _, spec := range file.Imports {
		if spec.Path.Value == `"`+importPath+`"` {
			if spec.Name != nil {
				return spec.Name.Name
			}
			return filepath.Base(importPath)
		}
	}
	return ""
}

func receiverCallCount(node ast.Node, receiver, method string) int {
	count := 0
	ast.Inspect(node, func(node ast.Node) bool {
		if node == nil {
			return true
		}
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
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

func constructedPlannerOwnsRoutingCalls(node ast.Node) bool {
	planner, _, ok := uniquelyAssignedCallIdentifier(node, func(call *ast.CallExpr) bool {
		return identIs(call.Fun, "requestRoutingPlanner")
	})
	if !ok || identifierAssignmentCount(node, planner) != 1 {
		return false
	}
	for _, method := range []string{"ApplyWithProfile", "ContextOverflowRetryWithProfile"} {
		if namedCallCountInNode(node, method) != 1 || receiverCallCount(node, planner, method) != 1 {
			return false
		}
	}
	return true
}

func normalDeliveryShadowDispatchValid(node ast.Node) bool {
	result, executePos, ok := uniquelyAssignedCallIdentifier(node, func(call *ast.CallExpr) bool {
		_, ok := exactTargetExecutorAttempt(call)
		return ok
	})
	if !ok || identifierAssignmentCount(node, result) != 1 {
		return false
	}

	allDispatches := namedCallsInNode(node, "dispatchShadowAfterCommit")
	if len(allDispatches) != 1 {
		return false
	}

	var committedBranch *ast.IfStmt
	branchCount := 0
	ast.Inspect(node, func(candidate ast.Node) bool {
		branch, ok := candidate.(*ast.IfStmt)
		if !ok || !selectorOnIdent(branch.Cond, result, "Committed") {
			return true
		}
		branchCount++
		committedBranch = branch
		return true
	})
	if branchCount != 1 || committedBranch.Pos() <= executePos {
		return false
	}

	branchDispatches := namedCallsInNode(committedBranch.Body, "dispatchShadowAfterCommit")
	if len(branchDispatches) != 1 || branchDispatches[0].Pos() != allDispatches[0].Pos() {
		return false
	}
	call := branchDispatches[0]
	if !selectorOnIdent(call.Fun, "p", "dispatchShadowAfterCommit") || len(call.Args) != 8 {
		return false
	}
	return identIs(call.Args[0], "runtime") &&
		identIs(call.Args[1], "proto") &&
		stringOfZeroArgReceiverCall(call.Args[2], "plan", "BackendProtocol") &&
		identIs(call.Args[3], "calledModel") &&
		identIs(call.Args[4], "exposed") &&
		identIs(call.Args[5], "t") &&
		identIs(call.Args[6], "requestID") &&
		selectorOnIdent(call.Args[7], result, "Commit")
}

func exactTargetExecutorAttempt(call *ast.CallExpr) (string, bool) {
	execute, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || execute.Sel.Name != "Execute" || len(call.Args) != 1 {
		return "", false
	}
	attempt, ok := call.Args[0].(*ast.Ident)
	if !ok {
		return "", false
	}
	factory, ok := execute.X.(*ast.CallExpr)
	if !ok || len(factory.Args) != 4 || !selectorOnIdent(factory.Fun, "p", "targetExecutor") {
		return "", false
	}
	if !zeroArgReceiverCall(factory.Args[0], attempt.Name, "Runtime") {
		return "", false
	}
	// The proxy-chain inputs threaded alongside the runtime must be the same
	// snapshot's Cfg/ParentOf plus a route/plan target's .Provider;
	// same-snapshot provenance is locked by executorRuntimeBoundToAttempt.
	cfg, ok := factory.Args[1].(*ast.SelectorExpr)
	if !ok || cfg.Sel.Name != "Cfg" || requestRoutingExprPath(cfg.X) == "" {
		return "", false
	}
	parentOf, ok := factory.Args[2].(*ast.SelectorExpr)
	if !ok || parentOf.Sel.Name != "ParentOf" || requestRoutingExprPath(parentOf.X) == "" {
		return "", false
	}
	provider, ok := factory.Args[3].(*ast.SelectorExpr)
	if !ok || provider.Sel.Name != "Provider" {
		return "", false
	}
	return attempt.Name, true
}

func shadowDispatchUsesCapturedRuntime(node ast.Node) bool {
	shadowRuntime, _, ok := uniquelyAssignedExpressionIdentifier(node, func(expr ast.Expr) bool {
		return selectorOnIdent(expr, "runtime", "Shadow")
	})
	if !ok || identifierAssignmentCount(node, shadowRuntime) != 1 ||
		selectorPathCount(node, "runtime.Shadow") != 1 ||
		len(callPathPositions(node, "p.shadow.Load")) != 0 {
		return false
	}
	for _, method := range []string{"ShouldSample", "TryAcquire"} {
		if namedCallCountInNode(node, method) != 1 || receiverCallCount(node, shadowRuntime, method) != 1 {
			return false
		}
	}
	permit, _, ok := uniquelyAssignedCallIdentifier(node, func(call *ast.CallExpr) bool {
		return selectorOnIdent(call.Fun, shadowRuntime, "TryAcquire")
	})
	if !ok || identifierAssignmentCount(node, permit) != 1 ||
		namedCallCountInNode(node, "Release") != 2 ||
		receiverCallCount(node, permit, "Release") != 2 {
		return false
	}

	runCalls := namedCallsInNode(node, "runShadow")
	if len(runCalls) != 1 {
		return false
	}
	run := runCalls[0]
	if !selectorOnIdent(run.Fun, "p", "runShadow") || len(run.Args) != 10 ||
		!identIs(run.Args[0], "runtime") || !identIs(run.Args[1], shadowRuntime) ||
		!identIs(run.Args[2], "stop") ||
		!identIs(run.Args[3], "proto") || !identIs(run.Args[4], "backendProto") ||
		!identIs(run.Args[5], "calledModel") || !identIs(run.Args[6], "exposed") ||
		!identIs(run.Args[7], "shadow") ||
		!zeroArgReceiverCall(run.Args[8], "commit", "RequestBody") ||
		!identIs(run.Args[9], "primaryRequestID") {
		return false
	}

	admissions := callsWithPath(node, "p.lifecycle.RunBeforeLogDrain")
	if len(admissions) != 1 || len(admissions[0].Args) != 1 {
		return false
	}
	callback, ok := admissions[0].Args[0].(*ast.FuncLit)
	return ok && len(namedCallsInNode(callback.Body, "runShadow")) == 1
}

func uniquelyAssignedCallIdentifier(
	node ast.Node,
	matches func(*ast.CallExpr) bool,
) (string, token.Pos, bool) {
	return uniquelyAssignedExpressionIdentifier(node, func(expr ast.Expr) bool {
		call, ok := expr.(*ast.CallExpr)
		return ok && matches(call)
	})
}

func uniquelyAssignedExpressionIdentifier(
	node ast.Node,
	matches func(ast.Expr) bool,
) (string, token.Pos, bool) {
	name := ""
	var position token.Pos
	count := 0
	ast.Inspect(node, func(candidate ast.Node) bool {
		assignment, ok := candidate.(*ast.AssignStmt)
		if !ok || len(assignment.Lhs) != len(assignment.Rhs) {
			return true
		}
		for index, rhs := range assignment.Rhs {
			if !matches(rhs) {
				continue
			}
			count++
			lhs, ok := assignment.Lhs[index].(*ast.Ident)
			if !ok || lhs.Name == "_" {
				continue
			}
			name = lhs.Name
			position = rhs.Pos()
		}
		return true
	})
	return name, position, count == 1 && name != ""
}

func identifierAssignmentCount(node ast.Node, name string) int {
	count := 0
	ast.Inspect(node, func(candidate ast.Node) bool {
		assignment, ok := candidate.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for _, lhs := range assignment.Lhs {
			if identIs(lhs, name) {
				count++
			}
		}
		return true
	})
	return count
}

func namedCallsInNode(node ast.Node, name string) []*ast.CallExpr {
	var calls []*ast.CallExpr
	ast.Inspect(node, func(candidate ast.Node) bool {
		call, ok := candidate.(*ast.CallExpr)
		if ok && callableName(call.Fun) == name {
			calls = append(calls, call)
		}
		return true
	})
	return calls
}

func callsWithPath(node ast.Node, path string) []*ast.CallExpr {
	var calls []*ast.CallExpr
	ast.Inspect(node, func(candidate ast.Node) bool {
		call, ok := candidate.(*ast.CallExpr)
		if ok && expressionPath(call.Fun) == path {
			calls = append(calls, call)
		}
		return true
	})
	return calls
}

func selectorPathCount(node ast.Node, path string) int {
	count := 0
	ast.Inspect(node, func(candidate ast.Node) bool {
		selector, ok := candidate.(*ast.SelectorExpr)
		if ok && expressionPath(selector) == path {
			count++
		}
		return true
	})
	return count
}

func selectorOnIdent(expr ast.Expr, receiver, field string) bool {
	selector, ok := expr.(*ast.SelectorExpr)
	return ok && selector.Sel.Name == field && identIs(selector.X, receiver)
}

func zeroArgReceiverCall(expr ast.Expr, receiver, method string) bool {
	call, ok := expr.(*ast.CallExpr)
	return ok && len(call.Args) == 0 && selectorOnIdent(call.Fun, receiver, method)
}

func stringOfZeroArgReceiverCall(expr ast.Expr, receiver, method string) bool {
	call, ok := expr.(*ast.CallExpr)
	return ok && identIs(call.Fun, "string") && len(call.Args) == 1 &&
		zeroArgReceiverCall(call.Args[0], receiver, method)
}

func packageLocalFunctionCallSites(t *testing.T, directory, function string) []interactionSite {
	t.Helper()
	var files []struct {
		path string
		file *ast.File
	}
	for _, path := range productionGoFilesIn(t, directory) {
		file, _ := parseGoFile(t, path)
		files = append(files, struct {
			path string
			file *ast.File
		}{path: filepath.ToSlash(path), file: file})
	}
	return rootCallSitesInFiles(files, function)
}

func packageLocalCompositeLiteralSites(t *testing.T, directory, typeName string) []interactionSite {
	t.Helper()
	var sites []interactionSite
	for _, path := range productionGoFilesIn(t, directory) {
		file, _ := parseGoFile(t, path)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(candidate ast.Node) bool {
				literal, ok := candidate.(*ast.CompositeLit)
				if ok && simpleTypeName(literal.Type) == typeName {
					sites = append(sites, interactionSite{
						file:     filepath.ToSlash(path),
						function: fn.Name.Name,
					})
				}
				return true
			})
		}
	}
	return sites
}

func importedTypeMethodCallSitesAcrossProduction(
	t *testing.T,
	importPath string,
	typeName string,
	method string,
) []interactionSite {
	t.Helper()
	var files []struct {
		path string
		file *ast.File
	}
	for _, path := range productionGoFilesRecursively(t, ".") {
		file, _ := parseGoFile(t, path)
		files = append(files, struct {
			path string
			file *ast.File
		}{path: filepath.ToSlash(path), file: file})
	}
	return importedTypeMethodCallSitesInFiles(files, importPath, typeName, method)
}

func importedTypeMethodCallSitesInFiles(
	files []struct {
		path string
		file *ast.File
	},
	importPath string,
	typeName string,
	method string,
) []interactionSite {
	var sites []interactionSite
	for _, input := range files {
		packageName := importedPackageName(input.file, importPath)
		if packageName == "" || packageName == "_" || packageName == "." {
			continue
		}
		for _, decl := range input.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			receivers := inferredImportedTypeIdentifiers(fn, packageName, typeName)
			for _, call := range namedCallsInNode(fn.Body, method) {
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if ok && importedTypeReceiver(selector.X, receivers, packageName, typeName) {
					sites = append(sites, interactionSite{file: input.path, function: fn.Name.Name})
				}
			}
		}
	}
	return sites
}

func importedTypeMethodReferenceSitesInFiles(
	files []struct {
		path string
		file *ast.File
	},
	importPath string,
	typeName string,
	method string,
) []interactionSite {
	var sites []interactionSite
	for _, input := range files {
		packageName := importedPackageName(input.file, importPath)
		if packageName == "" || packageName == "_" || packageName == "." {
			continue
		}
		for _, decl := range input.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			receivers := inferredImportedTypeIdentifiers(fn, packageName, typeName)
			ast.Inspect(fn.Body, func(candidate ast.Node) bool {
				selector, ok := candidate.(*ast.SelectorExpr)
				if ok && selector.Sel.Name == method &&
					importedTypeReceiver(selector.X, receivers, packageName, typeName) {
					sites = append(sites, interactionSite{file: input.path, function: fn.Name.Name})
				}
				return true
			})
		}
	}
	return sites
}

func importedTypeDeclarationSitesAcrossProduction(
	t *testing.T,
	importPath string,
	typeName string,
) []interactionSite {
	t.Helper()
	var files []struct {
		path string
		file *ast.File
	}
	for _, path := range productionGoFilesRecursively(t, ".") {
		file, _ := parseGoFile(t, path)
		files = append(files, struct {
			path string
			file *ast.File
		}{path: filepath.ToSlash(path), file: file})
	}
	return importedTypeDeclarationSitesInFiles(files, importPath, typeName)
}

// rootFunctionReferenceSites finds every bare reference to a package-level
// function and requires it to be the callee of a call expression. A function
// value such as `f := NewProxy; f(...)` is reported at the assignment site, so
// it cannot hide a second owner behind the direct-call owner checks.
func rootFunctionReferenceSites(t *testing.T, name string) []interactionSite {
	t.Helper()
	var files []struct {
		path string
		file *ast.File
	}
	for _, path := range productionGoFilesRecursively(t, ".") {
		file, _ := parseGoFile(t, path)
		files = append(files, struct {
			path string
			file *ast.File
		}{path: filepath.Base(path), file: file})
	}
	return rootFunctionReferenceSitesInFiles(files, name)
}

// importedFunctionReferenceSitesAcrossProduction finds every reference to an
// imported constructor and requires the direct `pkg.F(...)` shape: assignments
// of the function value are reported as their own sites and therefore break
// the single-owner assertion.
func importedFunctionReferenceSitesAcrossProduction(
	t *testing.T,
	importPath string,
	function string,
) []interactionSite {
	t.Helper()
	var files []struct {
		path string
		file *ast.File
	}
	for _, path := range productionGoFilesRecursively(t, ".") {
		file, _ := parseGoFile(t, path)
		files = append(files, struct {
			path string
			file *ast.File
		}{path: filepath.ToSlash(path), file: file})
	}
	return importedFunctionReferenceSitesInFiles(files, importPath, function)
}

// importedTypeValueReferenceSitesAcrossProduction finds imported type
// references in function bodies (composite literals, conversions, method
// expressions). Type-position uses in signatures are handled by the
// type-inference helpers; alias declarations have their own guard.
func importedTypeValueReferenceSitesAcrossProduction(
	t *testing.T,
	importPath string,
	typeName string,
) []interactionSite {
	t.Helper()
	var files []struct {
		path string
		file *ast.File
	}
	for _, path := range productionGoFilesRecursively(t, ".") {
		file, _ := parseGoFile(t, path)
		files = append(files, struct {
			path string
			file *ast.File
		}{path: filepath.ToSlash(path), file: file})
	}
	return importedTypeValueReferenceSitesInFiles(files, importPath, typeName)
}

// importedMethodExpressionSitesAcrossProduction reports `pkg.t.Method` /
// `(*pkg.t).method` expressions, which turn a reviewed method into an
// unreviewed function value.
func importedMethodExpressionSitesAcrossProduction(
	t *testing.T,
	importPath string,
	typeName string,
	method string,
) []interactionSite {
	t.Helper()
	var files []struct {
		path string
		file *ast.File
	}
	for _, path := range productionGoFilesRecursively(t, ".") {
		file, _ := parseGoFile(t, path)
		files = append(files, struct {
			path string
			file *ast.File
		}{path: filepath.ToSlash(path), file: file})
	}
	return importedMethodExpressionSitesInFiles(files, importPath, typeName, method)
}

// rootMethodValueReferenceSites reports same-package `receiver.method`
// selector expressions that are not the callee of a call expression. Direct
// calls are validated by their dedicated dataflow guards; this guard catches
// `fn := p.runShadow` / `fn := shadowRuntime.Execute` style escapes.
func rootMethodValueReferenceSites(t *testing.T, method string) []interactionSite {
	t.Helper()
	var sites []interactionSite
	for _, path := range productionGoFilesRecursively(t, ".") {
		file, _ := parseGoFile(t, path)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			sites = append(sites, methodValueReferenceSitesIn(fn, method, filepath.Base(path))...)
		}
	}
	return sites
}

func methodValueReferenceSitesIn(fn *ast.FuncDecl, method, path string) []interactionSite {
	var sites []interactionSite
	ast.Inspect(fn.Body, func(candidate ast.Node) bool {
		if candidate == nil {
			return true
		}
		if call, isCall := candidate.(*ast.CallExpr); isCall {
			if selector, isSelector := call.Fun.(*ast.SelectorExpr); isSelector && selector.Sel.Name == method {
				// Direct callee is legal; inspect its receiver and arguments, which
				// may still hide a method value.
				ast.Inspect(selector.X, func(child ast.Node) bool {
					if selector, ok := child.(*ast.SelectorExpr); ok && selector.Sel.Name == method {
						if _, receiverIsIdent := selector.X.(*ast.Ident); receiverIsIdent {
							sites = append(sites, interactionSite{file: path, function: fn.Name.Name})
						}
					}
					return true
				})
				for _, arg := range call.Args {
					ast.Inspect(arg, func(child ast.Node) bool {
						if selector, ok := child.(*ast.SelectorExpr); ok && selector.Sel.Name == method {
							if _, receiverIsIdent := selector.X.(*ast.Ident); receiverIsIdent {
								sites = append(sites, interactionSite{file: path, function: fn.Name.Name})
							}
						}
						return true
					})
				}
				return false
			}
			return true
		}
		if selector, isSelector := candidate.(*ast.SelectorExpr); isSelector && selector.Sel.Name == method {
			if _, receiverIsIdent := selector.X.(*ast.Ident); receiverIsIdent {
				sites = append(sites, interactionSite{file: path, function: fn.Name.Name})
			}
			return false
		}
		return true
	})
	return sites
}

func inferredImportedTypeIdentifiers(fn *ast.FuncDecl, packageName, typeName string) map[string]bool {
	identifiers := make(map[string]bool)
	addFieldsOfType(identifiers, fn.Type.Params, packageName, typeName)

	ast.Inspect(fn.Body, func(candidate ast.Node) bool {
		spec, ok := candidate.(*ast.ValueSpec)
		if !ok || !isImportedType(spec.Type, packageName, typeName) {
			return true
		}
		for _, name := range spec.Names {
			identifiers[name.Name] = true
		}
		return true
	})

	for changed := true; changed; {
		changed = false
		ast.Inspect(fn.Body, func(candidate ast.Node) bool {
			switch node := candidate.(type) {
			case *ast.AssignStmt:
				if len(node.Lhs) != len(node.Rhs) {
					return true
				}
				for index, rhs := range node.Rhs {
					lhs, ok := node.Lhs[index].(*ast.Ident)
					if ok && !identifiers[lhs.Name] &&
						importedTypeValue(rhs, identifiers, packageName, typeName) {
						identifiers[lhs.Name] = true
						changed = true
					}
				}
			case *ast.ValueSpec:
				if len(node.Names) != len(node.Values) {
					return true
				}
				for index, value := range node.Values {
					if !identifiers[node.Names[index].Name] &&
						importedTypeValue(value, identifiers, packageName, typeName) {
						identifiers[node.Names[index].Name] = true
						changed = true
					}
				}
			}
			return true
		})
	}
	return identifiers
}

func addFieldsOfType(names map[string]bool, fields *ast.FieldList, packageName, typeName string) {
	if fields == nil {
		return
	}
	for _, field := range fields.List {
		if !isImportedType(field.Type, packageName, typeName) {
			continue
		}
		for _, name := range field.Names {
			names[name.Name] = true
		}
	}
}

func isImportedType(expr ast.Expr, packageName, typeName string) bool {
	if expr == nil {
		return false
	}
	if pointer, ok := expr.(*ast.StarExpr); ok {
		expr = pointer.X
	}
	return selectorOnIdent(expr, packageName, typeName)
}

func importedTypeValue(expr ast.Expr, known map[string]bool, packageName, typeName string) bool {
	switch value := expr.(type) {
	case *ast.Ident:
		return known[value.Name]
	case *ast.CallExpr:
		return selectorOnIdent(value.Fun, packageName, "New"+typeName)
	default:
		return false
	}
}

func importedTypeReceiver(expr ast.Expr, known map[string]bool, packageName, typeName string) bool {
	return importedTypeValue(expr, known, packageName, typeName)
}

// TestArchitectureInteractionChecker proves the owner matchers reject an
// additional call/composite while ignoring comments. Without this control the
// production contracts could pass merely because their AST matcher regressed.
func TestArchitectureInteractionChecker(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "synthetic.go", `package main
import exec "model-proxy/internal/targetexec"
// NewProxy(cfg) and exec.Executor{} are comments, not call sites.
func owner() { NewProxy(cfg); _ = exec.Executor{} }
func bypass() { NewProxy(cfg); _ = exec.Executor{} }
func sameNameSelector() { factory.NewProxy(cfg) }
`, 0)
	if err != nil {
		t.Fatal(err)
	}

	calls := rootCallSitesInFiles([]struct {
		path string
		file *ast.File
	}{{path: "synthetic.go", file: file}}, "NewProxy")
	if len(calls) != 2 || calls[0].function != "owner" || calls[1].function != "bypass" {
		t.Errorf("root call positive control = %v, want owner and bypass only", calls)
	}
	if got := importedCompositeLiteralSitesInFiles([]struct {
		path string
		file *ast.File
	}{{path: "synthetic.go", file: file}}, "model-proxy/internal/targetexec", "Executor"); len(got) != 2 {
		t.Errorf("imported composite positive control = %v, want owner and bypass", got)
	}
	// Blank/dot-aliased imports must not contribute sites: without the alias
	// guard a blank import would make `_.Executor{}` match the local name.
	aliasSkipFile, err := parser.ParseFile(fset, "alias_skip.go", `package main
import (
	_ "model-proxy/internal/targetexec"
	. "model-proxy/internal/shadow"
)
func blankQualified() { _ = _.Executor{} }
func dotBare() { _ = Runtime{} }
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := importedCompositeLiteralSitesInFiles([]struct {
		path string
		file *ast.File
	}{{path: "alias_skip.go", file: aliasSkipFile}}, "model-proxy/internal/targetexec", "Executor"); len(got) != 0 {
		t.Errorf("imported composite alias control = %v, want no sites from blank/dot-aliased imports", got)
	}
	if methodFile, err := parser.ParseFile(fset, "methods.go", `package main
func h() { planner.Apply(); other.Apply() }
`, 0); err != nil {
		t.Fatal(err)
	} else if got := receiverCallCount(namedFunction(t, methodFile, "h").Body, "planner", "Apply"); got != 1 {
		t.Errorf("receiver call positive control = %d, want only planner.Apply", got)
	}

	plannerFile, err := parser.ParseFile(fset, "planner.go", `package main
func validPlanner() {
	route := requestRoutingPlanner()
	route.ApplyWithProfile()
	route.ContextOverflowRetryWithProfile()
}
func invalidPlanner() {
	route := requestRoutingPlanner()
	route.ApplyWithProfile()
	other.ContextOverflowRetryWithProfile()
}
func invalidPlannerFactory() {
	route := factory.requestRoutingPlanner()
	route.ApplyWithProfile()
	route.ContextOverflowRetryWithProfile()
}
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !constructedPlannerOwnsRoutingCalls(namedFunction(t, plannerFile, "validPlanner").Body) {
		t.Error("planner dataflow control rejected calls on the constructed planner")
	}
	if constructedPlannerOwnsRoutingCalls(namedFunction(t, plannerFile, "invalidPlanner").Body) {
		t.Error("planner dataflow control accepted a routing call on another receiver")
	}
	if constructedPlannerOwnsRoutingCalls(namedFunction(t, plannerFile, "invalidPlannerFactory").Body) {
		t.Error("planner dataflow control accepted a same-named selector factory")
	}

	commitFile, err := parser.ParseFile(fset, "commit.go", `package main
func validCommit() {
	attempt := makeAttempt()
	result := p.targetExecutor(attempt.Runtime(), runtime.Cfg, runtime.ParentOf, t.Provider).Execute(attempt)
	if result.Committed {
		p.dispatchShadowAfterCommit(runtime, proto, string(plan.BackendProtocol()), calledModel, exposed, t, requestID, result.Commit)
	}
}
func invalidCommit() {
	attempt := makeAttempt()
	result := p.targetExecutor(attempt.Runtime(), runtime.Cfg, runtime.ParentOf, t.Provider).Execute(attempt)
	if other.Committed {
		p.dispatchShadowAfterCommit(runtime, proto, string(plan.BackendProtocol()), calledModel, exposed, t, requestID, result.Commit)
	}
}
func invalidExecutorBinding() {
	attempt := makeAttempt()
	other := makeAttempt()
	result := p.targetExecutor(other.Runtime(), runtime.Cfg, runtime.ParentOf, t.Provider).Execute(attempt)
	if result.Committed {
		p.dispatchShadowAfterCommit(runtime, proto, string(plan.BackendProtocol()), calledModel, exposed, t, requestID, result.Commit)
	}
}
func invalidCommitPayload() {
	attempt := makeAttempt()
	result := p.targetExecutor(attempt.Runtime(), runtime.Cfg, runtime.ParentOf, t.Provider).Execute(attempt)
	if result.Committed {
		p.dispatchShadowAfterCommit(runtime, proto, string(plan.BackendProtocol()), calledModel, exposed, t, requestID, other.Commit)
	}
}
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !normalDeliveryShadowDispatchValid(namedFunction(t, commitFile, "validCommit").Body) {
		t.Error("post-commit dataflow control rejected the exact executor result")
	}
	if normalDeliveryShadowDispatchValid(namedFunction(t, commitFile, "invalidCommit").Body) {
		t.Error("post-commit dataflow control accepted dispatch under another result")
	}
	if normalDeliveryShadowDispatchValid(namedFunction(t, commitFile, "invalidExecutorBinding").Body) {
		t.Error("post-commit dataflow control accepted a mismatched executor runtime")
	}
	if normalDeliveryShadowDispatchValid(namedFunction(t, commitFile, "invalidCommitPayload").Body) {
		t.Error("post-commit dataflow control accepted another result's Commit")
	}

	shadowFile, err := parser.ParseFile(fset, "shadow.go", `package main
func validShadow() {
	shadowRuntime := runtime.Shadow
	shadowRuntime.ShouldSample()
	permit := shadowRuntime.TryAcquire()
	p.lifecycle.RunBeforeLogDrain(func(stop <-chan struct{}) {
		defer permit.Release()
		p.runShadow(runtime, shadowRuntime, stop, proto, backendProto, calledModel, exposed, shadow, commit.RequestBody(), primaryRequestID)
	})
	permit.Release()
}
func invalidShadow() {
	shadowRuntime := runtime.Shadow
	shadowRuntime.ShouldSample()
	other := p.shadow.Load()
	permit := other.TryAcquire()
	p.lifecycle.RunBeforeLogDrain(func(stop <-chan struct{}) {
		defer permit.Release()
		p.runShadow(runtime, other, stop, proto, backendProto, calledModel, exposed, shadow, commit.RequestBody(), primaryRequestID)
	})
	permit.Release()
}
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !shadowDispatchUsesCapturedRuntime(namedFunction(t, shadowFile, "validShadow").Body) {
		t.Error("Shadow runtime dataflow control rejected one captured generation")
	}
	if shadowDispatchUsesCapturedRuntime(namedFunction(t, shadowFile, "invalidShadow").Body) {
		t.Error("Shadow runtime dataflow control accepted a mid-dispatch runtime reload")
	}

	typedMethodFile, err := parser.ParseFile(fset, "typed_method.go", `package main
import shadowexec "model-proxy/internal/shadow"
func calls(runtime *shadowexec.Runtime, other otherRuntime) {
	runtime.Execute()
	other.Execute()
}
func constructs(other otherRuntime) {
	runtime := shadowexec.NewRuntime(options)
	runtime.Execute()
	other.Execute()
}
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	typedSites := importedTypeMethodCallSitesInFiles([]struct {
		path string
		file *ast.File
	}{{path: "typed_method.go", file: typedMethodFile}}, "model-proxy/internal/shadow", "Runtime", "Execute")
	if len(typedSites) != 2 || typedSites[0].function != "calls" || typedSites[1].function != "constructs" {
		t.Errorf("imported type method control = %v, want only typed and constructor-derived Runtime.Execute calls", typedSites)
	}

	bypassFile, err := parser.ParseFile(fset, "bypass.go", `package main
import (
	exec "model-proxy/internal/targetexec"
	shadowexec "model-proxy/internal/shadow"
)
type executorAlias = exec.Executor
func functionValue() { build := exec.NewPlan; _ = build(input) }
func directNewPlan() { _ = exec.NewPlan(input) }
func typeAlias() { _ = executorAlias{} }
func methodValue(runtime *shadowexec.Runtime) { run := runtime.Execute; _ = run }
func methodExpression() { _ = shadowexec.Runtime.Execute }
func directExecute(runtime *shadowexec.Runtime) { runtime.Execute(ctx, job) }
func methodValueRoot() { hook := p.runShadow; _ = hook }
func directRoot() { p.runShadow(runtime, shadowRuntime, proto, backendProto, calledModel, exposed, shadow, body, id) }
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	files := []struct {
		path string
		file *ast.File
	}{{path: "bypass.go", file: bypassFile}}

	if got := importedFunctionReferenceSitesInFiles(files, "model-proxy/internal/targetexec", "NewPlan"); len(got) != 2 ||
		got[0].function != "functionValue" || got[1].function != "directNewPlan" {
		t.Errorf("imported function reference control = %v, want functionValue alias plus directNewPlan", got)
	}
	if got := importedTypeValueReferenceSitesInFiles(files, "model-proxy/internal/targetexec", "Executor"); len(got) != 0 {
		t.Errorf("imported type value control = %v, want no direct exec.Executor body reference", got)
	}
	if got := importedTypeDeclarationSitesInFiles(files, "model-proxy/internal/targetexec", "Executor"); len(got) != 1 ||
		got[0].function != "type executorAlias" {
		t.Errorf("imported type alias control = %v, want type executorAlias", got)
	}
	if got := importedTypeMethodReferenceSitesInFiles(files, "model-proxy/internal/shadow", "Runtime", "Execute"); len(got) != 2 ||
		got[0].function != "methodValue" || got[1].function != "directExecute" {
		t.Errorf("imported method reference control = %v, want methodValue and directExecute; method expression is guarded separately", got)
	}
	if got := importedMethodExpressionSitesInFiles(files, "model-proxy/internal/shadow", "Runtime", "Execute"); len(got) != 1 ||
		got[0].function != "methodExpression" {
		t.Errorf("imported method expression control = %v, want only methodExpression", got)
	}
	if got := rootFunctionReferenceSitesInFiles(files, "NewProxy"); len(got) != 0 {
		t.Errorf("root function reference control = %v, want none", got)
	}
	if got := methodValueReferenceSitesIn(namedFunction(t, bypassFile, "methodValueRoot"), "runShadow", "bypass.go"); len(got) != 1 {
		t.Errorf("root method value control = %v, want methodValueRoot", got)
	}
	if got := methodValueReferenceSitesIn(namedFunction(t, bypassFile, "directRoot"), "runShadow", "bypass.go"); len(got) != 0 {
		t.Errorf("root direct method control = %v, want none", got)
	}
}

func importedFunctionReferenceSitesInFiles(
	files []struct {
		path string
		file *ast.File
	},
	importPath string,
	function string,
) []interactionSite {
	var sites []interactionSite
	for _, input := range files {
		packageName := importedPackageName(input.file, importPath)
		if packageName == "" || packageName == "_" || packageName == "." {
			continue
		}
		for _, decl := range input.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(candidate ast.Node) bool {
				selector, ok := candidate.(*ast.SelectorExpr)
				if ok && selectorOnIdent(selector, packageName, function) {
					sites = append(sites, interactionSite{file: input.path, function: fn.Name.Name})
				}
				return true
			})
		}
	}
	return sites
}

func importedTypeValueReferenceSitesInFiles(
	files []struct {
		path string
		file *ast.File
	},
	importPath string,
	typeName string,
) []interactionSite {
	var sites []interactionSite
	for _, input := range files {
		packageName := importedPackageName(input.file, importPath)
		if packageName == "" || packageName == "_" || packageName == "." {
			continue
		}
		for _, decl := range input.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(candidate ast.Node) bool {
				selector, ok := candidate.(*ast.SelectorExpr)
				if ok && selectorOnIdent(selector, packageName, typeName) {
					sites = append(sites, interactionSite{file: input.path, function: fn.Name.Name})
				}
				return true
			})
		}
	}
	return sites
}

func importedTypeDeclarationSitesInFiles(
	files []struct {
		path string
		file *ast.File
	},
	importPath string,
	typeName string,
) []interactionSite {
	var sites []interactionSite
	for _, input := range files {
		packageName := importedPackageName(input.file, importPath)
		if packageName == "" || packageName == "_" || packageName == "." {
			continue
		}
		for _, decl := range input.file.Decls {
			generic, ok := decl.(*ast.GenDecl)
			if !ok || generic.Tok != token.TYPE {
				continue
			}
			for _, spec := range generic.Specs {
				typeSpec, ok := spec.(*ast.TypeSpec)
				if ok && isImportedType(typeSpec.Type, packageName, typeName) {
					sites = append(sites, interactionSite{file: input.path, function: "type " + typeSpec.Name.Name})
				}
			}
		}
	}
	return sites
}

func importedMethodExpressionSitesInFiles(
	files []struct {
		path string
		file *ast.File
	},
	importPath string,
	typeName string,
	method string,
) []interactionSite {
	var sites []interactionSite
	for _, input := range files {
		packageName := importedPackageName(input.file, importPath)
		if packageName == "" || packageName == "_" || packageName == "." {
			continue
		}
		for _, decl := range input.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(candidate ast.Node) bool {
				selector, ok := candidate.(*ast.SelectorExpr)
				if !ok || selector.Sel.Name != method {
					return true
				}
				receiver := selector.X
				if paren, isParen := receiver.(*ast.ParenExpr); isParen {
					receiver = paren.X
				}
				if star, isStar := receiver.(*ast.StarExpr); isStar {
					receiver = star.X
				}
				if selectorOnIdent(receiver, packageName, typeName) {
					sites = append(sites, interactionSite{file: input.path, function: fn.Name.Name})
				}
				return true
			})
		}
	}
	return sites
}

func rootFunctionReferenceSitesInFiles(
	files []struct {
		path string
		file *ast.File
	},
	name string,
) []interactionSite {
	var sites []interactionSite
	for _, input := range files {
		for _, decl := range input.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(candidate ast.Node) bool {
				ident, ok := candidate.(*ast.Ident)
				if ok && ident.Name == name {
					sites = append(sites, interactionSite{file: input.path, function: fn.Name.Name})
				}
				return true
			})
		}
	}
	return sites
}

func rootCallSitesInFiles(files []struct {
	path string
	file *ast.File
}, name string) []interactionSite {
	var sites []interactionSite
	for _, input := range files {
		for _, decl := range input.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				if node == nil {
					return true
				}
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				if identIs(call.Fun, name) {
					sites = append(sites, interactionSite{file: input.path, function: fn.Name.Name})
				}
				return true
			})
		}
	}
	return sites
}

func importedCompositeLiteralSitesInFiles(files []struct {
	path string
	file *ast.File
}, importPath, typeName string) []interactionSite {
	var sites []interactionSite
	for _, input := range files {
		localName := importedPackageName(input.file, importPath)
		if localName == "" || localName == "_" || localName == "." {
			continue
		}
		for _, decl := range input.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				if node == nil {
					return true
				}
				literal, ok := node.(*ast.CompositeLit)
				if !ok {
					return true
				}
				selector, selectorOK := literal.Type.(*ast.SelectorExpr)
				if !selectorOK {
					return true
				}
				base, baseOK := selector.X.(*ast.Ident)
				if baseOK && base.Name == localName && selector.Sel.Name == typeName {
					sites = append(sites, interactionSite{file: input.path, function: fn.Name.Name})
				}
				return true
			})
		}
	}
	return sites
}

// selectorCompositeCountInNode counts <pkg>.<Type>{...} composite literals under n.
func selectorCompositeCountInNode(n ast.Node, pkg, typ string) int {
	count := 0
	ast.Inspect(n, func(node ast.Node) bool {
		lit, ok := node.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != typ {
			return true
		}
		if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == pkg {
			count++
		}
		return true
	})
	return count
}
