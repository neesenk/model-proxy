package presets

import "os"

// RunAdd / RunPresets are the process-level entries: they adapt the stream-
// parameterized handlers to the process Command contract (exit code becomes
// the exit status).
func RunAdd(args []string) {
	os.Exit(CmdAdd(args, os.Stdin, os.Stdout, os.Stderr))
}

// RunPresets is the process-level entry for `presets`.
func RunPresets(args []string) {
	os.Exit(CmdPresets(args, os.Stdin, os.Stdout, os.Stderr))
}
