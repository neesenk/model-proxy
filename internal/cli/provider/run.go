package provider

import "os"

// RunProvider is the process-level entry: it adapts the stream-parameterized
// handler to the process Command contract (exit code becomes the exit status).
func RunProvider(args []string) {
	os.Exit(CmdProvider(args, os.Stdin, os.Stdout, os.Stderr))
}
