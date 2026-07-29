package main

import "testing"

func TestParseServeArgs(t *testing.T) {
	// All cases use an explicit --config so the result doesn't depend on whether
	// ~/.model-proxy/config.yaml exists on the test host.
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

func TestParseServeArgs_Extra(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		wantCfg string
		wantLog string
	}{
		{"empty", []string{}, "config.yaml", ""},
		{"config only", []string{"--config", "x.yaml"}, "x.yaml", ""},
		{"log-file value", []string{"--log-file", "/tmp/x.log"}, "config.yaml", "/tmp/x.log"},
		{"log-file= form", []string{"--log-file=/tmp/x.log"}, "config.yaml", "/tmp/x.log"},
		{"both", []string{"--config", "x.yaml", "--log-file", "/tmp/x.log"}, "x.yaml", "/tmp/x.log"},
		{"log-file at end without value", []string{"--log-file"}, "config.yaml", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sa := parseServeArgs(tc.args)
			if sa.config != tc.wantCfg {
				t.Errorf("config=%q want %q", sa.config, tc.wantCfg)
			}
			if sa.logFile != tc.wantLog {
				t.Errorf("logFile=%q want %q", sa.logFile, tc.wantLog)
			}
		})
	}
}
