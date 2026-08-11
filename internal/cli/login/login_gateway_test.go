package login

import (
	"net/http"
	"net/url"
	"testing"

	"model-proxy/internal/provider"
)

func TestPublicCookies(t *testing.T) {
	// nil jar -> nil.
	c := NewAqpClient("/tmp/nope.json")
	if got := c.PublicCookies(); got != nil {
		t.Errorf("PublicCookies(nil jar)=%v want nil", got)
	}
	// With a cookie set on the jar for c.base.
	u, _ := url.Parse(provider.AqpBase)
	c2 := NewAqpClient("/tmp/nope.json")
	c2.Jar.SetCookies(u, []*http.Cookie{{Name: provider.SsoCookieName, Value: "v"}})
	got := c2.PublicCookies()
	if len(got) != 1 || got[0].Name != provider.SsoCookieName {
		t.Errorf("PublicCookies=%+v want [%s=v]", got, provider.SsoCookieName)
	}
}

// --- truncate (gateway.go) ---

func TestTruncate(t *testing.T) {
	if got := provider.Truncate("short", 10); got != "short" {
		t.Errorf("provider.Truncate(short)=%q", got)
	}
	if got := provider.Truncate("abcdef", 3); got != "abc..." {
		t.Errorf("provider.Truncate(abcdef,3)=%q want abc...", got)
	}
	if got := provider.Truncate("exact", 5); got != "exact" {
		t.Errorf("provider.Truncate(exact,5)=%q want exact", got)
	}
}
