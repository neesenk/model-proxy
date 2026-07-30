package main

import (
	clilogin "model-proxy/internal/cli/login"
	"net/http"
	"net/url"
	"testing"

	"model-proxy/provider"
)

// --- zhipuLimitLabel: moved to provider/quota_parse_test.go (Phase 1) ---

// --- SessionCookie: returns "SSO_C=val" when the jar has one, "" otherwise ---

func TestSessionCookie_Empty(t *testing.T) {
	c := clilogin.NewAqpClient("/tmp/nope.json")
	if got := c.SessionCookie(); got != "" {
		t.Errorf("SessionCookie with empty jar=%q want empty", got)
	}
}

func TestSessionCookie_WithCookie(t *testing.T) {
	c := clilogin.NewAqpClient("/tmp/nope.json")
	u, _ := url.Parse(provider.AqpBase)
	c.Jar.SetCookies(u, []*http.Cookie{{Name: provider.SsoCookieName, Value: "val123"}})
	got := c.SessionCookie()
	want := "SSO_C=val123"
	if got != want {
		t.Errorf("SessionCookie=%q want %q", got, want)
	}
}
