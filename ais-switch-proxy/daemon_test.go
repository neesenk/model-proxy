package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseServeArgs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		env  string
		want serveArgs
	}{
		{"daemon", []string{"--daemon"}, "", serveArgs{config: "config.yaml", daemon: true}},
		{"daemon shorthand", []string{"-d"}, "", serveArgs{config: "config.yaml", daemon: true}},
		{"config + daemon", []string{"--config", "/c.yaml", "--daemon"}, "",
			serveArgs{config: "/c.yaml", daemon: true}},
		{"config=", []string{"--config=/x.yaml"}, "", serveArgs{config: "/x.yaml"}},
		{"log-file", []string{"--log-file", "/var/log/p.log"}, "",
			serveArgs{config: "config.yaml", logFile: "/var/log/p.log"}},
		{"log-file=", []string{"--log-file=/y.log"}, "",
			serveArgs{config: "config.yaml", logFile: "/y.log"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			old := os.Getenv("AIS_SWITCH_PROXY_CONFIG")
			os.Setenv("AIS_SWITCH_PROXY_CONFIG", tc.env)
			defer os.Setenv("AIS_SWITCH_PROXY_CONFIG", old)
			got := parseServeArgs(tc.args)
			if got != tc.want {
				t.Errorf("got %+v want %+v", got, tc.want)
			}
		})
	}
}

func TestParseServeArgs_EnvOverridesConfig(t *testing.T) {
	old := os.Getenv("AIS_SWITCH_PROXY_CONFIG")
	os.Setenv("AIS_SWITCH_PROXY_CONFIG", "/from-env.yaml")
	defer os.Setenv("AIS_SWITCH_PROXY_CONFIG", old)
	got := parseServeArgs([]string{"--config", "/from-flag.yaml"})
	if got.config != "/from-env.yaml" {
		t.Errorf("env should override --config; got %q", got.config)
	}
}

func TestResolveLogFile(t *testing.T) {
	cfg := &Config{LogFile: "/from/config.log"}
	sa := serveArgs{config: "/etc/ais-switch-proxy/config.yaml"}

	// flag overrides config
	if got := resolveLogFile(serveArgs{config: sa.config, logFile: "/flag.log"}, cfg); got != "/flag.log" {
		t.Errorf("flag: got %q", got)
	}
	// config used when no flag
	if got := resolveLogFile(sa, cfg); got != "/from/config.log" {
		t.Errorf("config: got %q", got)
	}
	// default: the OS temp dir (runtime artifacts), e.g. /tmp on Linux, $TMPDIR on macOS
	wantDefault := filepath.Join(os.TempDir(), "ais-switch-proxy.log")
	if got := resolveLogFile(serveArgs{config: "/etc/ais-switch-proxy/config.yaml"}, &Config{}); got != wantDefault {
		t.Errorf("default: got %q want %q", got, wantDefault)
	}
	// ~ expansion
	if got := resolveLogFile(serveArgs{config: "x", logFile: "~/p.log"}, &Config{}); got != filepath.Join(homeDir(), "p.log") {
		t.Errorf("expand: got %q", got)
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

