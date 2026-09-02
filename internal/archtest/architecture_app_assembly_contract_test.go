package archtest

import (
	"go/ast"
	"testing"
)

// app_assembly_test.go keeps the AST-level assembly contract on
// internal/app/runtime.go: NewRuntime must build exactly one Proxy, start its
// runtime services once, and assemble exactly one mux/Web server with the
// concrete Runtime literal wiring. The behavioral lifecycle/reload tests live
// in internal/app/runtime_assembly_test.go.

func TestApplicationRuntimeConcreteAssemblyContract(t *testing.T) {
	file, _ := parseGoFile(t, "internal/app/runtime.go")
	fields := namedStructFields(t, file, "Runtime")
	for _, field := range []string{"ConfigPath", "StartupConfig", "Proxy", "Handler", "TransportTasks"} {
		if _, ok := fields[field]; !ok {
			t.Errorf("applicationRuntime is missing concrete owner field %q", field)
		}
	}

	constructor := namedFunction(t, file, "NewRuntime")
	if got := namedCallCountInNode(constructor.Body, "NewProxy"); got != 1 {
		t.Errorf("newApplicationRuntime NewProxy calls = %d, want exactly 1", got)
	}
	if got := callCountOnIdentInNode(constructor.Body, "proxy", "StartRuntimeServices"); got != 1 {
		t.Errorf("newApplicationRuntime proxy.StartRuntimeServices calls = %d, want exactly 1", got)
	}
	if got := namedCallCountInNode(constructor.Body, "NewServeMux"); got != 1 {
		t.Errorf("newApplicationRuntime http.NewServeMux calls = %d, want exactly 1", got)
	}
	if got := namedCallCountInNode(constructor.Body, "NewWebServer"); got != 1 {
		t.Errorf("newApplicationRuntime NewWebServer calls = %d, want exactly 1 guarded Web assembly", got)
	}

	literals := 0
	ast.Inspect(constructor.Body, func(node ast.Node) bool {
		unary, ok := node.(*ast.UnaryExpr)
		if !ok || unary.Op.String() != "&" {
			return true
		}
		literal, ok := unary.X.(*ast.CompositeLit)
		if !ok || !identIs(literal.Type, "Runtime") {
			return true
		}
		literals++
		want := map[string]string{
			"ConfigPath":    "configPath",
			"StartupConfig": "cfg",
			"Proxy":         "proxy",
			"Handler":       "mux",
		}
		got := map[string]string{}
		for _, element := range literal.Elts {
			entry, ok := element.(*ast.KeyValueExpr)
			if !ok {
				t.Fatalf("applicationRuntime literal element = %T, want keyed field", element)
			}
			key, ok := entry.Key.(*ast.Ident)
			if !ok {
				t.Fatalf("applicationRuntime literal key = %T, want identifier", entry.Key)
			}
			got[key.Name] = expressionName(entry.Value)
		}
		for field, value := range want {
			if got[field] != value {
				t.Errorf("applicationRuntime.%s initializer = %q, want %q", field, got[field], value)
			}
		}
		return true
	})
	if literals != 1 {
		t.Errorf("newApplicationRuntime concrete literals = %d, want exactly 1", literals)
	}
}

func callCountOnIdentInNode(node ast.Node, receiver, method string) int {
	count := 0
	ast.Inspect(node, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != method || !identIs(selector.X, receiver) {
			return true
		}
		count++
		return true
	})
	return count
}
