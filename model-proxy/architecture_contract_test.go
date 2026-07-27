package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// Architecture boundary contracts, checked on the parsed AST (selector/call
// semantics) instead of string matching:
//   - web.go must not touch Proxy internals directly — Web/API handlers read
//     runtime state through proxyReadView and mutate via explicit commands
//     (docs/architecture/overview.md「依赖规则」). Direct field chains like
//     `w.p.mu` are flagged, and so are one-hop aliases (`q := w.p; q.mu`),
//     which the old string scan could not see.
//   - fusion.go must not call provider/protocol/conversion helpers directly —
//     Fusion shares targetPlan with normal routes instead of duplicating
//     provider lookup, protocol selection or request conversion.
//   - daemon.go must not start Proxy-level background tasks directly — they
//     are owned by proxyLifecycle (startRuntimeServices / Proxy.Close).
//
// Being AST-based, comments and string literals can no longer false-positive,
// and only actual selector/call expressions are judged. Known limits (accepted,
// documented so future readers don't mistake this for a proof): there is no
// type resolution, so an alias obtained other than by direct assignment
// (function return `q := getP(w)`, parameter, struct field) is not tracked,
// and same-named fields/methods on unrelated types would also match (none
// exist today — the rules below encode exactly the tokens the old string test
// forbade).
func TestArchitectureBoundaries(t *testing.T) {
	t.Run("web.go reads Proxy internals only via narrow ports", func(t *testing.T) {
		f, fset := parseGoFile(t, "web.go")
		// Fields/methods of *Proxy that web.go must never select directly.
		forbiddenFields := map[string]bool{
			"mu": true, "healthMu": true, "cfg": true, "providers": true,
			"health": true, "modelLocks": true, "snapshotConfig": true,
		}
		aliases := directAliases(f, "p") // idents assigned `x := <any>.p`
		for _, v := range forbiddenFieldAccesses(f, fset, forbiddenFields, aliases) {
			t.Errorf("web.go bypasses proxyReadView: %s", v)
		}
	})

	t.Run("fusion.go reuses targetPlan helpers instead of duplicating them", func(t *testing.T) {
		f, fset := parseGoFile(t, "fusion.go")
		forbiddenCalls := map[string]bool{
			"providerConfig": true, "resolvedBackendProto": true,
			"convertRequestFor": true, "catalogSnapshot": true,
		}
		for _, v := range forbiddenCallSites(f, fset, forbiddenCalls, nil) {
			t.Errorf("fusion.go bypasses targetPlan: %s", v)
		}
	})

	t.Run("daemon.go leaves Proxy background tasks to proxyLifecycle", func(t *testing.T) {
		f, fset := parseGoFile(t, "daemon.go")
		forbiddenCalls := map[string]bool{
			"initStats": true, "initRequestLog": true, "statsFlushLoop": true,
		}
		// Chained internal components: <x>.reqLog.loop(), <x>.flusher.flush().
		forbiddenChains := [][2]string{
			{"reqLog", "loop"}, {"reqLog", "shutdown"}, {"flusher", "flush"},
		}
		for _, v := range forbiddenCallSites(f, fset, forbiddenCalls, forbiddenChains) {
			t.Errorf("daemon.go bypasses proxyLifecycle: %s", v)
		}
	})
}

