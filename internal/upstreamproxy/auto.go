// auto.go — the process-wide automatic chain (config global → env → system →
// direct) used by non-forwarding outbound calls (usage/quota/models fetches,
// login, pricing). The composition root publishes the config-level default
// with SetDefaultProxy on startup and every reload; AutoTransport reads it
// atomically per request, so reloads take effect without rebuilding clients.
package upstreamproxy

import (
	"net/http"
	"net/url"
	"sync/atomic"
)

// defaultProxy holds the config-level global proxy URL ("" = unset, off
// markers included). Written only by the composition root.
var defaultProxy atomic.Value // string

// SetDefaultProxy publishes the top-level config `proxy` value for the
// automatic chain. Called once at startup and on every config reload.
func SetDefaultProxy(value string) {
	defaultProxy.Store(value)
}

func configuredDefault() string {
	if v := defaultProxy.Load(); v != nil {
		return v.(string)
	}
	return ""
}

// sharedResolver backs AutoTransport; system detection is cached once per
// process inside NewResolver.
var sharedResolver = NewResolver()

// AutoTransport returns an http.RoundTripper whose proxy follows the
// automatic chain: config global proxy → environment → OS system proxy →
// direct. Loopback destinations bypass the system-proxy hop (and the env hop
// via http.ProxyFromEnvironment); an explicitly configured proxy URL applies
// to loopback too.
func AutoTransport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = func(req *http.Request) (*url.URL, error) {
		setting := configuredDefault()
		if setting != "" {
			if isOff(setting) {
				return nil, nil
			}
			u, err := parseProxyURL(setting)
			if err != nil {
				// Load-time validation normally rejects bad values; a bad value
				// that still arrives must not silently drop the proxy policy.
				return nil, err
			}
			return staticProxy(u)(req)
		}
		if envHasProxy() {
			return http.ProxyFromEnvironment(req)
		}
		if u := sharedResolver.SystemProxy(); u != nil {
			return loopbackBypassProxy(u)(req)
		}
		return nil, nil
	}
	return transport
}
