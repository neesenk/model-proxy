package main

import (
	"go/ast"
	"testing"
)

// TestRateLimitPolicyArchitecture keeps 429 classification and horizon policy
// leaf-owned. Root may only adapt the precomputed decision to runtime state.
func TestRateLimitPolicyArchitecture(t *testing.T) {
	forbiddenRoot := map[string]bool{
		"classify429": true, "parseResetHint": true, "hintDuration": true, "parseRateLimit": true,
	}
	for _, path := range productionGoFilesIn(t, ".") {
		file, fileSet := parseGoFile(t, path)
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if ok && forbiddenRoot[function.Name.Name] {
				t.Errorf("root production policy duplicate %s at %s", function.Name.Name, describe(fileSet, function, "function"))
			}
		}
	}

	parseCount := 0
	for _, path := range productionGoFilesIn(t, "internal/targetexec") {
		file, _ := parseGoFile(t, path)
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if ok && function.Name.Name == "ParseRateLimit" {
				parseCount++
			}
		}
	}
	if parseCount != 1 {
		t.Errorf("internal/targetexec must declare exactly one ParseRateLimit, got %d", parseCount)
	}
	if got := namedCallCountInNode(namedMethod(t, mustParseFile(t, "internal/targetexec/executor.go"), "Executor", "Execute").Body, "ParseRateLimit"); got != 1 {
		t.Errorf("Executor.Execute ParseRateLimit calls = %d, want 1", got)
	}

	fusion, fusionSet := parseGoFile(t, "fusion.go")
	calls := importedFunctionCallSites(fusion, fusionSet, "fusion.go", "model-proxy/internal/targetexec", "ParseRateLimit")
	if len(calls) != 1 {
		t.Errorf("Fusion ParseRateLimit call sites = %v, want exactly one", calls)
	}

	executor, _ := parseGoFile(t, "internal/targetexec/executor.go")
	if !rateLimitStatePortAcceptsDecision(executor) {
		t.Error("targetexec.State.RecordRateLimit must accept a precomputed RateLimitDecision")
	}
}

func mustParseFile(t *testing.T, path string) *ast.File {
	t.Helper()
	file, _ := parseGoFile(t, path)
	return file
}

func rateLimitStatePortAcceptsDecision(file *ast.File) bool {
	for _, declaration := range file.Decls {
		general, ok := declaration.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range general.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok || typeSpec.Name.Name != "State" {
				continue
			}
			interfaceType, ok := typeSpec.Type.(*ast.InterfaceType)
			if !ok {
				return false
			}
			for _, method := range interfaceType.Methods.List {
				if len(method.Names) != 1 || method.Names[0].Name != "RecordRateLimit" {
					continue
				}
				function, ok := method.Type.(*ast.FuncType)
				return ok && function.Params != nil && len(function.Params.List) == 2 && typeContainsIdent(function.Params.List[1].Type, "RateLimitDecision")
			}
		}
	}
	return false
}
