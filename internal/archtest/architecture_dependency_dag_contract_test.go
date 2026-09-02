package archtest

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const internalImportPathPrefix = "model-proxy/internal/"

// internalRepositoryImportPolicy is the authoritative direct repository-import
// policy for every production package below internal. Entries must remain
// exhaustive: package discovery rejects both missing and stale classifications.
func internalRepositoryImportPolicy() map[string]map[string]bool {
	return map[string]map[string]bool{
		"model-proxy/internal/accounts": {"model-proxy/internal/credstore": true},
		"model-proxy/internal/admin": {
			"model-proxy/internal/accounts": true, "model-proxy/internal/appapi": true,
			"model-proxy/internal/cache": true, "model-proxy/internal/config": true,
			"model-proxy/internal/configedit": true, "model-proxy/internal/credstore": true,
			"model-proxy/internal/fusion": true, "model-proxy/internal/login": true,
			"model-proxy/internal/observe/counters": true, "model-proxy/internal/observe/logx": true,
			"model-proxy/internal/observe/seclog": true, "model-proxy/internal/observe/stats": true,
			"model-proxy/internal/presets": true, "model-proxy/internal/pricing": true,
			"model-proxy/internal/probe": true, "model-proxy/internal/provider": true,
			"model-proxy/internal/routing": true, "model-proxy/internal/runtime": true,
		},
		"model-proxy/internal/archtest":      nil,
		"model-proxy/internal/cache":         nil,
		"model-proxy/internal/catalog":       nil,
		"model-proxy/internal/cli/doctor":    {"model-proxy/internal/display": true, "model-proxy/internal/accounts": true, "model-proxy/internal/appapi": true, "model-proxy/internal/cli/clicommon": true, "model-proxy/internal/cli/framework": true, "model-proxy/internal/cli/models": true, "model-proxy/internal/config": true, "model-proxy/internal/credstore": true, "model-proxy/internal/routing": true, "model-proxy/internal/takeover": true, "model-proxy/internal/observe/seclog": true, "model-proxy/internal/provider": true},
		"model-proxy/internal/cli/serve":     {"model-proxy/internal/config": true, "model-proxy/internal/observe/logx": true},
		"model-proxy/internal/cli/framework": {"model-proxy/internal/accounts": true, "model-proxy/internal/config": true},
		"model-proxy/internal/cli/login":     {"model-proxy/internal/display": true, "model-proxy/internal/accounts": true, "model-proxy/internal/cli/framework": true, "model-proxy/internal/cli/serve": true, "model-proxy/internal/config": true, "model-proxy/internal/login": true, "model-proxy/internal/provider": true},
		"model-proxy/internal/cli/models":    {"model-proxy/internal/display": true, "model-proxy/internal/cli/serve": true, "model-proxy/internal/cli/framework": true, "model-proxy/internal/accounts": true, "model-proxy/internal/catalog": true, "model-proxy/internal/config": true, "model-proxy/internal/configedit": true, "model-proxy/internal/probe": true, "model-proxy/internal/provider": true, "model-proxy/internal/providerbuild": true, "model-proxy/internal/routing": true},
		"model-proxy/internal/cli": {
			"model-proxy/internal/display": true, "model-proxy/internal/cli/framework": true,
			"model-proxy/internal/cli/account": true, "model-proxy/internal/cli/admin": true,
			"model-proxy/internal/cli/audit": true, "model-proxy/internal/cli/config": true,
			"model-proxy/internal/cli/diag": true, "model-proxy/internal/cli/doctor": true,
			"model-proxy/internal/cli/login": true, "model-proxy/internal/cli/models": true,
			"model-proxy/internal/cli/presets": true, "model-proxy/internal/cli/stats": true,
			"model-proxy/internal/cli/status": true,
			"model-proxy/internal/config":     true, "model-proxy/internal/takeover": true,
			"model-proxy/internal/observe/logx": true,
		},
		"model-proxy/internal/cli/stats":     {"model-proxy/internal/accounts": true, "model-proxy/internal/cli/framework": true, "model-proxy/internal/config": true, "model-proxy/internal/daemonctl": true, "model-proxy/internal/display": true, "model-proxy/internal/observe/stats": true, "model-proxy/internal/providerbuild": true},
		"model-proxy/internal/cli/audit":     {"model-proxy/internal/cli/framework": true, "model-proxy/internal/config": true, "model-proxy/internal/observe/seclog": true},
		"model-proxy/internal/cli/status":    {"model-proxy/internal/appapi": true, "model-proxy/internal/cli/clicommon": true, "model-proxy/internal/cli/framework": true, "model-proxy/internal/config": true, "model-proxy/internal/daemonctl": true, "model-proxy/internal/display": true, "model-proxy/internal/routing": true},
		"model-proxy/internal/cli/admin":     {"model-proxy/internal/cli/framework": true, "model-proxy/internal/config": true, "model-proxy/internal/daemonctl": true, "model-proxy/internal/display": true},
		"model-proxy/internal/cli/diag":      {"model-proxy/internal/cli/framework": true, "model-proxy/internal/cli/models": true, "model-proxy/internal/config": true, "model-proxy/internal/daemonctl": true, "model-proxy/internal/display": true, "model-proxy/internal/observe/requestlog": true, "model-proxy/internal/provider": true},
		"model-proxy/internal/cli/config":    {"model-proxy/internal/accounts": true, "model-proxy/internal/cli/framework": true, "model-proxy/internal/config": true, "model-proxy/internal/display": true, "model-proxy/internal/provider": true, "model-proxy/internal/routing": true, "model-proxy/internal/takeover": true},
		"model-proxy/internal/cli/account":   {"model-proxy/internal/accounts": true, "model-proxy/internal/cli/framework": true, "model-proxy/internal/cli/serve": true, "model-proxy/internal/config": true, "model-proxy/internal/display": true, "model-proxy/internal/login": true, "model-proxy/internal/providerbuild": true},
		"model-proxy/internal/cli/clitest":   {"model-proxy/internal/accounts": true},
		"model-proxy/internal/cli/presets":   {"model-proxy/internal/cli/framework": true, "model-proxy/internal/cli/login": true, "model-proxy/internal/cli/serve": true, "model-proxy/internal/config": true, "model-proxy/internal/login": true, "model-proxy/internal/presets": true},
		"model-proxy/internal/cli/clicommon": {"model-proxy/internal/display": true, "model-proxy/internal/appapi": true, "model-proxy/internal/daemonctl": true, "model-proxy/internal/provider": true},
		"model-proxy/internal/config":        {"model-proxy/internal/catalog": true, "model-proxy/internal/pricing": true, "model-proxy/internal/protocol": true},
		"model-proxy/internal/configedit":    nil,
		"model-proxy/internal/credstore":     nil,
		"model-proxy/internal/daemonctl":     nil,
		"model-proxy/internal/display":       nil,
		"model-proxy/internal/fusion":        {"model-proxy/internal/config": true, "model-proxy/internal/observe/logx": true},
		"model-proxy/internal/forward": {
			"model-proxy/internal/cache": true, "model-proxy/internal/catalog": true,
			"model-proxy/internal/config": true, "model-proxy/internal/fusion": true,
			"model-proxy/internal/guard": true, "model-proxy/internal/guard/session": true,
			"model-proxy/internal/observe/counters": true, "model-proxy/internal/observe/events": true,
			"model-proxy/internal/observe/logx": true, "model-proxy/internal/observe/requestlog": true,
			"model-proxy/internal/observe/seclog": true,
			"model-proxy/internal/protocol":       true, "model-proxy/internal/provider": true,
			"model-proxy/internal/routing": true, "model-proxy/internal/shadow": true,
			"model-proxy/internal/targetexec": true,
		},
		"model-proxy/internal/guard":                 nil,
		"model-proxy/internal/guard/session":         {"model-proxy/internal/guard": true},
		"model-proxy/internal/login":                 {"model-proxy/internal/display": true, "model-proxy/internal/accounts": true, "model-proxy/internal/config": true, "model-proxy/internal/provider": true, "model-proxy/internal/observe/logx": true},
		"model-proxy/internal/observe/budget":        {"model-proxy/internal/config": true, "model-proxy/internal/observe/events": true, "model-proxy/internal/observe/stats": true, "model-proxy/internal/observe/logx": true, "model-proxy/internal/pricing": true},
		"model-proxy/internal/observe/counters":      nil,
		"model-proxy/internal/observe/events":        nil,
		"model-proxy/internal/observe/logx":          nil,
		"model-proxy/internal/observe/requestlog":    {"model-proxy/internal/config": true, "model-proxy/internal/observe/logx": true},
		"model-proxy/internal/observe/seclog":        {"model-proxy/internal/observe/logx": true},
		"model-proxy/internal/observe/stats":         {"model-proxy/internal/observe/counters": true, "model-proxy/internal/observe/logx": true},
		"model-proxy/internal/pricing":               nil,
		"model-proxy/internal/probe":                 {"model-proxy/internal/config": true, "model-proxy/internal/provider": true},
		"model-proxy/internal/protocol":              {"model-proxy/internal/observe/logx": true},
		"model-proxy/internal/presets":               {"model-proxy/internal/config": true, "model-proxy/internal/configedit": true, "model-proxy/internal/provider": true},
		"model-proxy/internal/provider":              {"model-proxy/internal/display": true, "model-proxy/internal/credstore": true},
		"model-proxy/internal/providerbuild":         {"model-proxy/internal/display": true, "model-proxy/internal/accounts": true, "model-proxy/internal/config": true, "model-proxy/internal/provider": true, "model-proxy/internal/observe/logx": true},
		"model-proxy/internal/routing":               {"model-proxy/internal/catalog": true, "model-proxy/internal/config": true, "model-proxy/internal/protocol": true, "model-proxy/internal/provider": true},
		"model-proxy/internal/runtime":               {"model-proxy/internal/config": true, "model-proxy/internal/runtime/wirecap": true, "model-proxy/internal/provider": true, "model-proxy/internal/observe/logx": true},
		"model-proxy/internal/runtime/wirecap":       {"model-proxy/internal/config": true, "model-proxy/internal/provider": true},
		"model-proxy/internal/shadow":                {"model-proxy/internal/targetexec": true, "model-proxy/internal/transport/bodycapture": true},
		"model-proxy/internal/takeover":              {"model-proxy/internal/catalog": true, "model-proxy/internal/config": true, "model-proxy/internal/routing": true, "model-proxy/internal/observe/logx": true},
		"model-proxy/internal/targetexec":            {"model-proxy/internal/cache": true, "model-proxy/internal/config": true, "model-proxy/internal/protocol": true, "model-proxy/internal/transport/bodycapture": true, "model-proxy/internal/provider": true, "model-proxy/internal/observe/logx": true},
		"model-proxy/internal/transport/bodycapture": nil,
		"model-proxy/internal/app": {
			"model-proxy/internal/accounts": true, "model-proxy/internal/admin": true, "model-proxy/internal/appapi": true,
			"model-proxy/internal/cache": true, "model-proxy/internal/catalog": true,
			"model-proxy/internal/config": true, "model-proxy/internal/configedit": true,
			"model-proxy/internal/forward": true,
			"model-proxy/internal/fusion":  true, "model-proxy/internal/guard": true, "model-proxy/internal/guard/session": true, "model-proxy/internal/httpx": true,
			"model-proxy/internal/login":            true,
			"model-proxy/internal/observe/counters": true, "model-proxy/internal/observe/events": true,
			"model-proxy/internal/observe/budget":     true,
			"model-proxy/internal/observe/logx":       true,
			"model-proxy/internal/observe/requestlog": true, "model-proxy/internal/observe/seclog": true, "model-proxy/internal/observe/stats": true,
			"model-proxy/internal/presets": true, "model-proxy/internal/pricing": true, "model-proxy/internal/probe": true,
			"model-proxy/internal/protocol": true, "model-proxy/internal/routing": true,
			"model-proxy/internal/runtime": true, "model-proxy/internal/runtime/wirecap": true,
			"model-proxy/internal/shadow": true, "model-proxy/internal/targetexec": true,
			"model-proxy/internal/transport/bodycapture": true, "model-proxy/internal/web": true, "model-proxy/internal/webauth": true,
			"model-proxy/internal/credstore": true, "model-proxy/internal/display": true, "model-proxy/internal/provider": true, "model-proxy/internal/providerbuild": true,
		},
		"model-proxy/internal/appapi":  {"model-proxy/internal/fusion": true, "model-proxy/internal/observe/stats": true, "model-proxy/internal/presets": true, "model-proxy/internal/pricing": true},
		"model-proxy/internal/httpx":   nil,
		"model-proxy/internal/web":     {"model-proxy/internal/appapi": true, "model-proxy/internal/observe/logx": true, "model-proxy/internal/observe/requestlog": true, "model-proxy/internal/observe/stats": true, "model-proxy/internal/pricing": true, "model-proxy/internal/webauth": true},
		"model-proxy/internal/webauth": nil,
	}
}

