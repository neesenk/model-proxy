package main

import (
	"net/http"
	"net/url"
	"testing"
)

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

// --- zhipuLimitLabel ---

func TestZhipuLimitLabel(t *testing.T) {
	for _, tc := range []struct {
		typ, want string
		unit      int
	}{
		{"TOKENS_LIMIT", "5h tokens", 3},
		{"TOKENS_LIMIT", "Weekly tokens", 6},
		{"TOKENS_LIMIT", "Tokens (unit=9)", 9},
		{"TIME_LIMIT", "Monthly time", 5},
		{"TIME_LIMIT", "Time (unit=7)", 7},
		{"OTHER", "OTHER (unit=1)", 1},
	} {
		if got := zhipuLimitLabel(tc.typ, tc.unit); got != tc.want {
			t.Errorf("zhipuLimitLabel(%q,%d)=%q want %q", tc.typ, tc.unit, got, tc.want)
		}
	}
}

// --- SessionCookie: returns "SSO_C=val" when the jar has one, "" otherwise ---

func TestSessionCookie_Empty(t *testing.T) {
	c := newAqpClient("/tmp/nope.json")
	if got := c.SessionCookie(); got != "" {
		t.Errorf("SessionCookie with empty jar=%q want empty", got)
	}
}

func TestSessionCookie_WithCookie(t *testing.T) {
	c := newAqpClient("/tmp/nope.json")
	u, _ := url.Parse(aqpBase)
	c.Jar.SetCookies(u, []*http.Cookie{{Name: ssoCookieName, Value: "val123"}})
	got := c.SessionCookie()
	want := "SSO_C=val123"
	if got != want {
		t.Errorf("SessionCookie=%q want %q", got, want)
	}
}
