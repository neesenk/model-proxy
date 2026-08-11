package archtest

import (
	"go/ast"
	"strings"
	"testing"
)

// TestRequestRoutingPolicyArchitecture keeps request-derived policy stateless
// and leaf-owned while the root adapter remains the sole bridge to
// generation-gated scheduling.
func TestRequestRoutingPolicyArchitecture(t *testing.T) {
	t.Run("leaf dependencies and immutable planner surface", func(t *testing.T) {
		allowedImports := internalRepositoryImportPolicy()["model-proxy/internal/routing"]
		forbiddenBoundaryImports := map[string]bool{
			"net/http": true, "net/url": true, "os": true,
			"io": true, "database/sql": true,
		}
		for _, path := range productionGoFilesIn(t, "internal/routing") {
			file, fileSet := parseGoFile(t, path)
			for _, spec := range file.Imports {
				importPath := strings.Trim(spec.Path.Value, `"`)
				if forbiddenBoundaryImports[importPath] {
					t.Errorf("%s imports %s; I/O extraction belongs in the root adapter", path, importPath)
				}
			}
			if got := unexpectedRepositoryImports(file, allowedImports); len(got) != 0 {
				t.Errorf("%s imports outside routing leaves: %v", path, got)
			}
			if got := forbiddenIdentifierSites(file, fileSet, map[string]bool{
				"Proxy": true, "RuntimeSnapshot": true,
				"Executor": true, "Attempt": true,
			}); len(got) != 0 {
				t.Errorf("%s retained root/execution owner types: %v", path, got)
			}
		}

		requestFile, _ := parseGoFile(t, "internal/routing/request.Go")
		plannerFields := namedStructFields(t, requestFile, "Planner")
		want := map[string]bool{
			"config": true, "parentOf": true, "catalog": true,
			"expandedRoutes": true, "scheduler": true,
		}
		if got := structContractViolations(plannerFields, want, nil); len(got) != 0 {
			t.Errorf("routing.Planner contract violations: %v", got)
		}
		for field := range plannerFields {
			if ast.IsExported(field) {
				t.Errorf("routing.Planner field %q must remain private", field)
			}
		}
	})

	t.Run("root adapter freezes generation and is the only constructor", func(t *testing.T) {
		adapter, _ := parseGoFile(t, "internal/app/request_routing_adapter.go")
		schedulerFields := namedStructFields(t, adapter, "requestRoutingScheduler")
		want := map[string]bool{
			"proxy": true, "config": true, "parentOf": true,
			"routeKeys": true, "generation": true,
		}
		if got := structContractViolations(schedulerFields, want, nil); len(got) != 0 {
			t.Errorf("requestRoutingScheduler contract violations: %v", got)
		}
		schedule := namedMethod(t, adapter, "requestRoutingScheduler", "Schedule")
		if !scheduleCallUsesCapturedGeneration(schedule.Body) {
			t.Error("requestRoutingScheduler.Schedule must pass captured config/identity/generation to Proxy.schedule")
		}
		constructor := namedFunction(t, adapter, "requestRoutingPlanner")
		if got := namedCallCountInNode(constructor.Body, "NewPlanner"); got != 1 {
			t.Errorf("requestRoutingPlanner NewPlanner calls = %d, want 1", got)
		}
		if !plannerConstructorUsesRuntimeSnapshot(constructor.Body) {
			t.Error("requestRoutingPlanner must project one runtime snapshot into PlannerInput and requestRoutingScheduler")
		}

		constructorSites := importedFunctionReferenceSitesAcrossProduction(
			t,
			"model-proxy/internal/routing",
			"NewPlanner",
		)
		if len(constructorSites) != 1 ||
			constructorSites[0].file != "internal/app/request_routing_adapter.go" ||
			constructorSites[0].function != "requestRoutingPlanner" {
			t.Errorf("routing.NewPlanner production reference sites = %v, want only request_routing_adapter.go:requestRoutingPlanner direct call", constructorSites)
		}
		if sites := packageLocalFunctionCallSites(t, "internal/routing", "NewPlanner"); len(sites) != 0 {
			t.Errorf("routing package-local NewPlanner calls = %v, want none outside the root adapter", sites)
		}
	})

	t.Run("serveOnce reuses one planner for proactive and reactive routing", func(t *testing.T) {
		forbiddenRoot := map[string]bool{
			"applyRequestAwareRouting": true,
			"contextOverflowRetry":     true,
			"crossRoutePool":           true,
			"collectCrossRoute":        true,
			"modelFits":                true,
			"profileRequest":           true,
			"estimateInputTokens":      true,
			"imageOKForTarget":         true,
			"forceProvider":            true,
			"filterTargetsByProvider":  true,
			"requestHasTools":          true,
			"requestHasImage":          true,
			"targetCapabilities":       true,
			"lookupModelMeta":          true,
			"modelFitsRequest":         true,
			"hasCapability":            true,
			"supportsImage":            true,
			"decodeRune":               true,
			"isCJK":                    true,
			"isBase64Char":             true,
			"isBase64Run":              true,
		}
		for _, path := range productionGoFiles(t) {
			file, fileSet := parseGoFile(t, path)
			for _, declaration := range file.Decls {
				function, ok := declaration.(*ast.FuncDecl)
				if ok && forbiddenRoot[function.Name.Name] {
					t.Errorf("root request policy duplicate %s at %s", function.Name.Name, describe(fileSet, function, "function"))
				}
			}
		}

		forward, _ := parseGoFile(t, "internal/app/proxy_forward.go")
		serveOnce := namedMethod(t, forward, "Proxy", "serveOnce")
		for name, want := range map[string]int{
			"requestRoutingPlanner": 1,
			"Apply":                 1,
			"ContextOverflowRetry":  1,
		} {
			if got := namedCallCountInNode(serveOnce.Body, name); got != want {
				t.Errorf("Proxy.serveOnce %s calls = %d, want %d", name, got, want)
			}
		}
	})
}

