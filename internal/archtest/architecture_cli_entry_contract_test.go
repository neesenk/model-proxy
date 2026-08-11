package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// architecture_cli_entry_contract_test.go keeps the AST-level contracts on the
// thin root process entry: main must be a one-line
// os.Exit(newApplication().Run(...)), and the application composition reachable
// from NewApplication/Application.Run must not touch process globals. The
// behavioral CLI contracts (usage/help/dispatch/registry) live in
// internal/cli/cli_run_boundary_test.go.

func TestMainIsThinCLIEntry(t *testing.T) {
	files := parseCLIProductionFiles(t)
	mainFunc := findCLIProductionFunc(t, files, "main")
	if mainFunc.Type.Params.NumFields() != 0 || mainFunc.Type.Results != nil {
		t.Fatal("main must have no parameters or results")
	}
	if got := len(mainFunc.Body.List); got != 1 {
		t.Fatalf("main statements = %d, want exactly os.Exit(newApplication().Run(...))", got)
	}
	expression, ok := mainFunc.Body.List[0].(*ast.ExprStmt)
	if !ok {
		t.Fatalf("main statement type = %T, want expression statement", mainFunc.Body.List[0])
	}
	exitCall, ok := expression.X.(*ast.CallExpr)
	if !ok || !cliSelectorIs(exitCall.Fun, "os", "Exit") {
		t.Fatalf("main expression = %T, want os.Exit call", expression.X)
	}
	if len(exitCall.Args) != 1 {
		t.Fatalf("os.Exit arguments = %d, want newApplication().Run result only", len(exitCall.Args))
	}
	runCall, ok := exitCall.Args[0].(*ast.CallExpr)
	if !ok {
		t.Fatalf("os.Exit argument = %T, want newApplication().Run call", exitCall.Args[0])
	}
	runSelector, ok := runCall.Fun.(*ast.SelectorExpr)
	if !ok || runSelector.Sel.Name != "Run" {
		t.Fatalf("os.Exit callee = %T %v, want newApplication().Run", runCall.Fun, runCall.Fun)
	}
	applicationCall, ok := runSelector.X.(*ast.CallExpr)
	if !ok {
		t.Fatalf("Run receiver = %T, want newApplication()", runSelector.X)
	}
	applicationConstructor, ok := applicationCall.Fun.(*ast.Ident)
	if !ok || applicationConstructor.Name != "newApplication" || len(applicationCall.Args) != 0 {
		t.Fatalf("Run receiver = %T, want zero-argument newApplication()", runSelector.X)
	}
	if len(runCall.Args) != 4 {
		t.Fatalf("application.Run arguments = %d, want args plus three process streams", len(runCall.Args))
	}
	argsSlice, ok := runCall.Args[0].(*ast.SliceExpr)
	if !ok || !cliSelectorIs(argsSlice.X, "os", "Args") || !cliIntegerLiteralIs(argsSlice.Low, "1") ||
		argsSlice.High != nil || argsSlice.Max != nil {
		t.Fatalf("application.Run first argument must be exactly os.Args[1:]")
	}
	for index, name := range []string{"Stdin", "Stdout", "Stderr"} {
		if !cliSelectorIs(runCall.Args[index+1], "os", name) {
			t.Fatalf("application.Run argument %d must be exactly os.%s", index+2, name)
		}
	}
}

