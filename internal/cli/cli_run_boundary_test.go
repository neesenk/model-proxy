package cli

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

// serveStub is a no-op process-level serve driver for Application composition
// tests; the serve command is never dispatched by these tests.
func serveStub([]string) {}

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
			wantStdout: Usage,
			wantCode:   1,
		},
		{
			name:       "short help",
			args:       []string{"-h"},
			wantStdout: Usage,
			wantCode:   0,
		},
		{
			name:       "long help",
			args:       []string{"--help"},
			wantStdout: Usage,
			wantCode:   0,
		},
		{
			name:       "help command",
			args:       []string{"help"},
			wantStdout: Usage,
			wantCode:   0,
		},
		{
			name:       "unknown command",
			args:       []string{"not-a-command"},
			wantStdout: Usage,
			wantStderr: "unknown command: not-a-command\n\n",
			wantCode:   1,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := NewApplication(serveStub).Run(
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
		"add",
		"audit",
		"config",
		"doctor",
		"login",
		"logout",
		"models",
		"pin",
		"presets",
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
	gotCommands := make([]string, 0, len(Help))
	for command := range Help {
		gotCommands = append(gotCommands, command)
	}
	sort.Strings(gotCommands)
	if got, want := strings.Join(gotCommands, ","), strings.Join(helpCommands, ","); got != want {
		t.Fatalf("Help keys = %q, want exactly %q; update the exhaustive help cases", got, want)
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
			code := NewApplication(serveStub).Run(
				[]string{command, "--help", "--config", configFile},
				strings.NewReader("stdin must not be read for command help"),
				&stdout,
				&stderr,
			)
			wantStdout := Help[command] + "\n"
			if TakesProvider(command) {
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
		code := NewApplication(serveStub).Run(
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
			Help["serve"]+"\n",
			"",
		)
	})
}

func TestRunCLIArgsDispatchContract(t *testing.T) {
	t.Parallel()

	stdin := strings.NewReader("input")
	var stdout, stderr bytes.Buffer
	var gotArgs []string
	commands := map[string]Command{
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
	code := RunArgsWithCommands(
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
		"add":      "RunAdd",
		"audit":    "RunAudit",
		"presets":  "RunPresets",
		"config":   "RunConfig",
		"doctor":   "RunDoctor",
		"login":    "clilogin.CmdLogin",
		"logout":   "RunLogout",
		"models":   "RunModels",
		"pin":      "RunPin",
		"replay":   "RunReplay",
		"restore":  "RunRestore",
		"schedule": "RunSchedule",
		"serve":    "app.Serve",
		"shadow":   "RunShadow",
		"stats":    "RunStats",
		"takeover": "RunTakeover",
		"test":     "climodels.CmdTest",
		"unfreeze": "RunUnfreeze",
		"unpin":    "RunUnpin",
		"usage":    "RunUsage",
		"wire":     "RunWire",
	}
	app := NewApplication(serveStub)
	gotCommands := make([]string, 0, len(app.Commands))
	for command := range app.Commands {
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

	gotTargets := parseApplicationCommandTargets(t)
	if len(gotTargets) != len(wantTargets) {
		t.Fatalf("CLI command target count = %d, want %d: %#v", len(gotTargets), len(wantTargets), gotTargets)
	}
	for command, wantTarget := range wantTargets {
		if gotTarget := gotTargets[command]; gotTarget != wantTarget {
			t.Errorf("NewApplication().commands[%q] target = %q, want %q", command, gotTarget, wantTarget)
		}
	}
}

func parseApplicationCommandTargets(t *testing.T) map[string]string {
	t.Helper()
	files := parseCLIProductionFiles(t)
	constructor := findCLIProductionFunc(t, files, "NewApplication")
	targets := map[string]string{}
	assignments := 0
	ast.Inspect(constructor.Body, func(node ast.Node) bool {
		assignment, ok := node.(*ast.AssignStmt)
		if !ok || assignment.Tok != token.ASSIGN || len(assignment.Lhs) != 1 || len(assignment.Rhs) != 1 {
			return true
		}
		field, ok := assignment.Lhs[0].(*ast.SelectorExpr)
		if !ok || field.Sel.Name != "Commands" || !identIs(field.X, "app") {
			return true
		}
		assignments++
		literal, ok := assignment.Rhs[0].(*ast.CompositeLit)
		if !ok {
			t.Fatalf("NewApplication app.Commands value = %T, want map literal", assignment.Rhs[0])
		}
		for _, element := range literal.Elts {
			entry, ok := element.(*ast.KeyValueExpr)
			if !ok {
				t.Fatalf("NewApplication app.Commands element = %T, want key/value", element)
			}
			key, ok := entry.Key.(*ast.BasicLit)
			if !ok || key.Kind != token.STRING {
				t.Fatalf("NewApplication app.Commands key = %T, want string literal", entry.Key)
			}
			command := strings.Trim(key.Value, `"`)
			call, ok := entry.Value.(*ast.CallExpr)
			if !ok || len(call.Args) != 1 || !identIs(call.Fun, "ProcessCommand") {
				t.Fatalf("NewApplication().commands[%q] value = %T, want ProcessCommand(handler)", command, entry.Value)
			}
			handler := expressionName(call.Args[0])
			if handler == "" {
				t.Fatalf("NewApplication().commands[%q] handler = %T, want concrete handler", command, call.Args[0])
			}
			if previous, exists := targets[command]; exists {
				t.Fatalf("duplicate NewApplication().commands[%q]: %s and %s", command, previous, handler)
			}
			targets[command] = handler
		}
		return true
	})
	if assignments != 1 {
		t.Fatalf("NewApplication app.Commands assignments = %d, want exactly 1 concrete map", assignments)
	}
	return targets
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
