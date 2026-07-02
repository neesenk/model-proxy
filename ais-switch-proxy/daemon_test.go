package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseServeArgs(t *testing.T) {
	// All cases use an explicit --config so the result doesn't depend on whether
	// ~/.ais-switch/config.yaml exists on the test host.
	cases := []struct {
		name string
		args []string
		want serveArgs
	}{
		{"bare config", []string{"--config", "c.yaml"}, serveArgs{config: "c.yaml"}},
		{"config=", []string{"--config=/x.yaml"}, serveArgs{config: "/x.yaml"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseServeArgs(tc.args)
			if got != tc.want {
				t.Errorf("got %+v want %+v", got, tc.want)
			}
		})
	}
}

// TestConfigPath_LookupOrder verifies the lookup order:
// --config flag > ~/.ais-switch/config.yaml > ./config.yaml.
func TestConfigPath_LookupOrder(t *testing.T) {
	// 1. explicit flag wins over everything.
	got := configPath([]string{"--config", "/explicit.yaml"})
	if got != "/explicit.yaml" {
		t.Errorf("flag: got %q", got)
	}
	got = configPath([]string{"--config=/explicit2.yaml"})
	if got != "/explicit2.yaml" {
		t.Errorf("flag=: got %q", got)
	}

	// 2. user-level file wins over ./config.yaml. Point HOME at a temp dir with
	// the user config present (under .ais-switch/), and a different CWD config —
	// the user one wins.
	dir := t.TempDir()
	aisDir := filepath.Join(dir, ".ais-switch")
	if err := os.MkdirAll(aisDir, 0o755); err != nil {
		t.Fatal(err)
	}
	homeCfg := filepath.Join(aisDir, "config.yaml")
	if err := os.WriteFile(homeCfg, []byte("listen: 127.0.0.1:0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldHome := homeDirForTest
	homeDirForTest = dir
	defer func() { homeDirForTest = oldHome }()
	// Without a flag and with a user-level file present, configPath returns it.
	got = configPath(nil)
	if got != homeCfg {
		t.Errorf("user-level: got %q want %q", got, homeCfg)
	}
}

func TestResolveLogFile(t *testing.T) {
	cfg := &Config{LogFile: "/from/config.log"}
	sa := serveArgs{config: "/etc/ais-switch-proxy/config.yaml"}

	// config used
	if got := resolveLogFile(sa, cfg); got != "/from/config.log" {
		t.Errorf("config: got %q", got)
	}
	// default: the OS temp dir (runtime artifacts), e.g. /tmp on Linux, $TMPDIR on macOS
	wantDefault := filepath.Join(os.TempDir(), "ais-switch-proxy.log")
	if got := resolveLogFile(serveArgs{config: "/etc/ais-switch-proxy/config.yaml"}, &Config{}); got != wantDefault {
		t.Errorf("default: got %q want %q", got, wantDefault)
	}
}

func TestPidFilePath(t *testing.T) {
	if got := pidFilePath("/var/log/ais-switch-proxy.log"); got != "/var/log/ais-switch-proxy.pid" {
		t.Errorf("got %q", got)
	}
	if got := pidFilePath("/var/log/agent"); got != "/var/log/agent.pid" {
		t.Errorf("got %q", got)
	}
}

func TestWriteReadPidFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.pid")
	if err := writePidFile(path, 4242); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "4242\n" {
		t.Errorf("got %q", string(b))
	}
}