func assertInternalPackageImportPolicy(t *testing.T, directory string) {
	t.Helper()
	packagePath := filepath.ToSlash(filepath.Join("model-proxy", directory))
	allowed, ok := internalRepositoryImportPolicy()[packagePath]
	if !ok {
		t.Fatalf("%s has no internal repository-import policy classification", packagePath)
	}
	assertRepositoryPackageImports(t, directory, allowed)
}

func discoverProductionInternalPackages(root string) (map[string][]string, error) {
	packages := make(map[string][]string)
	testOnly := make(map[string][]string)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "testdata", "vendor":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".go") {
			return nil
		}
		directory := filepath.Dir(path)
		relative, err := filepath.Rel(root, directory)
		if err != nil {
			return err
		}
		packagePath := filepath.ToSlash(filepath.Join(internalImportPathPrefix, relative))
		// Test-only directories (e.g. internal/archtest) are still packages
		// that must be classified; their test files are import-checked.
		if strings.HasSuffix(entry.Name(), "_test.go") {
			testOnly[packagePath] = append(testOnly[packagePath], path)
			return nil
		}
		packages[packagePath] = append(packages[packagePath], path)
		return nil
	})
	if err != nil {
		return nil, err
	}
	for packagePath, paths := range testOnly {
		if _, ok := packages[packagePath]; !ok {
			packages[packagePath] = paths
		}
	}
	for packagePath := range packages {
		sort.Strings(packages[packagePath])
	}
	return packages, nil
}

