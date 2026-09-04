package upstreamproxy

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// clearProxyEnv pins every proxy env var to empty so chain tests are
// independent of the developer machine / CI environment.
func clearProxyEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "NO_PROXY", "no_proxy"} {
		t.Setenv(name, "")
	}
}

func systemStub(rawurl string) func() *url.URL {
	return func() *url.URL {
		if rawurl == "" {
			return nil
		}
		u, _ := url.Parse(rawurl)
		return u
	}
}

func TestResolveChainPriority(t *testing.T) {
	clearProxyEnv(t)
	t.Setenv("HTTPS_PROXY", "http://env-proxy:3128")
	resolver := &Resolver{SystemProxy: systemStub("http://sys-proxy:8080")}

	tests := []struct {
		name        string
		global      string
		perProvider string
		wantKey     string
	}{
		{name: "provider beats global env and system", global: "http://global:1", perProvider: "http://provider:2", wantKey: "url:http://provider:2"},
		{name: "global beats env and system", global: "http://global:1", wantKey: "url:http://global:1"},
		{name: "provider off short-circuits global", global: "http://global:1", perProvider: "off", wantKey: "direct"},
		{name: "provider direct alias short-circuits", global: "http://global:1", perProvider: "direct", wantKey: "direct"},
		{name: "env beats system", wantKey: "env"},
		{name: "global off beats env and system", global: "off", wantKey: "direct"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			key, _, err := resolver.Resolve(test.global, test.perProvider)
			if err != nil {
				t.Fatalf("Resolve(%q, %q) error = %v", test.global, test.perProvider, err)
			}
			if key != test.wantKey {
				t.Fatalf("Resolve(%q, %q) key = %q, want %q", test.global, test.perProvider, key, test.wantKey)
			}
		})
	}
}

func TestResolveSystemAndDirectFallback(t *testing.T) {
	clearProxyEnv(t)

	withSystem := &Resolver{SystemProxy: systemStub("http://sys-proxy:8080")}
	key, proxyFunc, err := withSystem.Resolve("", "")
	if err != nil {
		t.Fatalf("Resolve error = %v", err)
	}
	if key != "system:http://sys-proxy:8080" {
		t.Fatalf("key = %q, want system:http://sys-proxy:8080", key)
	}
	if proxyFunc == nil {
		t.Fatal("system hop must carry a proxy func")
	}

	noSystem := &Resolver{SystemProxy: systemStub("")}
	key, proxyFunc, err = noSystem.Resolve("", "")
	if err != nil {
		t.Fatalf("Resolve error = %v", err)
	}
	if key != "direct" || proxyFunc != nil {
		t.Fatalf("Resolve with nothing set = (%q, %v), want (direct, nil)", key, proxyFunc)
	}
}

func TestResolveRejectsInvalidURL(t *testing.T) {
	clearProxyEnv(t)
	resolver := &Resolver{SystemProxy: systemStub("")}
	if _, _, err := resolver.Resolve("", "ftp://proxy:21"); err == nil {
		t.Fatal("Resolve with ftp scheme must fail")
	}
	if _, _, err := resolver.Resolve("http://", ""); err == nil {
		t.Fatal("Resolve with empty host must fail")
	}
}

func TestLoopbackBypassProxy(t *testing.T) {
	u, _ := url.Parse("http://proxy:8080")
	proxyFunc := loopbackBypassProxy(u)
	for _, target := range []string{"http://127.0.0.1:8317/v1", "http://localhost:8317/v1", "http://[::1]:8317/v1", "http://127.0.0.2/v1"} {
		req, _ := http.NewRequest(http.MethodPost, target, nil)
		proxy, err := proxyFunc(req)
		if err != nil || proxy != nil {
			t.Fatalf("loopbackBypassProxy(%s) = (%v, %v), want direct", target, proxy, err)
		}
	}
	req, _ := http.NewRequest(http.MethodPost, "https://api.example.com/v1", nil)
	proxy, err := proxyFunc(req)
	if err != nil || proxy == nil || proxy.String() != u.String() {
		t.Fatalf("loopbackBypassProxy(remote) = (%v, %v), want %s", proxy, err, u)
	}
}

