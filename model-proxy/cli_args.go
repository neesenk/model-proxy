package main

import (
	cliframework "model-proxy/internal/cli/framework"
)

// CLI-wide arg parsing delegates to internal/cli/framework; aliases keep root
// call sites compiling during migration.
func configPath(args []string) string { return cliframework.ConfigPath(args) }
func positional(args []string) string { return cliframework.Positional(args) }
func flagStringValue(args []string, flag string) string {
	return cliframework.FlagStringValue(args, flag)
}
func hasFlagValue(args []string, flag string) bool {
	return cliframework.HasFlagValue(args, flag)
}
