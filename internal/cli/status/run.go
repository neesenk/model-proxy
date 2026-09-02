package status

import (
	cliframework "model-proxy/internal/cli/framework"
)

// RunSchedule is the process-level entry for `schedule`: load config, then
// query the running daemon's /debug/schedule.
func RunSchedule(args []string) { CmdSchedule(args, cliframework.LoadCmdConfig(args)) }

// RunServeStatus is the process-level entry for `serve status`: load config,
// then print the running daemon's status snapshot.
func RunServeStatus(args []string) { CmdServeStatus(args, cliframework.LoadCmdConfig(args)) }
