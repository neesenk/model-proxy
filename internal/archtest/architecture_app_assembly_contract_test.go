package archtest

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"strings"
	"testing"
)

const appImport = "model-proxy/internal/app"

func TestApplicationRuntimeConcreteAssemblyContract(t *testing.T) {
	file, _ := parseGoPackage(t, "internal/app")
	fields := namedStructFields(t, file, "Runtime")
	for _, field := range []string{"ConfigPath", "StartupConfig", "Proxy", "Handler", "TransportTasks"} {
		if _, ok := fields[field]; !ok {
			t.Errorf("app.Runtime is missing owner field %s", field)
		}
	}

	// Include every production file with a selector of this name. Type-checking
	// disambiguates unrelated receivers, promoted methods and method expressions.
	sites := map[string]bool{"internal/app/runtime.go": true}
	for _, path := range productionGoFilesRecursively(t, ".") {
		f, _ := parseGoFile(t, path)
		for n := range ast.Preorder(f) {
			if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "StartRuntimeServices" {
				sites[filepath.ToSlash(path)] = true
				break
			}
		}
	}
	checkAssemblyPackages(t, sites)
}

// assemblyViolations binds the startup receiver and the returned fields to the
// constructor results and parameters by Go object identity.
func assemblyViolations(f *ast.File, info *types.Info, owner *ast.FuncDecl) []string {
	var problems []string
	var allowed *ast.SelectorExpr
	if owner != nil {
		sig := info.Defs[owner.Name].Type().(*types.Signature)
		if sig.Params().Len() < 2 {
			return []string{"NewRuntime lacks config and path parameters"}
		}
		cfg, path := sig.Params().At(0), sig.Params().At(1)
		bindings := map[string]types.Object{}
		writes := map[types.Object]int{}
		bind := func(lhs, rhs ast.Expr) {
			obj := assemblyObject(info, lhs)
			writes[obj]++
			call, ok := rhs.(*ast.CallExpr)
			if !ok {
				return
			}
			fn, ok := assemblyObject(info, call.Fun).(*types.Func)
			if !ok {
				return
			}
			switch fn.FullName() {
			case appImport + ".NewProxy", "net/http.NewServeMux":
				bindings[fn.Name()] = obj
				if fn.Name() == "NewProxy" && (len(call.Args) != 1 || assemblyObject(info, call.Args[0]) != cfg) {
					problems = append(problems, "NewProxy must use the startup config")
				}
			}
		}
		for n := range ast.Preorder(owner.Body) {
			switch n := n.(type) {
			case *ast.AssignStmt:
				for i, lhs := range n.Lhs {
					if i < len(n.Rhs) {
						bind(lhs, n.Rhs[i])
					} else {
						writes[assemblyObject(info, lhs)]++
					}
				}
			case *ast.ValueSpec:
				for i, rhs := range n.Values {
					if i < len(n.Names) {
						bind(n.Names[i], rhs)
					}
				}
			}
		}
		proxy, mux := bindings["NewProxy"], bindings["NewServeMux"]
		if proxy == nil || mux == nil || writes[proxy] != 1 || writes[mux] != 1 {
			problems = append(problems, "NewRuntime must bind one NewProxy and one http.NewServeMux result")
		}
		want := map[string]types.Object{"StartupConfig": cfg, "ConfigPath": path, "Proxy": proxy, "Handler": mux}
		literals := 0
		calls := map[string]int{}
		for _, stmt := range owner.Body.List {
			expr, ok := stmt.(*ast.ExprStmt)
			if !ok {
				continue
			}
			call, ok := expr.X.(*ast.CallExpr)
			if !ok {
				continue
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if ok && isStartRuntimeServices(info, sel) && proxy != nil && assemblyObject(info, sel.X) == proxy && len(call.Args) == 1 && assemblyObject(info, call.Args[0]) == cfg && allowed == nil {
				allowed = sel
			}
		}
		for n := range ast.Preorder(owner.Body) {
			switch n := n.(type) {
			case *ast.CallExpr:
				fn, ok := assemblyObject(info, n.Fun).(*types.Func)
				if !ok {
					continue
				}
				calls[fn.FullName()]++
				if fn.FullName() == appImport+".NewWebServer" {
					if len(n.Args) != 2 || assemblyObject(info, n.Args[0]) != proxy || assemblyObject(info, n.Args[1]) != path {
						problems = append(problems, "NewWebServer must use the constructed Proxy and config path")
					}
				}
			case *ast.CompositeLit:
				typ, ok := info.TypeOf(n).(*types.Named)
				if !ok || typ.Obj().Name() != "Runtime" || typ.Obj().Pkg().Path() != appImport {
					continue
				}
				literals++
				got := map[string]types.Object{}
				for _, e := range n.Elts {
					if kv, ok := e.(*ast.KeyValueExpr); ok {
						if id, ok := kv.Key.(*ast.Ident); ok {
							got[id.Name] = assemblyObject(info, kv.Value)
						}
					}
				}
				for name, obj := range want {
					if obj == nil || got[name] != obj {
						problems = append(problems, "Runtime."+name+" does not use its assembly binding")
					}
				}
			}
		}
		for _, fn := range []string{appImport + ".NewProxy", "net/http.NewServeMux", appImport + ".NewWebServer"} {
			if calls[fn] != 1 {
				problems = append(problems, fmt.Sprintf("NewRuntime calls %s %d times, want one", fn, calls[fn]))
			}
		}
		if literals != 1 {
			problems = append(problems, fmt.Sprintf("NewRuntime has %d Runtime literals, want one", literals))
		}
		if allowed == nil {
			problems = append(problems, "NewRuntime must start the constructed Proxy with its startup config")
		}
	}
	for n := range ast.Preorder(f) {
		if sel, ok := n.(*ast.SelectorExpr); ok && sel != allowed && isStartRuntimeServices(info, sel) {
			problems = append(problems, "unexpected reference to (*app.Proxy).StartRuntimeServices")
		}
	}
	return problems
}

func assemblyObject(info *types.Info, expr ast.Expr) types.Object {
	switch x := ast.Unparen(expr).(type) {
	case *ast.Ident:
		return info.ObjectOf(x)
	case *ast.SelectorExpr:
		return info.ObjectOf(x.Sel)
	}
	return nil
}

func isStartRuntimeServices(info *types.Info, sel *ast.SelectorExpr) bool {
	fn, ok := info.ObjectOf(sel.Sel).(*types.Func)
	return ok && sel.Sel.Name == "StartRuntimeServices" && fn.FullName() == "(*"+appImport+".Proxy).StartRuntimeServices"
}

func checkAssemblyControls(t *testing.T, imports types.Importer) {
	const fixture = `package app
import "net/http"
type Proxy struct{}
func (*Proxy) StartRuntimeServices(int) {}
func NewProxy(int) *Proxy { return &Proxy{} }
func NewWebServer(*Proxy,string) {}
type Other struct{}
func (Other) StartRuntimeServices(int) {}
type Runtime struct { ConfigPath string; StartupConfig int; Proxy *Proxy; Handler *http.ServeMux }
func NewRuntime(settings int, location string) *Runtime {
	assembled := NewProxy(settings)
	assembled.StartRuntimeServices(settings)
	router := http.NewServeMux()
	NewWebServer(assembled,location)
	return &Runtime{StartupConfig: settings, ConfigPath: location, Proxy: assembled, Handler: router}
}
`
	for _, tc := range []struct {
		name, from, to string
		valid          bool
	}{
		{"renamed locals and unrelated method", "", "func extra(o Other){o.StartRuntimeServices(0)}", true},
		{"unrelated function field", "", "func extra(o struct{StartRuntimeServices func(int)}){o.StartRuntimeServices(0)}", true},
		{"second receiver in helper", "", "func extra(other *Proxy){other.StartRuntimeServices(0)}", false},
		{"rebound local", "assembled.StartRuntimeServices(settings)", "assembled = new(Proxy); assembled.StartRuntimeServices(settings)", false},
		{"var binding", "assembled := NewProxy(settings)", "var assembled = NewProxy(settings)", true},
		{"unused second mux", "router := http.NewServeMux()", "router := http.NewServeMux(); http.NewServeMux()", false},
		{"method value", "", "func extra(other *Proxy){ _ = other.StartRuntimeServices }", false},
		{"method expression", "", "type Alias = Proxy; var extra = (*Alias).StartRuntimeServices", false},
		{"different runtime owner", "Proxy: assembled", "Proxy: &Proxy{}", false},
		{"different startup receiver", "assembled.StartRuntimeServices(settings)", "new(Proxy).StartRuntimeServices(settings)", false},
		{"deferred closure", "assembled.StartRuntimeServices(settings)", "_ = func(){ assembled.StartRuntimeServices(settings) }", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := fixture
			if tc.from == "" {
				src += tc.to
			} else {
				src = strings.Replace(src, tc.from, tc.to, 1)
			}
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "assembly.go", src, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			info := &types.Info{Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{}, Types: map[ast.Expr]types.TypeAndValue{}}
			conf := types.Config{Importer: imports}
			if _, err := conf.Check(appImport, fset, []*ast.File{f}, info); err != nil {
				t.Fatal(err)
			}
			problems := assemblyViolations(f, info, namedFunction(t, f, "NewRuntime"))
			if (len(problems) == 0) != tc.valid {
				t.Fatalf("unexpected assembly verdict: %v", problems)
			}
		})
	}
}