func internalPolicyClassificationViolations(discovered map[string][]string, policy map[string]map[string]bool) []string {
	var violations []string
	for packagePath := range discovered {
		if _, ok := policy[packagePath]; !ok {
			violations = append(violations, "missing policy classification for "+packagePath)
		}
	}
	for packagePath := range policy {
		if _, ok := discovered[packagePath]; !ok {
			violations = append(violations, "stale policy classification for "+packagePath)
		}
	}
	return sortedNames(violations)
}

func internalPolicyUnknownTargetViolations(policy map[string]map[string]bool) []string {
	var violations []string
	for source, allowed := range policy {
		for target := range allowed {
			if strings.HasPrefix(target, internalImportPathPrefix) {
				if _, ok := policy[target]; !ok {
					violations = append(violations, fmt.Sprintf("%s allows unclassified internal package %s", source, target))
				}
			}
		}
	}
	return sortedNames(violations)
}

func internalPolicyCycles(policy map[string]map[string]bool) []string {
	const (
		unvisited = iota
		visiting
		visited
	)
	state := make(map[string]int, len(policy))
	var cycles []string
	var visit func(string, []string)
	visit = func(node string, trail []string) {
		state[node] = visiting
		trail = append(trail, node)
		for _, dependency := range sortedBoolNames(policy[node]) {
			if !strings.HasPrefix(dependency, internalImportPathPrefix) {
				continue
			}
			switch state[dependency] {
			case unvisited:
				visit(dependency, trail)
			case visiting:
				for index, ancestor := range trail {
					if ancestor == dependency {
						cycles = append(cycles, strings.Join(append(append([]string(nil), trail[index:]...), dependency), " -> "))
						break
					}
				}
			}
		}
		state[node] = visited
	}
	for _, node := range sortedPolicyNodes(policy) {
		if state[node] == unvisited {
			visit(node, nil)
		}
	}
	return sortedNames(cycles)
}

