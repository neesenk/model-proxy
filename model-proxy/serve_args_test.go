package main

import "testing"

// pure_funcs_test.go covers small pure/simple functions still under 70%:
// parseServeArgs, zhipuLimitLabel, SessionCookie.

// --- parseServeArgs ---

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
