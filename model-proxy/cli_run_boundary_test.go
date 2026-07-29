package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestRunCLIArgsTopLevelContract(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantStdout string
		wantStderr string
		wantCode   int
	}{
		{
			name:       "no arguments",
			wantStdout: usage,
			wantCode:   1,
		},
		{
			name:       "short help",
			args:       []string{"-h"},
			wantStdout: usage,
			wantCode:   0,
		},
		{
			name:       "long help",
			args:       []string{"--help"},
			wantStdout: usage,
			wantCode:   0,
		},
		{
			name:       "help command",
			args:       []string{"help"},
			wantStdout: usage,
			wantCode:   0,
		},
		{
			name:       "unknown command",
			args:       []string{"not-a-command"},
			wantStdout: usage,
			wantStderr: "unknown command: not-a-command\n\n",
			wantCode:   1,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := runCLIArgs(
				test.args,
				strings.NewReader("stdin must not be read for top-level routing"),
				&stdout,
				&stderr,
			)
			requireCLIResult(
				t,
				code,
				stdout.String(),
				stderr.String(),
				test.wantCode,
				test.wantStdout,
				test.wantStderr,
			)
		})
	}
}

func TestRunCLIArgsCommandHelpContract(t *testing.T) {
	helpCommands := []string{
		"config",
		"doctor",
		"login",
		"logout",
		"models",
		"pin",
		"replay",
		"restore",
		"schedule",
		"serve",
		"stats",
		"takeover",
		"test",
		"unfreeze",
		"unpin",
		"usage",
		"wire",
	}
	gotCommands := make([]string, 0, len(cmdHelp))
	for command := range cmdHelp {
		gotCommands = append(gotCommands, command)
	}
	sort.Strings(gotCommands)
	if got, want := strings.Join(gotCommands, ","), strings.Join(helpCommands, ","); got != want {
		t.Fatalf("cmdHelp keys = %q, want exactly %q; update the exhaustive help cases", got, want)
	}

	configFile := writeTempConfig(t, minimalConfig)
	providerAppendix := fmt.Sprintf(
		"\nProviders (from config):\n  %-14s  provider=%-12s  %s\n",
		"aqp",
		"aqp",
		"https://example.invalid/compass-api/v1",
	)

	for _, command := range helpCommands {
		command := command
		t.Run(command, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := runCLIArgs(
				[]string{command, "--help", "--config", configFile},
				strings.NewReader("stdin must not be read for command help"),
				&stdout,
				&stderr,
			)
			wantStdout := cmdHelp[command] + "\n"
			if takesProvider(command) {
				wantStdout += providerAppendix
			}
			requireCLIResult(
				t,
				code,
				stdout.String(),
				stderr.String(),
				0,
				wantStdout,
				"",
			)
		})
	}

	t.Run("short flag uses the same exact output", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := runCLIArgs(
			[]string{"serve", "-h"},
			strings.NewReader(""),
			&stdout,
			&stderr,
		)
		requireCLIResult(
			t,
			code,
			stdout.String(),
			stderr.String(),
			0,
			cmdHelp["serve"]+"\n",
			"",
		)
	})
}

func TestRunCLIArgsDispatchContract(t *testing.T) {
	t.Parallel()

	stdin := strings.NewReader("input")
	var stdout, stderr bytes.Buffer
	var gotArgs []string
	commands := map[string]cliCommand{
		"probe": func(args []string, gotStdin io.Reader, gotStdout, gotStderr io.Writer) int {
			gotArgs = append([]string(nil), args...)
			if gotStdin != stdin {
				t.Error("handler did not receive the injected stdin")
			}
			_, _ = io.WriteString(gotStdout, "probe stdout\n")
			_, _ = io.WriteString(gotStderr, "probe stderr\n")
			return 7
		},
	}
	code := runCLIArgsWithCommands(
		[]string{"probe", "first", "--flag"},
		stdin,
		&stdout,
		&stderr,
		commands,
	)
	requireCLIResult(
		t,
		code,
		stdout.String(),
		stderr.String(),
		7,
		"probe stdout\n",
		"probe stderr\n",
	)
	if got, want := strings.Join(gotArgs, "\x00"), "first\x00--flag"; got != want {
		t.Fatalf("handler args = %q, want %q", gotArgs, []string{"first", "--flag"})
	}
}

