package webauth

// r6_resolve_scan_test.go — Resolve's contract is the one its comment states:
// it walks the whole token set exactly like Accept, so which-key attribution
// leaks no more timing than the gate itself (R6 B19). The verdicts are pinned
// by behavior; the walk itself is a property of the code's shape (an early
// return changes no observable output), so it is pinned on the AST.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

// TestR6ResolveMatchesAcceptVerdicts: Resolve answers a token exactly when
// Accept admits the presentation and "" exactly when it refuses — including
// the disabled, missing-file and no-presentation fail-closed cases.
func TestR6ResolveMatchesAcceptVerdicts(t *testing.T) {
	enabled := NewSource(writeTokenFile(t, t.TempDir(), "keys", "# team\nsk-one\nsk-two\nsk-dup\nsk-dup\n"))
	missing := NewSource(filepath.Join(t.TempDir(), "absent"))
	for _, source := range []*Source{enabled, missing, NewSource(), nil} {
		for _, presented := range []string{"sk-one", "sk-two", "sk-dup", "sk-on", "SK-ONE", "sk-unknown", ""} {
			got, accepted := Resolve(source, presented), source.Accept(presented)
			if accepted != (got != "") {
				t.Errorf("Resolve(%q) = %q while Accept = %v, want the same verdict", presented, got, accepted)
			}
			if accepted && got != presented {
				t.Errorf("Resolve(%q) = %q, want the configured token", presented, got)
			}
		}
	}
	if got := Resolve(enabled, "sk-dup"); got != "sk-dup" {
		t.Errorf("duplicate lines resolve to %q, want the token", got)
	}
}

// TestR6ResolveLoopWalksTheWholeSet: the token-set loop in Resolve must be
// straight-line — no return, break/continue or nested branch inside, and
// exactly one constant-time comparison per iteration. Then every candidate is
// compared whatever line hits and no instruction depends on which one did,
// which is the walk Accept performs.
func TestR6ResolveLoopWalksTheWholeSet(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "keyfile.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var resolve *ast.FuncDecl
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "Resolve" {
			resolve = fn
		}
	}
	if resolve == nil {
		t.Fatal("Resolve not found in keyfile.go")
	}
	var loops []*ast.RangeStmt
	ast.Inspect(resolve.Body, func(node ast.Node) bool {
		if loop, ok := node.(*ast.RangeStmt); ok {
			loops = append(loops, loop)
		}
		return true
	})
	if len(loops) != 1 {
		t.Fatalf("Resolve range loops = %d, want exactly 1 (the walk over the token set)", len(loops))
	}

	comparisons := 0
	ast.Inspect(loops[0].Body, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.ReturnStmt:
			t.Errorf("Resolve loop returns early at %s: a hit short-circuits the walk", fset.Position(typed.Pos()))
		case *ast.BranchStmt:
			t.Errorf("Resolve loop breaks/continues at %s: a hit short-circuits the walk", fset.Position(typed.Pos()))
		case *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt, *ast.SwitchStmt, *ast.SelectStmt:
			t.Errorf("Resolve loop body branches at %s: the match latch must stay branch-free", fset.Position(typed.Pos()))
		case *ast.CallExpr:
			// The comparison itself: subtle.ConstantTimeCompare, the only
			// constant-time comparison Resolve may run per iteration.
			if sel, ok := typed.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "ConstantTimeCompare" {
				comparisons++
			}
		}
		return true
	})
	if comparisons != 1 {
		t.Errorf("constant-time comparisons inside the Resolve loop = %d, want exactly 1 per iteration", comparisons)
	}
}
