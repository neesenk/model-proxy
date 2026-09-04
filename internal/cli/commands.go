// commands.go owns the process-level CLI entries that stay at the root: the
// dispatch loop, the Command contract, and the takeover/restore client-config
// rewrites. Every other command's process entry lives in its own subpackage
// (cli/<domain>.Run<Command>); the composition root (main) only registers
// these by name.
package cli

import (
	"fmt"
	"io"
	"log"

	clidoctor "model-proxy/internal/cli/doctor"
	cliframework "model-proxy/internal/cli/framework"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/observe/logx"
	"model-proxy/internal/takeover"
)

// Command is the process-level adapter for an existing command handler. The
// stream parameters make the front-door contract explicit.
type Command func(args []string, stdin io.Reader, stdout, stderr io.Writer) int

// ProcessCommand adapts a plain handler to the Command contract.
func ProcessCommand(run func([]string)) Command {
	return func(args []string, _ io.Reader, _, _ io.Writer) int {
		run(args)
		return 0
	}
}

// RunArgsWithCommands is the CLI dispatch loop: help handling, then the named
// command. Neither layer reads process globals.
func RunArgsWithCommands(
	args []string,
	stdin io.Reader,
	stdout, stderr io.Writer,
	commands map[string]Command,
) int {
	if len(args) == 0 {
		_, _ = io.WriteString(stdout, Usage)
		return 1
	}

	cmd := args[0]
	if cmd == "-h" || cmd == "--help" || cmd == "help" {
		_, _ = io.WriteString(stdout, Usage)
		return 0
	}

	if help, ok := Help[cmd]; ok && HasHelpFlag(args[1:]) {
		_, _ = fmt.Fprintln(stdout, help)
		if TakesProvider(cmd) {
			PrintConfigProvidersTo(stdout, args[1:])
		}
		return 0
	}

	run, ok := commands[cmd]
	if !ok {
		_, _ = fmt.Fprintf(stderr, "unknown command: %s\n\n", cmd)
		_, _ = io.WriteString(stdout, Usage)
		return 1
	}
	return run(args[1:], stdin, stdout, stderr)
}

// HasHelpFlag reports whether args include -h/--help.
func HasHelpFlag(args []string) bool {
	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			return true
		}
	}
	return false
}

// RunTakeover rewrites a client config to point at the proxy.
// `takeover list` prints the available client templates (preset + user).
func RunTakeover(args []string) {
	cfg := cliframework.LoadCmdConfig(args)
	which := cliframework.Positional(args)
	if which == "list" {
		clients, err := takeover.ListClients(cfg, "", "")
		if err != nil {
			log.Fatal(err)
		}
		// Mark the variant `takeover <family>` / `takeover all` would pick
		// for each multi-template family (native-protocol auto-selection).
		selected := takeover.AutoSelectedNames(cfg, "")
		for _, c := range clients {
			desc := c.Template.Description
			if desc == "" {
				desc = "-"
			}
			mark := " "
			if selected[c.Name] {
				mark = "*"
			}
			proto := c.Template.Protocol
			if proto == "" {
				proto = "-"
			}
			fmt.Printf("%s %-17s %-12s %-10s %-8s %-10s %s\n", mark, c.Name, c.Template.ClientFamily(), proto, c.Template.Format, c.Template.Source, desc)
			fmt.Printf("%-21s%s\n", "", c.File)
		}
		fmt.Println("\n* = variant auto-selected for its client family (protocol the route providers serve natively)")
		return
	}
	bakDir := takeover.BackupDir(cliframework.ConfigPath(args))
	if err := takeover.RunTakeover(cfg, which, bakDir, takeover.ModelFactsFor(cfg, which, cliframework.HomeDir(), ""), ""); err != nil {
		log.Fatal(err)
	}
	verifyTakeoverDrift(cfg, which, bakDir)
}

// verifyTakeoverDrift re-checks the proxy pointer of every client this
// takeover just rewrote, reusing doctor's drift check. A healthy takeover
// shows no drift; a client that still does not point at the proxy (rewritten
// back by another tool, a write that did not take effect, a vanished file)
// gets a stderr warning and — when guard.audit is on — a seclog drift record
// via doctor's shared audit helper. Verification never changes the exit
// code: warnings and audit-append failures degrade to stderr notes only.
func verifyTakeoverDrift(cfg *configdomain.Config, which, bakDir string) {
	selected := map[string]bool{}
	clients, err := takeover.ResolveClients(cfg, which, "")
	if err != nil {
		logx.Warnf("takeover: list clients: %v", err)
		return
	}
	for _, c := range clients {
		selected[c.Name] = true
	}
	var drift []clidoctor.ClientDrift
	for _, d := range clidoctor.CheckTakeoverDrift(cfg, bakDir, "") {
		if !selected[d.Client] || !d.Taken || d.OK {
			continue
		}
		drift = append(drift, d)
		logx.Warnf("  ⚠ %s drift detected right after takeover: %s points to %s, want %s",
			d.Client, d.File, d.Current, d.Expected)
	}
	clidoctor.AuditTakeoverDrift(cfg, drift, "takeover")
}

// RunRestore restores a client config from its takeover backup.
func RunRestore(args []string) {
	cfg := cliframework.LoadCmdConfig(args)
	which := cliframework.Positional(args)
	if err := takeover.RunRestore(cfg, which, takeover.BackupDir(cliframework.ConfigPath(args)), ""); err != nil {
		log.Fatal(err)
	}
}
