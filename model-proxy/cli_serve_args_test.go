package main

import (
	"testing"

	cliserve "model-proxy/internal/cli/serve"
)

func TestParseServeArgs(t *testing.T) {
	// All cases use an explicit --config so the result doesn't depend on whether
	// ~/.model-proxy/config.yaml exists on the test host.
	cases := []struct {
		name string
		args []string
		want cliserve.Args
	}{
		{"bare config", []string{"--config", "c.yaml"}, cliserve.Args{Config: "c.yaml"}},
		{"config=", []string{"--config=/x.yaml"}, cliserve.Args{Config: "/x.yaml"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cliserve.ParseArgs(tc.args)
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
			sa := cliserve.ParseArgs(tc.args)
			if sa.Config != tc.wantCfg {
				t.Errorf("config=%q want %q", sa.Config, tc.wantCfg)
			}
			if sa.LogFile != tc.wantLog {
				t.Errorf("logFile=%q want %q", sa.LogFile, tc.wantLog)
			}
		})
	}
}
