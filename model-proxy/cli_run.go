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

// runCLIArgs is the compatibility entry used by tests and embedders that still
// call the pre-composition boundary. The concrete application owns command
// registration; neither layer reads process globals.
func runCLIArgs(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return newApplication().Run(args, stdin, stdout, stderr)
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
