package main

import (
	"net/http"
	"net/url"
	"testing"

	"model-proxy/provider"
)

func TestPublicCookies(t *testing.T) {
	// nil jar -> nil.
	c := newAqpClient("/tmp/nope.json")
	if got := c.PublicCookies(); got != nil {
		t.Errorf("PublicCookies(nil jar)=%v want nil", got)
	}
	// With a cookie set on the jar for c.base.
	u, _ := url.Parse(provider.AqpBase)
	c2 := newAqpClient("/tmp/nope.json")
	c2.Jar.SetCookies(u, []*http.Cookie{{Name: provider.SsoCookieName, Value: "v"}})
	got := c2.PublicCookies()
	if len(got) != 1 || got[0].Name != provider.SsoCookieName {
		t.Errorf("PublicCookies=%+v want [%s=v]", got, provider.SsoCookieName)
	}
}
