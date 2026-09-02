package clitest

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHelperProcess is this package's own subprocess entrypoint, used by the
// RunCLI* self-tests below. The dummy handlers never touch config or the
// network: "echo" prints its args, "cat" echoes stdin, "cwd" prints the
// working directory, "exit3" exits non-zero.
func TestHelperProcess(t *testing.T) {
	HelperProcess(t, map[string]func([]string){
		"echo": func(args []string) { fmt.Println(strings.Join(args, " ")) },
		"cat": func([]string) {
			b, _ := io.ReadAll(os.Stdin)
			fmt.Print(string(b))
		},
		"cwd": func([]string) {
			d, err := os.Getwd()
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(2)
			}
			fmt.Println(d)
		},
		"exit3": func([]string) { os.Exit(3) },
	})
}

func TestRunCLI_EchoRoundTrip(t *testing.T) {
	stdout, _, code := RunCLI(t, "echo", "", "hello", "world")
	if code != 0 {
		t.Fatalf("echo exit = %d, want 0", code)
	}
	if strings.TrimSpace(stdout) != "hello world" {
		t.Errorf("echo stdout = %q, want %q", stdout, "hello world")
	}
}

func TestRunCLI_ExitCodePropagates(t *testing.T) {
	_, _, code := RunCLI(t, "exit3", "")
	if code != 3 {
		t.Errorf("exit3 code = %d, want 3", code)
	}
}

func TestRunCLIWithStdin_PipesInput(t *testing.T) {
	stdout, _, code := RunCLIWithStdin(t, "stdin-text", "", "cat", "")
	if code != 0 {
		t.Fatalf("cat exit = %d, want 0", code)
	}
	if stdout != "stdin-text" {
		t.Errorf("cat stdout = %q, want piped stdin", stdout)
	}
}

func TestRunCLIInCWD_PinsWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	// macOS temp dirs are /var symlinks into /private/var; the subprocess
	// reports the resolved path.
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	stdout, _, code := RunCLIInCWD(t, dir, "cwd")
	if code != 0 {
		t.Fatalf("cwd exit = %d, want 0", code)
	}
	if strings.TrimSpace(stdout) != want {
		t.Errorf("cwd = %q, want %q", strings.TrimSpace(stdout), want)
	}
}

func TestRunHelper_Dispatch(t *testing.T) {
	var got []string
	handlers := map[string]func([]string){
		"probe": func(args []string) { got = args },
	}
	env := func(m map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
	}
	code := runHelper(env(map[string]string{
		"MP_SUBCMD":   "probe",
		"MP_CLI_ARGS": "a --flag b",
	}), []string{"tb", "--", "ignored"}, handlers)
	if code != 0 {
		t.Errorf("runHelper code = %d, want 0", code)
	}
	if strings.Join(got, " ") != "a --flag b" {
		t.Errorf("handler args = %v, want [a --flag b]", got)
	}

	// Without MP_CLI_ARGS the last argv element is the (single) arg.
	got = nil
	code = runHelper(env(map[string]string{"MP_SUBCMD": "probe"}), []string{"tb", "--", "last"}, handlers)
	if code != 0 || strings.Join(got, " ") != "last" {
		t.Errorf("argv fallback: code = %d args = %v", code, got)
	}
}

func TestRunHelper_UnknownSubcommand(t *testing.T) {
	code := runHelper(func(string) (string, bool) { return "nope", true }, []string{"tb"}, map[string]func([]string){})
	if code != 2 {
		t.Errorf("unknown subcommand code = %d, want 2", code)
	}
}

func TestRunHelper_EmptyArgsEnvMeansNoArgs(t *testing.T) {
	var got []string
	handlers := map[string]func([]string){"probe": func(args []string) { got = args }}
	env := func(k string) (string, bool) {
		switch k {
		case "MP_CLI_ARGS":
			return "", true
		case "MP_SUBCMD":
			return "probe", true
		}
		return "", false
	}
	// MP_CLI_ARGS is set but empty: the argv fallback ("tb") must NOT win.
	code := runHelper(env, []string{"tb"}, handlers)
	if code != 0 || len(got) != 0 {
		t.Errorf("empty MP_CLI_ARGS: code = %d args = %v, want no args", code, got)
	}
}

func TestRunHelper_BadCWD(t *testing.T) {
	code := runHelper(func(k string) (string, bool) {
		if k == "MP_CLI_CWD" {
			return "/definitely/does/not/exist", true
		}
		return "probe", true
	}, []string{"tb"}, map[string]func([]string){"probe": func([]string) {}})
	if code != 2 {
		t.Errorf("bad cwd code = %d, want 2", code)
	}
}

func TestSetStdin(t *testing.T) {
	SetStdin(t, "piped-input")
	b, err := io.ReadAll(os.Stdin)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "piped-input" {
		t.Errorf("stdin = %q, want piped-input", b)
	}
}
