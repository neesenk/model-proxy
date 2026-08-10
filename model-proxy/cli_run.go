package main

import (
	"io"

	clicmd "model-proxy/internal/cli"
)

// cliCommand aliases the internal/cli command contract.
type cliCommand = clicmd.Command

func runCLIArgsWithCommands(
	args []string,
	stdin io.Reader,
	stdout, stderr io.Writer,
	commands map[string]cliCommand,
) int {
	return clicmd.RunArgsWithCommands(args, stdin, stdout, stderr, commands)
}