func TestCLICommandRegistryIsExhaustive(t *testing.T) {
	wantTargets := map[string]string{
		"config":   "cmdConfig",
		"doctor":   "cmdDoctor",
		"login":    "cmdLogin",
		"logout":   "cmdLogout",
		"models":   "cmdModels",
		"pin":      "cmdPin",
		"replay":   "cmdReplay",
		"restore":  "cmdRestore",
		"schedule": "cmdSchedule",
		"serve":    "cmdServe",
		"shadow":   "cmdShadow",
		"stats":    "cmdStats",
		"takeover": "cmdTakeover",
		"test":     "cmdTest",
		"unfreeze": "cmdUnfreeze",
		"unpin":    "cmdUnpin",
		"usage":    "cmdUsage",
		"wire":     "cmdWire",
	}
	gotCommands := make([]string, 0, len(cliCommands))
	for command := range cliCommands {
		gotCommands = append(gotCommands, command)
	}
	sort.Strings(gotCommands)
	wantCommands := make([]string, 0, len(wantTargets))
	for command := range wantTargets {
		wantCommands = append(wantCommands, command)
	}
	sort.Strings(wantCommands)
	if got, want := strings.Join(gotCommands, ","), strings.Join(wantCommands, ","); got != want {
		t.Fatalf("CLI command registry = %q, want exactly %q", got, want)
	}

	gotTargets := parseCLICommandTargets(t)
	if len(gotTargets) != len(wantTargets) {
		t.Fatalf("CLI command target count = %d, want %d: %#v", len(gotTargets), len(wantTargets), gotTargets)
	}
	for command, wantTarget := range wantTargets {
		if gotTarget := gotTargets[command]; gotTarget != wantTarget {
			t.Errorf("cliCommands[%q] target = %q, want %q", command, gotTarget, wantTarget)
		}
	}
}

func TestMainIsThinCLIEntry(t *testing.T) {
	files := parseCLIProductionFiles(t)
	mainFunc := findCLIProductionFunc(t, files, "main")
	if mainFunc.Type.Params.NumFields() != 0 || mainFunc.Type.Results != nil {
		t.Fatal("main must have no parameters or results")
	}
	if got := len(mainFunc.Body.List); got != 1 {
		t.Fatalf("main statements = %d, want exactly os.Exit(runCLIArgs(...))", got)
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
		t.Fatalf("os.Exit arguments = %d, want runCLIArgs result only", len(exitCall.Args))
	}
	runCall, ok := exitCall.Args[0].(*ast.CallExpr)
	if !ok {
		t.Fatalf("os.Exit argument = %T, want runCLIArgs call", exitCall.Args[0])
	}
	runName, ok := runCall.Fun.(*ast.Ident)
	if !ok || runName.Name != "runCLIArgs" {
		t.Fatalf("os.Exit callee = %T %v, want runCLIArgs", runCall.Fun, runCall.Fun)
	}
	if len(runCall.Args) != 4 {
		t.Fatalf("runCLIArgs arguments = %d, want args plus three process streams", len(runCall.Args))
	}
	argsSlice, ok := runCall.Args[0].(*ast.SliceExpr)
	if !ok || !cliSelectorIs(argsSlice.X, "os", "Args") || !cliIntegerLiteralIs(argsSlice.Low, "1") ||
		argsSlice.High != nil || argsSlice.Max != nil {
		t.Fatalf("runCLIArgs first argument must be exactly os.Args[1:]")
	}
	for index, name := range []string{"Stdin", "Stdout", "Stderr"} {
		if !cliSelectorIs(runCall.Args[index+1], "os", name) {
			t.Fatalf("runCLIArgs argument %d must be exactly os.%s", index+2, name)
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
			if !ok || function.Recv != nil {
				continue
			}
			functions[function.Name.Name] = append(
				functions[function.Name.Name],
				functionLocation{function: function, file: file},
			)
		}
	}

	var found []string
	visited := map[string]bool{}
	queue := []string{"runCLIArgs"}
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
						if name != "runCLIArgsWithCommands" {
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
		t.Fatalf("runCLIArgs static call graph accesses process globals directly: %s", strings.Join(found, ", "))
	}
}