func scheduleCallUsesCapturedGeneration(node ast.Node) bool {
	var matched bool
	ast.Inspect(node, func(candidate ast.Node) bool {
		call, ok := candidate.(*ast.CallExpr)
		if !ok || len(call.Args) != 7 {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "schedule" {
			return true
		}
		proxyField, ok := selector.X.(*ast.SelectorExpr)
		proxyOwner, ownerOK := proxyField.X.(*ast.Ident)
		if !ok || !ownerOK || proxyOwner.Name != "scheduler" || proxyField.Sel.Name != "proxy" {
			return true
		}
		wantFields := []string{
			"config", "parentOf", "", "", "", "routeKeys", "generation",
		}
		for index, want := range wantFields {
			if want == "" {
				continue
			}
			field, ok := call.Args[index].(*ast.SelectorExpr)
			owner, ownerOK := field.X.(*ast.Ident)
			if !ok || !ownerOK || owner.Name != "scheduler" || field.Sel.Name != want {
				return true
			}
		}
		for index, want := range map[int]string{2: "routeName", 3: "sessionKey", 4: "targets"} {
			argument, ok := call.Args[index].(*ast.Ident)
			if !ok || argument.Name != want {
				return true
			}
		}
		matched = true
		return false
	})
	return matched
}

func plannerConstructorUsesRuntimeSnapshot(node ast.Node) bool {
	var literals []*ast.CompositeLit
	ast.Inspect(node, func(candidate ast.Node) bool {
		literal, ok := candidate.(*ast.CompositeLit)
		if ok && callableName(literal.Type) == "PlannerInput" {
			literals = append(literals, literal)
		}
		return true
	})
	if len(literals) != 1 {
		return false
	}
	fields := requestRoutingCompositeFields(literals[0])
	if len(fields) != 5 ||
		requestRoutingExprPath(fields["Config"]) != "runtime.Cfg" ||
		requestRoutingExprPath(fields["ParentOf"]) != "runtime.ParentOf" ||
		requestRoutingExprPath(fields["Catalog"]) != "runtime.Catalog" ||
		requestRoutingExprPath(fields["ExpandedRoutes"]) != "runtime.ExpandedRoutes" {
		return false
	}
	scheduler, ok := fields["Scheduler"].(*ast.CompositeLit)
	if !ok || simpleTypeName(scheduler.Type) != "requestRoutingScheduler" {
		return false
	}
	schedulerFields := requestRoutingCompositeFields(scheduler)
	return len(schedulerFields) == 5 &&
		requestRoutingExprPath(schedulerFields["proxy"]) == "proxy" &&
		requestRoutingExprPath(schedulerFields["config"]) == "runtime.Cfg" &&
		requestRoutingExprPath(schedulerFields["parentOf"]) == "runtime.ParentOf" &&
		requestRoutingExprPath(schedulerFields["routeKeys"]) == "routeKeys" &&
		requestRoutingExprPath(schedulerFields["generation"]) == "runtime.Generation"
}

func requestRoutingCompositeFields(literal *ast.CompositeLit) map[string]ast.Expr {
	fields := make(map[string]ast.Expr, len(literal.Elts))
	for _, element := range literal.Elts {
		entry, ok := element.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, keyOK := entry.Key.(*ast.Ident)
		if keyOK {
			fields[key.Name] = entry.Value
		}
	}
	return fields
}

func requestRoutingExprPath(expression ast.Expr) string {
	switch expression := expression.(type) {
	case *ast.Ident:
		return expression.Name
	case *ast.SelectorExpr:
		base := requestRoutingExprPath(expression.X)
		if base == "" {
			return ""
		}
		return base + "." + expression.Sel.Name
	default:
		return ""
	}
}