func sortedPolicyNodes(policy map[string]map[string]bool) []string {
	nodes := make([]string, 0, len(policy))
	for node := range policy {
		nodes = append(nodes, node)
	}
	sort.Strings(nodes)
	return nodes
}

func cloneBoolMap(values map[string]bool) map[string]bool {
	clone := make(map[string]bool, len(values))
	for value, present := range values {
		clone[value] = present
	}
	return clone
}

func repositoryImportAliasViolations(file *ast.File) []string {
	var violations []string
	for _, spec := range file.Imports {
		importPath := strings.Trim(spec.Path.Value, `"`)
		if !strings.HasPrefix(importPath, "model-proxy/") || spec.Name == nil {
			continue
		}
		if spec.Name.Name == "." || spec.Name.Name == "_" {
			violations = append(violations, spec.Name.Name+" "+importPath)
		}
	}
	return sortedNames(violations)
}

func TestArchitectureInternalDependencyDAG(t *testing.T) {
	policy := internalRepositoryImportPolicy()
	discovered, err := discoverProductionInternalPackages(repoRooted(t, "internal"))
	if err != nil {
		t.Fatalf("discover internal production packages: %v", err)
	}
	if got := internalPolicyClassificationViolations(discovered, policy); len(got) != 0 {
		t.Errorf("internal repository-import policy must classify every discovered package exactly once: %v", got)
	}
	if got := internalPolicyUnknownTargetViolations(policy); len(got) != 0 {
		t.Errorf("internal repository-import policy names unknown internal dependencies: %v", got)
	}
	if got := internalPolicyCycles(policy); len(got) != 0 {
		t.Errorf("internal repository-import policy must be acyclic: %v", got)
	}
	for packagePath, paths := range discovered {
		allowed := policy[packagePath]
		for _, path := range paths {
			file, _ := parseGoFile(t, path)
			if got := unexpectedRepositoryImports(file, allowed); len(got) != 0 {
				t.Errorf("%s imports repository packages outside %s policy: %v", path, packagePath, got)
			}
		}
	}
}