func TestStaticProxyAppliesToLoopback(t *testing.T) {
	u, _ := url.Parse("http://proxy:8080")
	req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:8317/v1", nil)
	proxy, err := staticProxy(u)(req)
	if err != nil || proxy == nil || proxy.String() != u.String() {
		t.Fatalf("staticProxy(loopback) = (%v, %v) — an explicitly configured proxy is user intent and applies to loopback too", proxy, err)
	}
}

func TestValidateSetting(t *testing.T) {
	valid := []string{"", "off", "OFF", "direct", "http://proxy:8080", "https://user:pass@proxy:8443", "socks5://proxy:1080"}
	for _, s := range valid {
		if err := ValidateSetting(s); err != nil {
			t.Errorf("ValidateSetting(%q) = %v, want nil", s, err)
		}
	}
	invalid := []string{"ftp://proxy:21", "http://", "://nohost", "proxy:8080"}
	for _, s := range invalid {
		if err := ValidateSetting(s); err == nil {
			t.Errorf("ValidateSetting(%q) = nil, want error", s)
		}
	}
}

func TestAutoTransportHonorsChain(t *testing.T) {
	clearProxyEnv(t)
	t.Cleanup(func() { SetDefaultProxy("") })

	transport := AutoTransport()
	proxyOf := func(target string) *url.URL {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, target, nil)
		u, err := transport.Proxy(req)
		if err != nil {
			t.Fatalf("Proxy(%s) error = %v", target, err)
		}
		return u
	}

	SetDefaultProxy("http://cfg-proxy:9000")
	if got := proxyOf("https://api.example.com"); got == nil || got.String() != "http://cfg-proxy:9000" {
		t.Fatalf("with config default, proxy = %v, want http://cfg-proxy:9000", got)
	}
	if got := proxyOf("http://127.0.0.1:8317"); got == nil || got.String() != "http://cfg-proxy:9000" {
		t.Fatalf("explicit config proxy applies to loopback too, got %v", got)
	}

	SetDefaultProxy("off")
	if got := proxyOf("https://api.example.com"); got != nil {
		t.Fatalf("off must force direct, got %v", got)
	}

	SetDefaultProxy("")
	t.Setenv("HTTPS_PROXY", "http://env-proxy:3128")
	if got := proxyOf("https://api.example.com"); got == nil || got.String() != "http://env-proxy:3128" {
		t.Fatalf("with env proxy, proxy = %v, want http://env-proxy:3128", got)
	}
}

// TestValidateSettingRedactsUserinfo pins the credential rule: proxy URLs may
// carry userinfo, and parse errors surface in config validation and warn
// logs — the error must never echo the credentials back.
func TestValidateSettingRedactsUserinfo(t *testing.T) {
	for _, bad := range []string{
		"socks5h://user:secret@proxy.example.com", // unsupported scheme
		"http://user:secret@",                     // missing host
	} {
		err := ValidateSetting(bad)
		if err == nil {
			t.Fatalf("ValidateSetting(%q) = nil, want error", bad)
		}
		if strings.Contains(err.Error(), "user:secret") || strings.Contains(err.Error(), "secret") {
			t.Errorf("ValidateSetting(%q) error leaks credentials: %v", bad, err)
		}
	}
}

func TestIsLoopback(t *testing.T) {
	for _, host := range []string{
		"localhost", "LOCALHOST", "127.0.0.1", "127.1.2.3", "::1",
		"0:0:0:0:0:0:0:1", "::ffff:127.0.0.1", "::FFFF:127.0.0.1",
	} {
		if !isLoopback(host) {
			t.Errorf("isLoopback(%q) = false, want true", host)
		}
	}
	for _, host := range []string{"example.com", "10.0.0.1", "::ffff:10.0.0.1", "128.0.0.1"} {
		if isLoopback(host) {
			t.Errorf("isLoopback(%q) = true, want false", host)
		}
	}
}

// TestAutoTransportSharedInstance pins the connection-reuse contract:
// AutoTransport must return the same process-lifetime transport on every
// call, so maintenance call sites share its connection pool instead of each
// cloning and discarding one.
func TestAutoTransportSharedInstance(t *testing.T) {
	if AutoTransport() != AutoTransport() {
		t.Fatal("AutoTransport() returned distinct transports; call sites cannot share the pool")
	}
}
