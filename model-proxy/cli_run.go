package main

import (
	"fmt"
	"io"
)

// cliCommand is the process-level adapter for an existing command handler.
// The stream parameters make the front-door contract explicit and provide the
// migration seam for handlers that still own process-scoped I/O or termination.
type cliCommand func(args []string, stdin io.Reader, stdout, stderr io.Writer) int

func processCLICommand(run func([]string)) cliCommand {
	return func(args []string, _ io.Reader, _, _ io.Writer) int {
		run(args)
		return 0
	}
}

var cliCommands = map[string]cliCommand{
	"serve":    processCLICommand(cmdServe),
	"takeover": processCLICommand(cmdTakeover),
	"restore":  processCLICommand(cmdRestore),
	"login":    processCLICommand(cmdLogin),
	"logout":   processCLICommand(cmdLogout),
	"usage":    processCLICommand(cmdUsage),
	"models":   processCLICommand(cmdModels),
	"config":   processCLICommand(cmdConfig),
	"schedule": processCLICommand(cmdSchedule),
	"pin":      processCLICommand(cmdPin),
	"unpin":    processCLICommand(cmdUnpin),
	"unfreeze": processCLICommand(cmdUnfreeze),
	"stats":    processCLICommand(cmdStats),
	"doctor":   processCLICommand(cmdDoctor),
	"test":     processCLICommand(cmdTest),
	"replay":   processCLICommand(cmdReplay),
	"shadow":   processCLICommand(cmdShadow),
	"wire":     processCLICommand(cmdWire),
}

// runCLIArgs owns top-level CLI parsing and exit status. It deliberately does
// not read process globals: main binds os.Args and the process streams once,
// while tests can call this boundary directly with buffers.
func runCLIArgs(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return runCLIArgsWithCommands(args, stdin, stdout, stderr, cliCommands)
}

func runCLIArgsWithCommands(
	args []string,
	stdin io.Reader,
	stdout, stderr io.Writer,
	commands map[string]cliCommand,
) int {
	if len(args) == 0 {
		_, _ = io.WriteString(stdout, usage)
		return 1
	}

	cmd := args[0]
	if cmd == "-h" || cmd == "--help" || cmd == "help" {
		_, _ = io.WriteString(stdout, usage)
		return 0
	}

	if help, ok := cmdHelp[cmd]; ok && hasHelpFlag(args[1:]) {
		_, _ = fmt.Fprintln(stdout, help)
		if takesProvider(cmd) {
			printConfigProvidersTo(stdout, args[1:])
		}
		return 0
	}

	run, ok := commands[cmd]
	if !ok {
		_, _ = fmt.Fprintf(stderr, "unknown command: %s\n\n", cmd)
		_, _ = io.WriteString(stdout, usage)
		return 1
	}
	return run(args[1:], stdin, stdout, stderr)
}

func hasHelpFlag(args []string) bool {
	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			return true
		}
	}
	return false
}