func TestArchitectureRepositoryImportsUseExplicitNames(t *testing.T) {
	for _, path := range productionGoFilesRecursively(t, ".") {
		file, _ := parseGoFile(t, path)
		if got := repositoryImportAliasViolations(file); len(got) != 0 {
			t.Errorf("%s has dot or blank repository imports that can bypass owner guards: %v", path, got)
		}
	}
}

func TestDiscoverProductionInternalPackagesIncludesNestedPackage(t *testing.T) {
	root := t.TempDir()
	packageDir := filepath.Join(root, "nested", "package")
	if err := os.MkdirAll(packageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packageDir, "package.go"), []byte("package sample\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packageDir, "package_test.go"), []byte("package sample\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	packages, err := discoverProductionInternalPackages(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(packages) != 1 {
		t.Fatalf("discovered packages = %v, want one production package", packages)
	}
	for _, paths := range packages {
		if len(paths) != 1 || filepath.Base(paths[0]) != "package.go" {
			t.Fatalf("discovered paths = %v, want only package.go", paths)
		}
	}
}

func TestDiscoverProductionInternalPackagesIncludesTestOnlyPackage(t *testing.T) {
	root := t.TempDir()
	packageDir := filepath.Join(root, "contracts")
	if err := os.MkdirAll(packageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packageDir, "contract_test.go"), []byte("package contracts\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	packages, err := discoverProductionInternalPackages(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(packages) != 1 {
		t.Fatalf("discovered packages = %v, want the test-only package", packages)
	}
	for _, paths := range packages {
		if len(paths) != 1 || filepath.Base(paths[0]) != "contract_test.go" {
			t.Fatalf("discovered paths = %v, want only contract_test.go", paths)
		}
	}
}

func TestUnexpectedRepositoryImportsReportsDisallowedEdge(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "positive_control.go", `package sample
import "model-proxy/internal/protocol"`, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := unexpectedRepositoryImports(file, map[string]bool{"model-proxy/internal/pricing": true})
	if len(got) != 1 || got[0] != "model-proxy/internal/protocol" {
		t.Errorf("unexpected repository imports = %v, want [model-proxy/internal/protocol]", got)
	}
}

func TestInternalPolicyCyclesReportsCycle(t *testing.T) {
	policy := map[string]map[string]bool{
		"model-proxy/internal/first":  {"model-proxy/internal/second": true},
		"model-proxy/internal/second": {"model-proxy/internal/first": true},
	}
	if got := internalPolicyCycles(policy); len(got) != 1 || got[0] != "model-proxy/internal/first -> model-proxy/internal/second -> model-proxy/internal/first" {
		t.Errorf("policy cycles = %v, want first -> second -> first", got)
	}
}

func TestRepositoryImportAliasViolationsReportsGuardBypass(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "positive_control.go", `package sample
import (
	. "model-proxy/internal/protocol"
	_ "model-proxy/internal/provider"
	alias "model-proxy/internal/config"
)
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := repositoryImportAliasViolations(file)
	want := []string{". model-proxy/internal/protocol", "_ model-proxy/internal/provider"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("repository alias violations = %v, want %v", got, want)
	}
}