func parseCLICommandTargets(t *testing.T) map[string]string {
	t.Helper()
	files := parseCLIProductionFiles(t)
	targets := map[string]string{}
	declarations := 0
	for _, file := range files {
		for _, declaration := range file.Decls {
			general, ok := declaration.(*ast.GenDecl)
			if !ok || general.Tok != token.VAR {
				continue
			}
			for _, specification := range general.Specs {
				values, ok := specification.(*ast.ValueSpec)
				if !ok || len(values.Names) != 1 || values.Names[0].Name != "cliCommands" {
					continue
				}
				declarations++
				if len(values.Values) != 1 {
					t.Fatalf("cliCommands values = %d, want one map literal", len(values.Values))
				}
				literal, ok := values.Values[0].(*ast.CompositeLit)
				if !ok {
					t.Fatalf("cliCommands value = %T, want map literal", values.Values[0])
				}
				for _, element := range literal.Elts {
					entry, ok := element.(*ast.KeyValueExpr)
					if !ok {
						t.Fatalf("cliCommands element = %T, want key/value", element)
					}
					key, ok := entry.Key.(*ast.BasicLit)
					if !ok || key.Kind != token.STRING {
						t.Fatalf("cliCommands key = %T, want string literal", entry.Key)
					}
					command := strings.Trim(key.Value, `"`)
					call, ok := entry.Value.(*ast.CallExpr)
					if !ok || len(call.Args) != 1 {
						t.Fatalf("cliCommands[%q] value = %T, want processCLICommand(handler)", command, entry.Value)
					}
					factory, ok := call.Fun.(*ast.Ident)
					if !ok || factory.Name != "processCLICommand" {
						t.Fatalf("cliCommands[%q] factory = %T, want processCLICommand", command, call.Fun)
					}
					handler, ok := call.Args[0].(*ast.Ident)
					if !ok {
						t.Fatalf("cliCommands[%q] handler = %T, want identifier", command, call.Args[0])
					}
					if previous, exists := targets[command]; exists {
						t.Fatalf("duplicate cliCommands[%q]: %s and %s", command, previous, handler.Name)
					}
					targets[command] = handler.Name
				}
			}
		}
	}
	if declarations != 1 {
		t.Fatalf("cliCommands declarations = %d, want exactly 1", declarations)
	}
	return targets
}

func assertCLIHandlerBindingIsDirect(t *testing.T, files []*ast.File) {
	t.Helper()
	function := findCLIProductionFunc(t, files, "runCLIArgsWithCommands")
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

func requireCLIResult(
	t *testing.T,
	code int,
	stdout string,
	stderr string,
	wantCode int,
	wantStdout string,
	wantStderr string,
) {
	t.Helper()
	if code != wantCode {
		t.Errorf("exit code = %d, want %d", code, wantCode)
	}
	if stdout != wantStdout {
		t.Errorf("stdout mismatch\n--- got ---\n%q\n--- want ---\n%q", stdout, wantStdout)
	}
	if stderr != wantStderr {
		t.Errorf("stderr mismatch\n--- got ---\n%q\n--- want ---\n%q", stderr, wantStderr)
	}
}

func parseCLIProductionFiles(t *testing.T) []*ast.File {
	t.Helper()
	paths, err := filepath.Glob("*.go")
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
			t.Fatal("runCLIArgs source must not dot-import os")
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
