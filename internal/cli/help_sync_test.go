package cli

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// This file pins the cross-referenced CLI wiring that must move together:
// command registry (app.go NewApplication) ↔ -h usage list + per-command help
// (help.go) ↔ docs sections (CLI.md) ↔ the test-subprocess dispatch
// (subprocess_test_support_test.go). Each contract is checked against the real
// registry so adding/removing a command without updating its help text or docs
// fails here instead of drifting silently.

// usageCommandNames extracts the first token of every command line in the
// Usage "Commands:" section (e.g. "  routes [model]       ..." -> "routes").
func usageCommandNames(t *testing.T) map[string]string {
	t.Helper()
	start := strings.Index(Usage, "Commands:\n")
	end := strings.Index(Usage, "Options:")
	if start < 0 || end < 0 || end < start {
		t.Fatal("Usage: cannot locate Commands:/Options: sections")
	}
	out := map[string]string{}
	line := regexp.MustCompile(`^  (\S+)(.*)$`)
	for _, l := range strings.Split(Usage[start+len("Commands:\n"):end], "\n") {
		m := line.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		out[m[1]] = strings.TrimSpace(m[2])
	}
	if len(out) == 0 {
		t.Fatal("Usage: no command lines parsed")
	}
	return out
}

// registeredCommands builds the registry the same way production does.
func registeredCommands(t *testing.T) map[string]bool {
	t.Helper()
	app := NewApplication(func([]string) {})
	if len(app.Commands) == 0 {
		t.Fatal("NewApplication registered no commands")
	}
	out := make(map[string]bool, len(app.Commands))
	for cmd := range app.Commands {
		out[cmd] = true
	}
	return out
}

// TestHelpCoversRegisteredCommands pins the help contract in BOTH directions:
// every command registered in NewApplication must appear in the `-h` usage
// list AND have a per-command `<command> -h` entry (Help map), and every
// usage-listed command must be registered (minus `help`, which the dispatcher
// intercepts instead of registering). Adding a command without its help text —
// or leaving a stale entry after removing one — fails here.
func TestHelpCoversRegisteredCommands(t *testing.T) {
	registry := registeredCommands(t)
	usage := usageCommandNames(t)

	for cmd := range registry {
		if _, ok := usage[cmd]; !ok {
			t.Errorf("command %q is registered but missing from the -h usage list", cmd)
		}
		if _, ok := Help[cmd]; !ok {
			t.Errorf("command %q is registered but has no per-command help entry (Help map)", cmd)
		}
	}
	for cmd, desc := range usage {
		if _, registered := registry[cmd]; !registered && cmd != "help" {
			t.Errorf("usage list has %q but it is not a registered command (stale entry?)", cmd)
		}
		if desc == "" {
			t.Errorf("usage entry %q has no description", cmd)
		}
		if _, ok := Help[cmd]; cmd == "help" && ok {
			t.Error("help is intercepted before the Help map; a Help[\"help\"] entry is dead text")
		}
	}
	if _, ok := usage["help"]; !ok {
		t.Error("usage list must document the help command")
	}
}

// TestSubprocessDispatchCasesAreRegisteredCommands pins the test-infrastructure
// side of the same wiring: every `case "<cmd>":` in the helper-process
// dispatch (subprocess_test_support_test.go) must name a registered command.
// runCLI tests route through that switch, so a stale or mistyped case either
// silently never runs or 2s with "unknown MP_SUBCMD".
func TestSubprocessDispatchCasesAreRegisteredCommands(t *testing.T) {
	data, err := os.ReadFile("subprocess_test_support_test.go")
	if err != nil {
		t.Fatalf("read subprocess dispatch: %v", err)
	}
	registry := registeredCommands(t)
	// stop/reload are `serve` subcommands with direct dispatch entries here
	// (their handlers are cliserve.Cmd*), not top-level registry commands.
	serveSubcommands := map[string]bool{"stop": true, "reload": true}
	re := regexp.MustCompile(`(?m)^\tcase "([a-z]+)":`)
	seen := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(string(data), -1) {
		cmd := m[1]
		seen[cmd] = true
		if !registry[cmd] && !serveSubcommands[cmd] {
			t.Errorf("subprocess dispatch has case %q but %q is not a registered command (stale entry?)", cmd, cmd)
		}
	}
	if len(seen) == 0 {
		t.Fatal("no cases parsed from subprocess_test_support_test.go — dispatch contract is blind")
	}
}

// TestCLIMDDocumentsRegisteredCommands pins docs sync (AGENTS 红线 6): every
// registered command must have a `## N. `…“ section in CLI.md, and every
// command named in a CLI.md section heading must be registered. `help` is the
// only exemption (it has no section; the -h list documents it).
func TestCLIMDDocumentsRegisteredCommands(t *testing.T) {
	data, err := os.ReadFile("../../CLI.md")
	if err != nil {
		t.Fatalf("read CLI.md: %v", err)
	}
	documented := map[string]bool{}
	heading := regexp.MustCompile(`(?m)^## .*$`)
	tick := regexp.MustCompile("`([^`]+)`")
	for _, line := range heading.FindAllString(string(data), -1) {
		for _, phrase := range tick.FindAllStringSubmatch(line, -1) {
			cmd := strings.Fields(phrase[1])[0]
			cmd = strings.TrimSuffix(cmd, "|") // e.g. `config init|print|check`
			if strings.HasPrefix(cmd, "-") {
				continue // flag phrases (e.g. `--live`), not commands
			}
			documented[cmd] = true
		}
	}
	if len(documented) == 0 {
		t.Fatal("no command headings parsed from CLI.md — docs contract is blind")
	}

	registry := registeredCommands(t)
	var missing, stale []string
	for cmd := range registry {
		if cmd != "help" && !documented[cmd] {
			missing = append(missing, cmd)
		}
	}
	for cmd := range documented {
		if !registry[cmd] && cmd != "help" {
			stale = append(stale, cmd)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	if len(missing) > 0 {
		t.Errorf("CLI.md has no section for registered command(s): %s — add a `## N. ` section per command", strings.Join(missing, ", "))
	}
	if len(stale) > 0 {
		t.Errorf("CLI.md sections name unregistered command(s): %s — stale docs after a removal?", strings.Join(stale, ", "))
	}
}
