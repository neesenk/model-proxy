package main

import (
	clilogin "model-proxy/internal/cli/login"
	"net/http"
	"net/url"
	"testing"

	"model-proxy/provider"
)

func TestPublicCookies(t *testing.T) {
	// nil jar -> nil.
	c := clilogin.NewAqpClient("/tmp/nope.json")
	if got := c.PublicCookies(); got != nil {
		t.Errorf("PublicCookies(nil jar)=%v want nil", got)
	}
	// With a cookie set on the jar for c.base.
	u, _ := url.Parse(provider.AqpBase)
	c2 := clilogin.NewAqpClient("/tmp/nope.json")
	c2.Jar.SetCookies(u, []*http.Cookie{{Name: provider.SsoCookieName, Value: "v"}})
	got := c2.PublicCookies()
	if len(got) != 1 || got[0].Name != provider.SsoCookieName {
		t.Errorf("PublicCookies=%+v want [%s=v]", got, provider.SsoCookieName)
	}
}

// --- truncate (gateway.go) ---

func TestTruncate(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Errorf("truncate(short)=%q", got)
	}
	if got := truncate("abcdef", 3); got != "abc..." {
		t.Errorf("truncate(abcdef,3)=%q want abc...", got)
	}
	if got := truncate("exact", 5); got != "exact" {
		t.Errorf("truncate(exact,5)=%q want exact", got)
	}
}
