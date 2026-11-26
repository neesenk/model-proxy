package archtest

import (
	"go/ast"
	"testing"
)

// TestRateLimitPolicyArchitecture keeps 429 classification and horizon policy
// leaf-owned. The composition root (internal/app, the forward pipeline in
// internal/forward, plus the thin root package) may only adapt the precomputed
// decision to runtime state — the duplicate ban must cover internal/app and
// internal/forward, where the request path lives today.
func TestRateLimitPolicyArchitecture(t *testing.T) {
	forbiddenRoot := map[string]bool{
		"classify429": true, "parseResetHint": true, "hintDuration": true, "parseRateLimit": true,
		// The concurrency half of the classification is leaf-owned too.
		"parseConcurrencyLimit": true, "hasTimeHint": true, "parseTimedRateLimit": true,
	}
	for _, dir := range []string{".", "internal/app", "internal/forward"} {
		for _, path := range productionGoFilesIn(t, dir) {
			file, fileSet := parseGoFile(t, path)
			for _, declaration := range file.Decls {
				function, ok := declaration.(*ast.FuncDecl)
				if ok && forbiddenRoot[function.Name.Name] {
					t.Errorf("composition-root policy duplicate %s at %s", function.Name.Name, describe(fileSet, function, "function"))
				}
			}
		}
	}

	if parseCount := declarationCountIn(t, "internal/targetexec", "ParseRateLimit"); parseCount != 1 {
		t.Errorf("internal/targetexec must declare exactly one ParseRateLimit, got %d", parseCount)
	}
	// ParseRateLimitFor is the whole classification — ParseRateLimit is its
	// no-vantage-point delegate — and the concurrency reader exists once.
	for _, name := range []string{"ParseRateLimitFor", "parseConcurrencyLimit"} {
		if got := declarationCountIn(t, "internal/targetexec", name); got != 1 {
			t.Errorf("internal/targetexec must declare exactly one %s, got %d", name, got)
		}
	}
	// The executor classifies exactly once per 429, through the vantage-point
	// entry: a request that had company on its account is the only evidence
	// an unstated concurrency limit can be inferred from.
	execute := namedMethod(t, mustParseFile(t, "internal/targetexec/executor.go"), "Executor", "Execute").Body
	if got := namedCallCountInNode(execute, "ParseRateLimitFor"); got != 1 {
		t.Errorf("Executor.Execute ParseRateLimitFor calls = %d, want 1", got)
	}
	if got := namedCallCountInNode(execute, "ParseRateLimit"); got != 0 {
		t.Errorf("Executor.Execute ParseRateLimit calls = %d, want 0 (the vantage point would be lost)", got)
	}

	// The two seams that call an upstream without the executor — the
	// decisions selector leg and the guard adjudication exchange — classify
	// through the same vantage-point entry, exactly once and never through
	// the vantage-point-less delegate: a request that had company on its
	// account is the only evidence an unstated concurrency limit can be
	// inferred from.
	for _, seam := range []string{"internal/forward/decisions.go", "internal/app/model_call.go"} {
		file, fileSet := parseGoFile(t, seam)
		for _, entry := range []struct {
			name string
			want int
		}{{"ParseRateLimitFor", 1}, {"ParseRateLimit", 0}} {
			calls := importedFunctionCallSites(file, fileSet, seam, "model-proxy/internal/targetexec", entry.name)
			if len(calls) != entry.want {
				t.Errorf("%s %s call sites = %v, want exactly %d", seam, entry.name, calls, entry.want)
			}
		}
	}

	executor, _ := parseGoFile(t, "internal/targetexec/executor.go")
	if !rateLimitStatePortAcceptsDecision(executor) {
		t.Error("targetexec.State.RecordRateLimit must accept a precomputed RateLimitDecision")
	}
	if !statePortMethodAcceptsDecision(executor, "RecordConcurrencyLimit") {
		t.Error("targetexec.State.RecordConcurrencyLimit must accept a precomputed RateLimitDecision")
	}
}

// declarationCountIn counts top-level functions with the given name across a
// directory's production files.
func declarationCountIn(t *testing.T, dir, name string) int {
	t.Helper()
	count := 0
	for _, path := range productionGoFilesIn(t, dir) {
		file, _ := parseGoFile(t, path)
		for _, declaration := range file.Decls {
			if function, ok := declaration.(*ast.FuncDecl); ok && function.Name.Name == name {
				count++
			}
		}
	}
	return count
}

// statePortMethodAcceptsDecision reports whether targetexec.State declares the
// named method with a RateLimitDecision as its second parameter.
func statePortMethodAcceptsDecision(file *ast.File, name string) bool {
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
				if len(method.Names) != 1 || method.Names[0].Name != name {
					continue
				}
				function, ok := method.Type.(*ast.FuncType)
				return ok && function.Params != nil && len(function.Params.List) == 2 && typeContainsIdent(function.Params.List[1].Type, "RateLimitDecision")
			}
		}
	}
	return false
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