func TestRunCLIArgsDoesNotAccessProcessGlobals(t *testing.T) {
	files := parseCLIProductionFiles(t)
	assertCLIHandlerBindingIsDirect(t, files)
	forbidden := map[string]bool{
		"Args":   true,
		"Exit":   true,
		"Stdin":  true,
		"Stdout": true,
		"Stderr": true,
	}
	type functionLocation struct {
		function *ast.FuncDecl
		file     *ast.File
	}
	functions := map[string][]functionLocation{}
	for _, file := range files {
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok {
				continue
			}
			name := function.Name.Name
			if function.Recv != nil {
				receiver, ok := receiverTypeName(function.Recv)
				if !ok {
					t.Fatalf("CLI method %s has unsupported receiver", function.Name.Name)
				}
				name = receiver + "." + name
			}
			functions[name] = append(
				functions[name],
				functionLocation{function: function, file: file},
			)
		}
	}

	var found []string
	visited := map[string]bool{}
	queue := []string{"NewApplication", "Application.Run"}
	predeclaredCalls := map[string]bool{
		"append": true, "cap": true, "clear": true, "close": true, "complex": true,
		"copy": true, "delete": true, "imag": true, "len": true, "make": true,
		"max": true, "min": true, "new": true, "panic": true, "print": true,
		"println": true, "real": true, "recover": true,
		"bool": true, "byte": true, "complex64": true, "complex128": true,
		"error": true, "float32": true, "float64": true, "int": true,
		"int8": true, "int16": true, "int32": true, "int64": true,
		"rune": true, "string": true, "uint": true, "uint8": true,
		"uint16": true, "uint32": true, "uint64": true, "uintptr": true,
	}
	for len(queue) != 0 {
		name := queue[0]
		queue = queue[1:]
		if visited[name] {
			continue
		}
		visited[name] = true
		locations := functions[name]
		if len(locations) != 1 {
			t.Fatalf("reachable CLI function %s declarations = %d, want exactly 1", name, len(locations))
		}
		location := locations[0]
		osPackageNames := cliOSPackageNames(t, location.file)
		importedPackages := cliImportedPackageNames(location.file)
		for _, shadow := range cliImportedNameShadows(location.function, importedPackages) {
			found = append(found, name+":import-shadow:"+shadow)
		}
		ast.Inspect(location.function.Body, func(node ast.Node) bool {
			switch value := node.(type) {
			case *ast.SelectorExpr:
				if !forbidden[value.Sel.Name] {
					return true
				}
				packageName, ok := value.X.(*ast.Ident)
				if ok && osPackageNames[packageName.Name] {
					found = append(found, name+":"+packageName.Name+"."+value.Sel.Name)
				}
			case *ast.CallExpr:
				switch callable := value.Fun.(type) {
				case *ast.Ident:
					if len(functions[callable.Name]) != 0 {
						if !visited[callable.Name] {
							queue = append(queue, callable.Name)
						}
					} else if callable.Name == "run" {
						if name != "ProcessCommand" && name != "RunArgsWithCommands" {
							found = append(found, name+":unexpected-handler-call:"+callable.Name)
						}
					} else if !predeclaredCalls[callable.Name] {
						found = append(found, name+":unresolved-call:"+callable.Name)
					}
				case *ast.SelectorExpr:
					base, ok := callable.X.(*ast.Ident)
					if !ok || !importedPackages[base.Name] {
						found = append(found, name+":method-call:"+callable.Sel.Name)
					}
				}
			}
			return true
		})
	}

	sort.Strings(found)
	if len(found) != 0 {
		t.Fatalf("application.Run static call graph accesses process globals directly: %s", strings.Join(found, ", "))
	}
}

func assertCLIHandlerBindingIsDirect(t *testing.T, files []*ast.File) {
	t.Helper()
	function := findCLIProductionFunc(t, files, "RunArgsWithCommands")
	assignments := 0
	directBindings := 0
	calls := 0
	ast.Inspect(function.Body, func(node ast.Node) bool {
		switch value := node.(type) {
		case *ast.AssignStmt:
			assignsRun := false
			for _, expression := range value.Lhs {
				identifier, ok := expression.(*ast.Ident)
				if ok && identifier.Name == "run" {
					assignments++
					assignsRun = true
				}
			}
			if !assignsRun || value.Tok != token.DEFINE ||
				len(value.Lhs) != 2 || len(value.Rhs) != 1 {
				return true
			}
			runName, runOK := value.Lhs[0].(*ast.Ident)
			okName, okOK := value.Lhs[1].(*ast.Ident)
			index, indexOK := value.Rhs[0].(*ast.IndexExpr)
			if !runOK || !okOK || !indexOK {
				return true
			}
			commandsName, commandsOK := index.X.(*ast.Ident)
			commandName, commandOK := index.Index.(*ast.Ident)
			if runName.Name == "run" &&
				okName.Name == "ok" &&
				commandsOK && commandsName.Name == "commands" &&
				commandOK && commandName.Name == "cmd" {
				directBindings++
			}
		case *ast.CallExpr:
			identifier, ok := value.Fun.(*ast.Ident)
			if ok && identifier.Name == "run" {
				calls++
			}
		case *ast.ValueSpec:
			for _, name := range value.Names {
				if name.Name == "run" {
					assignments++
				}
			}
		case *ast.RangeStmt:
			for _, expression := range []ast.Expr{value.Key, value.Value} {
				identifier, ok := expression.(*ast.Ident)
				if ok && identifier.Name == "run" {
					assignments++
				}
			}
		}
		return true
	})
	if assignments != 1 || directBindings != 1 || calls != 1 {
		t.Fatalf(
			"CLI handler binding assignments/direct/calls = %d/%d/%d, want 1/1/1 from commands[cmd]",
			assignments,
			directBindings,
			calls,
		)
	}
}

