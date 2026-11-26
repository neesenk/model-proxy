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
		for _, function := range []string{"NewProxy", "NewWebServer"} {
			sites := rootFunctionReferenceSites(t, function)
			if len(sites) != 1 || sites[0].file != "runtime.go" || sites[0].function != "NewRuntime" {
				t.Errorf("%s production reference sites = %v, want only app.NewRuntime", function, sites)
			}
		}

		serve, _ := parseGoFile(t, "cli_serve.go")
		if got := namedCallCount(serve, "newApplicationRuntime"); got != 1 {
			t.Errorf("cli_serve.go newApplicationRuntime calls = %d, want one serve handoff", got)
		}
	})

	t.Run("target executor is assembled only by its forward adapter", func(t *testing.T) {
		planSites := importedFunctionCallSitesAcrossProduction(t, "model-proxy/internal/targetexec", "NewPlan")
		if len(planSites) != 1 || planSites[0].file != "internal/forward/plan.go" || planSites[0].function != "PlanTarget" {
			t.Errorf("targetexec.NewPlan production call sites = %v, want only forward/plan.go:PlanTarget", planSites)
		}
		if sites := packageLocalFunctionCallSites(t, "internal/targetexec", "NewPlan"); len(sites) != 0 {
			t.Errorf("targetexec package-local NewPlan calls = %v, want none outside the forward plan owner", sites)
		}
		if sites := importedFunctionReferenceSitesAcrossProduction(t, "model-proxy/internal/targetexec", "NewPlan"); len(sites) != 1 ||
			sites[0].file != "internal/forward/plan.go" || sites[0].function != "PlanTarget" {
			t.Errorf("targetexec.NewPlan production reference sites = %v, want only forward/plan.go:PlanTarget direct call", sites)
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
			"newTargetAttempt": true, "forwardLogCtx": true, "buildRequestLogInput": true,
			"targetPlanInput": true, "PlanTarget": true, "targetExecutor": true,
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
		// binding and vice versa. The uniform binding is ProcessCommand
		// (stream-agnostic handlers); streamBoundCommands may instead bind the
		// handler DIRECTLY — the only sanctioned shape for handlers that
		// consume the injected streams (bundled stdio MCP servers, whose
		// transport IS the stream pair — ProcessCommand would discard it).
		// Extending that allowlist is an architectural decision, made here
		// consciously rather than by silent drift.
		streamBoundCommands := map[string]bool{"mcp-server": true}
		registryEntries := 0
		directBindings := 0
		var unbound []string
		ast.Inspect(constructor.Body, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok || len(lit.Elts) == 0 {
				return true
			}
			mt, ok := lit.Type.(*ast.MapType)
			if !ok {
				return true
			}
			if ident, ok := mt.Value.(*ast.Ident); !ok || ident.Name != "Command" {
				return true
			}
			registryEntries = len(lit.Elts)
			for _, elt := range lit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					unbound = append(unbound, "<unnamed>")
					continue
				}
				name := ""
				if key, ok := kv.Key.(*ast.BasicLit); ok {
					name = strings.Trim(key.Value, `"`)
				}
				if call, ok := kv.Value.(*ast.CallExpr); ok {
					if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "ProcessCommand" {
						continue // the uniform binding
					}
				}
				if streamBoundCommands[name] {
					directBindings++
					continue
				}
				unbound = append(unbound, name)
			}
			return true
		})
		if registryEntries == 0 {
			t.Fatal("no Commands map literal found in NewApplication — registry-count contract is blind")
		}
		if got := namedCallCountInNode(constructor.Body, "ProcessCommand"); got != registryEntries-directBindings {
			t.Errorf("newApplication: %d ProcessCommand registrations for %d Commands registry entries (%d direct stream bindings) — every registered command needs a binding and vice versa", got, registryEntries, directBindings)
		}
		if len(unbound) > 0 {
			t.Errorf("newApplication: registry entries %v lack a sanctioned binding — use ProcessCommand, or extend streamBoundCommands deliberately for stream-consuming handlers", unbound)
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

func selectorOnIdent(expr ast.Expr, receiver, field string) bool {
	selector, ok := expr.(*ast.SelectorExpr)
	return ok && selector.Sel.Name == field && identIs(selector.X, receiver)
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
	. "example.com/worker"
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

	typedMethodFile, err := parser.ParseFile(fset, "typed_method.go", `package main
import workerexec "example.com/worker"
func calls(runtime *workerexec.Runtime, other otherRuntime) {
	runtime.Execute()
	other.Execute()
}
func constructs(other otherRuntime) {
	runtime := workerexec.NewRuntime(options)
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
	}{{path: "typed_method.go", file: typedMethodFile}}, "example.com/worker", "Runtime", "Execute")
	if len(typedSites) != 2 || typedSites[0].function != "calls" || typedSites[1].function != "constructs" {
		t.Errorf("imported type method control = %v, want only typed and constructor-derived Runtime.Execute calls", typedSites)
	}

	bypassFile, err := parser.ParseFile(fset, "bypass.go", `package main
import (
	exec "model-proxy/internal/targetexec"
	workerexec "example.com/worker"
)
type executorAlias = exec.Executor
func functionValue() { build := exec.NewPlan; _ = build(input) }
func directNewPlan() { _ = exec.NewPlan(input) }
func typeAlias() { _ = executorAlias{} }
func methodValue(runtime *workerexec.Runtime) { run := runtime.Execute; _ = run }
func methodExpression() { _ = workerexec.Runtime.Execute }
func directExecute(runtime *workerexec.Runtime) { runtime.Execute(ctx, job) }
func methodValueRoot() { hook := p.runDetached; _ = hook }
func directRoot() { p.runDetached(runtime, workerRuntime, proto, backendProto, calledModel, exposed, shadow, body, id) }
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
	if got := importedTypeMethodReferenceSitesInFiles(files, "example.com/worker", "Runtime", "Execute"); len(got) != 2 ||
		got[0].function != "methodValue" || got[1].function != "directExecute" {
		t.Errorf("imported method reference control = %v, want methodValue and directExecute; method expression is guarded separately", got)
	}
	if got := importedMethodExpressionSitesInFiles(files, "example.com/worker", "Runtime", "Execute"); len(got) != 1 ||
		got[0].function != "methodExpression" {
		t.Errorf("imported method expression control = %v, want only methodExpression", got)
	}
	if got := rootFunctionReferenceSitesInFiles(files, "NewProxy"); len(got) != 0 {
		t.Errorf("root function reference control = %v, want none", got)
	}
	if got := methodValueReferenceSitesIn(namedFunction(t, bypassFile, "methodValueRoot"), "runDetached", "bypass.go"); len(got) != 1 {
		t.Errorf("root method value control = %v, want methodValueRoot", got)
	}
	if got := methodValueReferenceSitesIn(namedFunction(t, bypassFile, "directRoot"), "runDetached", "bypass.go"); len(got) != 0 {
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
