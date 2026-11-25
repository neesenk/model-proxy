package app

// r6_provider_key_migration_test.go pins the durability wiring of the
// quota_state.json key migration (R6 review B15). The parent-directory fsync
// after the atomic rename is a best-effort side effect with no portable
// behavioral observation, so the contract is stated on the exact production
// call site: once the rename has succeeded, the renamed file's directory is
// fsynced through the shared wirecap helper — the same template as the
// quota tracker persist. Without it a crash + reboot can lose the directory
// entry although the temp file was fsynced, silently undoing the migration
// and dropping the legacy bare-name quota snapshots at load.

import (
	"go/ast"
	"go/token"
	"go/types"
	"strconv"
	"strings"
	"testing"
)

// r6ImportAlias returns the identifier file uses for pkgPath ("" when the
// file does not import it).
func r6ImportAlias(file *ast.File, pkgPath string) string {
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || path != pkgPath {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return path[strings.LastIndex(path, "/")+1:]
	}
	return ""
}

// r6CallPositions lists where fn calls pkg.name, pkg being resolved through
// file's imports.
func r6CallPositions(file *ast.File, fn ast.Node, pkgPath, name string) []token.Pos {
	alias := r6ImportAlias(file, pkgPath)
	if alias == "" {
		return nil
	}
	var pos []token.Pos
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
			if x, ok := sel.X.(*ast.Ident); ok && x.Name == alias && sel.Sel.Name == name {
				pos = append(pos, call.Pos())
			}
		}
		return true
	})
	return pos
}

func TestR6RenameQuotaStateProviderKeys_FsyncsDirAfterRename(t *testing.T) {
	file, fset := parseGoFile(t, "provider_key_migration.go")
	var fn *ast.FuncDecl
	for _, decl := range file.Decls {
		if f, ok := decl.(*ast.FuncDecl); ok && f.Recv == nil && f.Name.Name == "renameQuotaStateProviderKeys" {
			fn = f
		}
	}
	if fn == nil {
		t.Fatal("renameQuotaStateProviderKeys not found in provider_key_migration.go")
	}

	const wirecap = "model-proxy/internal/runtime/wirecap"
	renames := r6CallPositions(file, fn.Body, "os", "Rename")
	if len(renames) != 1 {
		t.Fatalf("os.Rename calls in renameQuotaStateProviderKeys = %d, want exactly 1", len(renames))
	}
	fsyncs := r6CallPositions(file, fn.Body, wirecap, "SyncParentDir")
	if len(fsyncs) != 1 {
		t.Fatalf("SyncParentDir calls in renameQuotaStateProviderKeys = %d, want exactly 1: the rename's directory entry must be fsynced (tracker persist template)", len(fsyncs))
	}

	// The fsync is the statement right after the rename's error check: the
	// success path only (a failed rename removed the temp file and left
	// nothing durable to sync).
	renameStmt := -1
	for i, stmt := range fn.Body.List {
		if stmt.Pos() <= renames[0] && renames[0] <= stmt.End() {
			renameStmt = i
		}
	}
	if renameStmt < 0 || renameStmt+1 >= len(fn.Body.List) {
		t.Fatalf("os.Rename at %v is not followed by a statement of the function body", fset.Position(renames[0]))
	}
	next, ok := fn.Body.List[renameStmt+1].(*ast.ExprStmt)
	if !ok {
		t.Fatalf("statement after the os.Rename check is %T, want the SyncParentDir call", fn.Body.List[renameStmt+1])
	}
	call, ok := next.X.(*ast.CallExpr)
	if !ok || call.Pos() != fsyncs[0] {
		t.Fatalf("statement after the os.Rename check at %v is not the SyncParentDir call at %v",
			fset.Position(next.Pos()), fset.Position(fsyncs[0]))
	}
	// The argument is `dir` — the directory holding both the temp file and
	// the renamed file (os.CreateTemp(dir, …) / os.Rename(tmp, path)) — not
	// the file path: the directory entry is what needs to become durable.
	if len(call.Args) != 1 {
		t.Fatalf("SyncParentDir takes %d argument(s), want exactly dir", len(call.Args))
	}
	if arg, ok := call.Args[0].(*ast.Ident); !ok || arg.Name != "dir" {
		t.Fatalf("SyncParentDir argument is %s, want dir (the renamed file's directory)", types.ExprString(call.Args[0]))
	}
}
