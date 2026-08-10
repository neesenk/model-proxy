package main

import climodels "model-proxy/internal/cli/models"

// cmdTest delegates to internal/cli/models.CmdTest.
func cmdTest(args []string) { climodels.CmdTest(args) }
