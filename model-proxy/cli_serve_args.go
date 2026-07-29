package main

import "strings"

// serveArgs holds parsed `serve` flags.
type serveArgs struct {
	config  string
	logFile string // --log-file override
}

func parseServeArgs(args []string) serveArgs {
	sa := serveArgs{config: configPath(args)}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--log-file":
			if i+1 < len(args) {
				sa.logFile = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "--log-file="):
			sa.logFile = strings.TrimPrefix(a, "--log-file=")
		}
	}
	return sa
}
