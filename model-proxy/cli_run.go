package main

import (
	"io"

	clicmd "model-proxy/internal/cli"
)

// cliCommand aliases the internal/cli command contract.
type cliCommand = clicmd.Command

func processCLICommand(run func([]string)) cliCommand {
	return clicmd.ProcessCommand(run)
}

func runCLIArgsWithCommands(
	args []string,
	stdin io.Reader,
	stdout, stderr io.Writer,
	commands map[string]cliCommand,
) int {
	return clicmd.RunArgsWithCommands(args, stdin, stdout, stderr, commands)
}

func hasHelpFlag(args []string) bool { return clicmd.HasHelpFlag(args) }
