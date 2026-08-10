// commands.go owns the process-level CLI entries: each loads config (when
// needed) and delegates to the command implementation. The composition root
// (main) only registers these by name.
package cli

import (
	"log"
	"os"

	cliframework "model-proxy/internal/cli/framework"
	climodels "model-proxy/internal/cli/models"
	configdomain "model-proxy/internal/config"
)

// LoadCmdConfig loads the CLI config or exits.
func LoadCmdConfig(args []string) *configdomain.Config {
	cfg, err := configdomain.LoadConfig(cliframework.ConfigPath(args))
	if err != nil {
		log.Fatal(err)
	}
	return cfg
}

func RunStats(args []string) {
	cfg := LoadCmdConfig(args)
	os.Exit(CmdStats(args, cfg.Listen, os.Stdout, os.Stderr))
}

func RunModels(args []string) {
	cfg := LoadCmdConfig(args)
	climodels.CmdModels(args, cfg, cliframework.ConfigPath(args))
}

func RunUsage(args []string)  { CmdUsage(args, LoadCmdConfig(args)) }
func RunLogout(args []string) { CmdLogout(args, LoadCmdConfig(args)) }

func RunConfig(args []string)        { CmdConfigRun(args) }
func RunSchedule(args []string)      { CmdSchedule(args, LoadCmdConfig(args)) }
func RunPin(args []string)           { CmdPin(args, LoadCmdConfig(args)) }
func RunUnpin(args []string)         { CmdUnpin(args, LoadCmdConfig(args)) }
func RunUnfreeze(args []string)      { CmdUnfreeze(args, LoadCmdConfig(args)) }
func RunReplay(args []string)        { CmdReplay(args, LoadCmdConfig(args)) }
func RunShadow(args []string)        { CmdShadow(args, LoadCmdConfig(args)) }
func RunShadowReport(args []string)  { CmdShadowReport(args, LoadCmdConfig(args)) }
func RunWire(args []string)          { CmdWire(args, LoadCmdConfig(args)) }
func RunWireRecordCLI(args []string) { CmdWireRecord(args, LoadCmdConfig(args)) }
func RunServeStatus(args []string)   { CmdServeStatus(args, LoadCmdConfig(args)) }
