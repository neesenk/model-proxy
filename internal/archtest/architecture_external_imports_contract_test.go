package archtest

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// externalImportAllowlist is the closed set of external module prefixes every
// file below internal/ may import. stdlib is always allowed; model-proxy
// internal edges are governed by the DAG contract. An import outside this set
// is a violation: extend the list deliberately, with a rationale per entry —
// this is the depguard role, in-repo, so a coding agent cannot silently grow
// the dependency graph (go.mod diffs are reviewable but easy to skim past).
//
// Scope mirrors the DAG contract: internal/ only (root main and scripts/
// harnesses stay out; their external imports are none today and would show
// up in go.mod review regardless).
var externalImportAllowlist = map[string]string{
	"github.com/bytedance/sonic":    "JSON hot path (requestWantsStream/ExtractModel/converter wire parsing)",
	"github.com/mattn/go-isatty":    "CLI color detection (internal/display)",
	"github.com/zalando/go-keyring": "OS keychain credential storage (internal/credstore)",
	"golang.org/x/sys":              "windows registry for keyring + platform syscalls",
	"gopkg.in/yaml.v3":              "config/takeover YAML parsing and surgical editing",
	"modernc.org/sqlite":            "pure-Go stats store (CGO_ENABLED=0 cross builds)",
}

// testOnlyImportPaths may only be imported from _test.go files — production
// code pulling in test synchronization helpers would couple production
// behavior to test-only machinery.
var testOnlyImportPaths = map[string]bool{
	"model-proxy/internal/testsyn": true,
}

// collectInternalGoFiles returns every .go file under internal/ grouped by
// package directory (production AND test files: test files are part of the
// module graph and can grow dependencies just as well).
func collectInternalGoFiles(root string) (map[string][]string, error) {
	files := make(map[string][]string)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "testdata", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".go") {
			return nil
		}
		rel, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = append(files[filepath.ToSlash(rel)], path)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

// externalImportViolations parses every file and reports imports outside the
// contract. File paths (not parsed contents) decide the test-only rule, the
// same way the compiler does.
func externalImportViolations(filesByDir map[string][]string) []string {
	var violations []string
	dirs := make([]string, 0, len(filesByDir))
	for dir := range filesByDir {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	for _, dir := range dirs {
		for _, file := range filesByDir[dir] {
			fset := token.NewFileSet()
			parsed, err := parser.ParseFile(fset, file, nil, parser.ImportsOnly)
			if err != nil {
				violations = append(violations, fmt.Sprintf("%s: parse: %v", file, err))
				continue
			}
			for _, spec := range parsed.Imports {
				path := strings.Trim(spec.Path.Value, `"`)
				if testOnlyImportPaths[path] && !strings.HasSuffix(file, "_test.go") {
					violations = append(violations, fmt.Sprintf("%s: production file imports test-only helper %s", file, path))
					continue
				}
				first := strings.SplitN(path, "/", 2)[0]
				if !strings.Contains(first, ".") || strings.HasPrefix(path, "model-proxy/") {
					continue // stdlib or repo-internal (DAG contract governs the latter)
				}
				if !hasAllowedExternalPrefix(path) {
					violations = append(violations, fmt.Sprintf("%s: import %s is outside the external allowlist — extend externalImportAllowlist deliberately with a rationale", file, path))
				}
			}
		}
	}
	return violations
}

func hasAllowedExternalPrefix(path string) bool {
	for prefix := range externalImportAllowlist {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

func TestArchitectureExternalImportContract(t *testing.T) {
	files, err := collectInternalGoFiles(repoRooted(t, "internal"))
	if err != nil {
		t.Fatalf("collect internal go files: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no internal packages discovered")
	}
	if violations := externalImportViolations(files); len(violations) != 0 {
		t.Fatalf("external import contract violations:\n%s", strings.Join(violations, "\n"))
	}
}

// TestArchitectureExternalImportContractControls: synthetic positive/negative
// controls proving the guard has teeth — it accepts the reviewed shapes and
// rejects banned imports and production use of test-only helpers.
func TestArchitectureExternalImportContractControls(t *testing.T) {
	good := map[string][]string{"pkg": {
		writeFileForContract(t, "good.go", `
package pkg
import (
	"bytes"
	"github.com/bytedance/sonic/ast"
	"gopkg.in/yaml.v3"
)
`),
		writeFileForContract(t, "good_test.go", `
package pkg
import "model-proxy/internal/testsyn"
`),
	}}
	if violations := externalImportViolations(good); len(violations) != 0 {
		t.Fatalf("clean imports flagged: %v", violations)
	}

	banned := map[string][]string{"pkg": {writeFileForContract(t, "banned.go", `
package pkg
import "github.com/spf13/cobra"
`)}}
	if violations := externalImportViolations(banned); len(violations) != 1 || !strings.Contains(violations[0], "cobra") {
		t.Fatalf("banned import not caught: %v", violations)
	}

	prodTestsyn := map[string][]string{"pkg": {writeFileForContract(t, "uses_helper.go", `
package pkg
import "model-proxy/internal/testsyn"
`)}}
	if violations := externalImportViolations(prodTestsyn); len(violations) != 1 || !strings.Contains(violations[0], "test-only") {
		t.Fatalf("production testsyn import not caught: %v", violations)
	}

	// allowlist entries must have a non-empty rationale comment.
	for prefix, why := range externalImportAllowlist {
		if strings.TrimSpace(why) == "" {
			t.Fatalf("allowlist entry %s has no rationale", prefix)
		}
	}
}

// writeFileForContract materializes source text on disk under the test's own
// temp dir with the given FILE NAME — the suffix decides the test-only rule.
func writeFileForContract(t *testing.T, name, src string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(file, []byte(src), 0o644); err != nil {
		t.Fatalf("write synthetic file: %v", err)
	}
	return file
}