// TestArchitectureBoundaryChecker is the positive control for the AST rules
// above: the real checks pass vacuously when the tree is clean, so feed the
// checkers synthetic sources that violate each rule (including the `q := w.p`
// alias the old string test missed, and a comment-only mention that must NOT
// fire).
func TestArchitectureBoundaryChecker(t *testing.T) {
	parse := func(src string) (*ast.File, *token.FileSet) {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "synthetic.go", "package main\n"+src, 0)
		if err != nil {
			t.Fatal(err)
		}
		return f, fset
	}

	// w.p.mu: comment must not fire; direct chain and alias must.
	f, fset := parse(`func h() {
	// w.p.mu in a comment is fine
	q := w.p
	q.mu.RLock()
	w.p.health.Lock()
	_ = w.p.readView()
}`)
	fields := map[string]bool{"mu": true, "health": true}
	got := forbiddenFieldAccesses(f, fset, fields, directAliases(f, "p"))
	if len(got) != 2 {
		t.Errorf("field check: got %v, want exactly the q.mu and w.p.health accesses", got)
	}

	// Call rules: plain and receiver calls fire; comments don't.
	f, fset = parse(`func h() {
	// providerConfig( in a comment is fine
	providerConfig(x)
	p.convertRequestFor(y)
}`)
	gotCalls := forbiddenCallSites(f, fset, map[string]bool{"providerConfig": true, "convertRequestFor": true}, nil)
	if len(gotCalls) != 2 {
		t.Errorf("call check: got %v, want both call sites", gotCalls)
	}

	// Chained rules: p.reqLog.loop() fires, p.reqLog.flush() does not.
	f, fset = parse(`func h() {
	p.reqLog.loop(ctx)
	p.reqLog.flush()
}`)
	gotCalls = forbiddenCallSites(f, fset, nil, [][2]string{{"reqLog", "loop"}})
	if len(gotCalls) != 1 {
		t.Errorf("chain check: got %v, want only reqLog.loop", gotCalls)
	}
}

func parseGoFile(t *testing.T, path string) (*ast.File, *token.FileSet) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	return f, fset
}

// directAliases returns the idents bound to `.<field>` by direct assignment
// (`x := w.p`, `x = w.p`), propagated through ident-to-ident copies
// (`y := x`) to a fixpoint.
func directAliases(f *ast.File, field string) map[string]bool {
	aliases := map[string]bool{}
	for changed := true; changed; {
		changed = false
		ast.Inspect(f, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
				return true
			}
			lhs, ok := as.Lhs[0].(*ast.Ident)
			if !ok || aliases[lhs.Name] {
				return true
			}
			match := false
			switch rhs := as.Rhs[0].(type) {
			case *ast.SelectorExpr:
				match = rhs.Sel.Name == field
			case *ast.Ident:
				match = aliases[rhs.Name]
			}
			if match {
				aliases[lhs.Name] = true
				changed = true
			}
			return true
		})
	}
	return aliases
}

// forbiddenFieldAccesses reports selector expressions `<X>.<name>` where name
// is forbidden and X is either a `.<pField>` chain (`w.p.mu` — the base ident
// is irrelevant, any receiver's `.p` is a Proxy) or a direct alias of one
// (`q.mu` after `q := w.p`).
func forbiddenFieldAccesses(f *ast.File, fset *token.FileSet, forbidden map[string]bool, aliases map[string]bool) []string {
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || !forbidden[sel.Sel.Name] {
			return true
		}
		switch x := sel.X.(type) {
		case *ast.SelectorExpr:
			if x.Sel.Name == "p" {
				out = append(out, describe(fset, sel, fmt.Sprintf("%s.p.%s", exprName(x.X), sel.Sel.Name)))
			}
		case *ast.Ident:
			if aliases[x.Name] {
				out = append(out, describe(fset, sel, x.Name+"."+sel.Sel.Name))
			}
		}
		return true
	})
	return out
}

// forbiddenCallSites reports call expressions whose function name is in
// names (plain `f(...)` or any receiver's `x.f(...)`), plus chained internal
// component calls `<x>.<chain[0]>.<chain[1]>(...)`.
func forbiddenCallSites(f *ast.File, fset *token.FileSet, names map[string]bool, chains [][2]string) []string {
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			if names[fn.Name] {
				out = append(out, describe(fset, call, fn.Name+"(...)"))
			}
		case *ast.SelectorExpr:
			if names[fn.Sel.Name] {
				out = append(out, describe(fset, call, fn.Sel.Name+"(...)"))
			}
			if inner, ok := fn.X.(*ast.SelectorExpr); ok {
				for _, ch := range chains {
					if inner.Sel.Name == ch[0] && fn.Sel.Name == ch[1] {
						out = append(out, describe(fset, call, ch[0]+"."+ch[1]+"(...)"))
					}
				}
			}
		}
		return true
	})
	return out
}

func describe(fset *token.FileSet, n ast.Node, what string) string {
	return fmt.Sprintf("%s at %s", what, fset.Position(n.Pos()))
}

// exprName renders an identifier base (w) or falls back to a placeholder for
// composite expressions, only for readable violation messages.
func exprName(e ast.Expr) string {
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return strings.TrimSpace(fmt.Sprintf("%T", e))
}