func parseCLIProductionFiles(t *testing.T) []*ast.File {
	t.Helper()
	paths, err := filepath.Glob(repoRooted(t, "*.go"))
	if err == nil {
		if extra, gerr := filepath.Glob(repoRooted(t, "internal/cli/*.go")); gerr == nil {
			paths = append(paths, extra...)
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	fileSet := token.NewFileSet()
	files := make([]*ast.File, 0, len(paths))
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		files = append(files, file)
	}
	if len(files) == 0 {
		t.Fatal("no production Go files found")
	}
	return files
}

func findCLIProductionFunc(t *testing.T, files []*ast.File, name string) *ast.FuncDecl {
	t.Helper()
	var found []*ast.FuncDecl
	for _, file := range files {
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if ok && function.Recv == nil && function.Name.Name == name {
				found = append(found, function)
			}
		}
	}
	if len(found) != 1 {
		t.Fatalf("production function %s declarations = %d, want exactly 1", name, len(found))
	}
	return found[0]
}

func receiverTypeName(receivers *ast.FieldList) (string, bool) {
	if receivers == nil || len(receivers.List) != 1 {
		return "", false
	}
	typ := receivers.List[0].Type
	if pointer, ok := typ.(*ast.StarExpr); ok {
		typ = pointer.X
	}
	identifier, ok := typ.(*ast.Ident)
	return identifier.Name, ok
}

func identIs(expression ast.Expr, name string) bool {
	identifier, ok := expression.(*ast.Ident)
	return ok && identifier.Name == name
}

func expressionName(expression ast.Expr) string {
	switch value := expression.(type) {
	case *ast.Ident:
		return value.Name
	case *ast.SelectorExpr:
		base := expressionName(value.X)
		if base == "" {
			return ""
		}
		return base + "." + value.Sel.Name
	case *ast.ParenExpr:
		return expressionName(value.X)
	default:
		return ""
	}
}

func cliOSPackageNames(t *testing.T, file *ast.File) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	for _, importSpec := range file.Imports {
		if importSpec.Path.Value != `"os"` {
			continue
		}
		if importSpec.Name == nil {
			names["os"] = true
			continue
		}
		if importSpec.Name.Name == "." {
			t.Fatal("application composition source must not dot-import os")
		}
		if importSpec.Name.Name != "_" {
			names[importSpec.Name.Name] = true
		}
	}
	return names
}

func cliImportedPackageNames(file *ast.File) map[string]bool {
	names := map[string]bool{}
	for _, importSpec := range file.Imports {
		if importSpec.Name != nil {
			if importSpec.Name.Name != "_" && importSpec.Name.Name != "." {
				names[importSpec.Name.Name] = true
			}
			continue
		}
		importPath := strings.Trim(importSpec.Path.Value, `"`)
		names[filepath.Base(importPath)] = true
	}
	return names
}

func cliImportedNameShadows(function *ast.FuncDecl, imported map[string]bool) []string {
	var shadows []string
	record := func(identifier *ast.Ident) {
		if identifier != nil && imported[identifier.Name] {
			shadows = append(shadows, identifier.Name)
		}
	}
	recordFields := func(fieldLists ...*ast.FieldList) {
		for _, fieldList := range fieldLists {
			if fieldList == nil {
				continue
			}
			for _, field := range fieldList.List {
				for _, name := range field.Names {
					record(name)
				}
			}
		}
	}
	recordFields(function.Type.Params, function.Type.Results)
	ast.Inspect(function.Body, func(node ast.Node) bool {
		switch value := node.(type) {
		case *ast.FuncLit:
			recordFields(value.Type.Params, value.Type.Results)
		case *ast.TypeSpec:
			record(value.Name)
		case *ast.AssignStmt:
			if value.Tok != token.DEFINE {
				return true
			}
			for _, expression := range value.Lhs {
				identifier, _ := expression.(*ast.Ident)
				record(identifier)
			}
		case *ast.ValueSpec:
			for _, name := range value.Names {
				record(name)
			}
		case *ast.RangeStmt:
			if value.Tok != token.DEFINE {
				return true
			}
			key, _ := value.Key.(*ast.Ident)
			item, _ := value.Value.(*ast.Ident)
			record(key)
			record(item)
		}
		return true
	})
	sort.Strings(shadows)
	return shadows
}

func cliSelectorIs(expression ast.Expr, packageName, name string) bool {
	selector, ok := expression.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != name {
		return false
	}
	identifier, ok := selector.X.(*ast.Ident)
	return ok && identifier.Name == packageName
}

func cliIntegerLiteralIs(expression ast.Expr, value string) bool {
	literal, ok := expression.(*ast.BasicLit)
	return ok && literal.Kind == token.INT && literal.Value == value
}
