// auto.go — the process-wide automatic chain (config global → env → system →
// direct) used by non-forwarding outbound calls that carry no provider
// override (pricing, login, and provider maintenance calls whose proxy_url is
// unset). The composition root publishes the config-level default with
// SetDefaultProxy on startup and every reload; AutoTransport reads it
// atomically per request, so reloads take effect without rebuilding clients.
// Provider-scoped maintenance calls resolve through ProviderTransport below.
package upstreamproxy

import (
	"net/http"
	"net/url"
	"strings"
	"sync"
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

// autoTransport is the single process-lifetime transport for the automatic
// chain. AutoTransport used to clone a fresh http.Transport per call and the
// ~25 maintenance call sites (usage/login/pricing/models fetches) handed the
// one-shot transports to one-shot clients — no connection reuse across calls,
// and each discarded transport kept idle connections and their goroutines
// alive until the 90s idle timeout. One shared transport fixes both; its
// Proxy closure re-reads configuredDefault() per request, so reloads still
// take effect immediately without rebuilding anything.
var autoTransport = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = autoProxyFunc
	return t
}()

// AutoTransport returns the process-shared transport whose proxy follows the
// automatic chain: config global proxy → environment → OS system proxy →
// direct. Loopback destinations bypass the system-proxy hop (and the env hop
// via http.ProxyFromEnvironment); an explicitly configured proxy URL applies
// to loopback too. The same instance is returned on every call — treat it as
// read-only (never Close it, never mutate its fields).
func AutoTransport() *http.Transport {
	return autoTransport
}

func autoProxyFunc(req *http.Request) (*url.URL, error) {
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

// providerTransports pools the per-provider maintenance transports (usage /
// quota / mint / models fetches) keyed by the raw proxy_url setting — same
// pooling rationale as the process-shared autoTransport, extended to one slot
// per distinct provider override. Empty setting shares the auto chain.
var (
	providerTransportsMu sync.Mutex
	providerTransports   = map[string]*http.Transport{}
)

// ProviderTransport returns the shared transport for ONE provider's
// account-maintenance calls: a non-empty proxy_url (URL or `off`) resolves
// exactly like the forward path's per-provider override, and an empty one
// falls to the automatic chain (config global proxy → env → system →
// direct). Load-time validation rejects bad values, so parse failures here
// degrade to the automatic chain rather than failing closed mid-request.
func ProviderTransport(perProvider string) *http.Transport {
	setting := strings.TrimSpace(perProvider)
	if setting == "" {
		return autoTransport
	}
	providerTransportsMu.Lock()
	defer providerTransportsMu.Unlock()
	if t, ok := providerTransports[setting]; ok {
		return t
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConnsPerHost = 16
	switch {
	case isOff(setting):
		t.Proxy = nil
	default:
		u, err := parseProxyURL(setting)
		if err != nil {
			providerTransports[setting] = autoTransport
			return autoTransport
		}
		t.Proxy = staticProxy(u)
	}
	providerTransports[setting] = t
	return t
}
