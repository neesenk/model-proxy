package main

import (
	clicmd "model-proxy/internal/cli"
	clilogin "model-proxy/internal/cli/login"
	climodels "model-proxy/internal/cli/models"
)

// Command wrappers delegate to internal/cli Run* entries.
func cmdStats(args []string)          { clicmd.RunStats(args) }
func cmdModels(args []string)         { clicmd.RunModels(args) }
func cmdUsage(args []string)          { clicmd.RunUsage(args) }
func cmdLogout(args []string)         { clicmd.RunLogout(args) }
func cmdConfig(args []string)         { clicmd.RunConfig(args) }
func cmdSchedule(args []string)       { clicmd.RunSchedule(args) }
func cmdPin(args []string)            { clicmd.RunPin(args) }
func cmdUnpin(args []string)          { clicmd.RunUnpin(args) }
func cmdUnfreeze(args []string)       { clicmd.RunUnfreeze(args) }
func cmdReplay(args []string)         { clicmd.RunReplay(args) }
func cmdShadow(args []string)         { clicmd.RunShadow(args) }
func cmdShadowReport(args []string)   { clicmd.RunShadowReport(args) }
func cmdWire(args []string)           { clicmd.RunWire(args) }
func cmdWireRecord(args []string)     { clicmd.RunWireRecordCLI(args) }
func cmdServeStatusCLI(args []string) { clicmd.RunServeStatus(args) }
func cmdDoctor(args []string)         { clicmd.RunDoctor(args) }
func cmdTakeover(args []string)       { clicmd.RunTakeover(args) }
func cmdRestore(args []string)        { clicmd.RunRestore(args) }
func cmdLogin(args []string)          { clilogin.CmdLogin(args) }
func cmdTest(args []string)           { climodels.CmdTest(args) }
