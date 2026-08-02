package main

import (
	"model-proxy/provider"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- statusColor: all branches ---

func TestStatusColor_AllBranches(t *testing.T) {
	// Force log color on (it's off in tests: stderr is not a tty) so the
	// status→ANSI mapping is actually exercised — including branch edges.
	old := provider.LogColorEnabled
	provider.LogColorEnabled = true
	defer func() { provider.LogColorEnabled = old }()
	for _, c := range []struct {
		status int
		code   string
	}{
		{200, provider.LogAnsiGreen}, {299, provider.LogAnsiGreen},
		{300, provider.LogAnsiYellow}, {499, provider.LogAnsiYellow},
		{500, provider.LogAnsiRed},
		{100, provider.LogAnsiGray}, {0, provider.LogAnsiGray},
	} {
		want := c.code + "x" + provider.LogAnsiReset
		if got := provider.StatusColor(c.status, "x"); got != want {
			t.Errorf("provider.StatusColor(%d)=%q, want %q", c.status, got, want)
		}
	}
	// Color off: identity passthrough.
	provider.LogColorEnabled = false
	if got := provider.StatusColor(200, "ok"); got != "ok" {
		t.Errorf("provider.StatusColor(200) with color off=%q, want ok", got)
	}
}

// --- cmdConfig init: writes config.yaml in the CWD ---

func TestCLI_ConfigInit(t *testing.T) {
	dir := t.TempDir()
	// The subprocess runs `config init` which writes "config.yaml" in its CWD.
	// We can't set CWD via runCLI directly; instead, use the helper subprocess
	// but chdir via a wrapper. Simpler: call cmdConfig init in-process in a temp
	// CWD (it doesn't os.Exit on success).
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	cmdConfig([]string{"init"})
	data, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatalf("config init did not write config.yaml: %v", err)
	}
	if !strings.Contains(string(data), "providers:") {
		t.Errorf("config init wrote unexpected content:\n%s", data)
	}
}

// --- cmdConfig print: prints listen + providers + routes ---

func TestCLI_ConfigPrint(t *testing.T) {
	cfgPath := writeTempConfig(t, minimalConfig)
	// Run in-process: cmdConfig print reads cliframework.ConfigPath(args[1:]) where args[0]=="print".
	out := grabStdout(t, func() {
		cmdConfig([]string{"print", "--config", cfgPath})
	})
	if !strings.Contains(out, "listen:") || !strings.Contains(out, "aqp") {
		t.Errorf("config print missing content:\n%s", out)
	}
}
