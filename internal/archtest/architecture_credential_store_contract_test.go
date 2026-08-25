package archtest

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TestCredentialStoreKeychainImportContract pins the closed credential-storage
// boundary: only internal/credstore may import OS-keychain machinery
// (zalando/go-keyring and its backend drivers). Every other package must reach
// credential storage through credstore.Ref, so the storage backend stays
// swappable from one place (docs/research/design-s1-credential-keychain.md).
func TestCredentialStoreKeychainImportContract(t *testing.T) {
	const ownerDir = "internal/credstore"

	forbidden := map[string]bool{
		"github.com/zalando/go-keyring": true,
		"github.com/godbus/dbus/v5":     true,
		"github.com/danieljoos/wincred": true,
	}

	err := filepath.WalkDir(repoRooted(t, "internal"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		inOwner := strings.HasSuffix(filepath.ToSlash(filepath.Dir(path)), "/"+ownerDir)

		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Errorf("parse %s: %v", path, perr)
			return nil
		}
		for _, imp := range f.Imports {
			importPath := strings.Trim(imp.Path.Value, `"`)
			if forbidden[importPath] && !inOwner {
				t.Errorf("%s imports keychain package %q outside %s — route credential I/O through internal/credstore",
					filepath.Base(path), importPath, ownerDir)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
