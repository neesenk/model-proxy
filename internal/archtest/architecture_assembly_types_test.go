package archtest

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/build"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

// Check active files first. If a candidate has a platform build tag, check
// that package in its target build, without silently dropping the reference.
func checkAssemblyPackages(t *testing.T, pending map[string]bool) {
	t.Helper()
	for _, target := range [][2]string{{runtime.GOOS, runtime.GOARCH}, {"linux", "amd64"}, {"windows", "amd64"}, {"darwin", "arm64"}, {"linux", "arm64"}, {"darwin", "amd64"}, {"windows", "arm64"}} {
		if len(pending) == 0 {
			return
		}
		dirs := map[string]string{}
		buildCtx := build.Default
		buildCtx.GOOS, buildCtx.GOARCH = target[0], target[1]
		for path := range pending {
			dir := filepath.Dir(path)
			active, err := buildCtx.MatchFile(repoRooted(t, dir), filepath.Base(path))
			if err != nil {
				t.Fatal(err)
			}
			if active {
				dirs[repoRooted(t, dir)] = "./" + filepath.ToSlash(dir)
			}
		}
		if len(dirs) == 0 {
			continue
		}
		args := append([]string{"list", "-deps", "-export", "-compiled", "-json=ImportPath,Dir,CompiledGoFiles,Export"}, slices.Sorted(maps.Values(dirs))...)
		cmd := exec.Command("go", args...)
		cmd.Dir = repoRoot(t)
		cmd.Env = append(cmd.Environ(), "GOOS="+target[0], "GOARCH="+target[1], "GOPROXY=off")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("loading Go types for %v: %v: %s", target, err, stderr.Bytes())
		}
		type pkg struct {
			ImportPath, Dir, Export string
			CompiledGoFiles         []string
		}
		var checked []pkg
		exports := map[string]string{}
		dec := json.NewDecoder(bytes.NewReader(out))
		for dec.More() {
			var p pkg
			if err := dec.Decode(&p); err != nil {
				t.Fatal(err)
			}
			exports[p.ImportPath] = p.Export
			if _, wanted := dirs[p.Dir]; wanted {
				checked = append(checked, p)
			}
		}
		fset := token.NewFileSet()
		imports := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) { return os.Open(exports[path]) })
		for _, p := range checked {
			var files []*ast.File
			candidates := map[*ast.File]string{}
			for _, path := range p.CompiledGoFiles {
				if !filepath.IsAbs(path) {
					path = filepath.Join(p.Dir, path)
				}
				f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
				if err != nil {
					t.Fatal(err)
				}
				files = append(files, f)
				if rel := filepath.ToSlash(repoRootRel(t, path)); pending[rel] {
					candidates[f] = rel
				}
			}
			if len(candidates) == 0 {
				continue
			}
			info := &types.Info{Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{}, Types: map[ast.Expr]types.TypeAndValue{}}
			conf := types.Config{Importer: imports, Sizes: types.SizesFor("gc", target[1])}
			if _, err := conf.Check(p.ImportPath, fset, files, info); err != nil {
				t.Fatal(err)
			}
			for f, path := range candidates {
				var owner *ast.FuncDecl
				if path == "internal/app/runtime.go" {
					owner = namedFunction(t, f, "NewRuntime")
					t.Run("binding controls", func(t *testing.T) { checkAssemblyControls(t, imports) })
				}
				for _, v := range assemblyViolations(f, info, owner) {
					t.Errorf("%s: %s", path, v)
				}
				delete(pending, path)
			}
		}
	}
	if len(pending) > 0 {
		t.Errorf("startup references outside verified Go builds: %v", slices.Sorted(maps.Keys(pending)))
	}
}
