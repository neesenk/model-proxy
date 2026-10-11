package framework

import (
	"net/http"
	"sync"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/upstreamproxy"
)

// providerResolver is the CLI-process twin of the daemon's proxy resolver
// (internal/app proxy_transport.go): one process-lifetime system-proxy
// detection, shared by every per-provider client below.
var providerResolver = upstreamproxy.NewResolver()

var (
	providerClientsMu sync.Mutex
	providerClients   = map[string]*http.Client{}
)

// ProviderClient returns an HTTP client whose transport follows the FULL
// per-provider proxy resolution — providers.<name>.proxy_url → global proxy →
// env → system proxy → direct — the same order the daemon's forward path
// uses. One-shot CLI probes (test / models refresh / wire) must resolve per
// target: probing on the shared AutoTransport chain would honor only the
// global setting and silently ignore a provider-scoped proxy_url (codex via
// mihomo under a global `proxy: off` was the observed miss).
func ProviderClient(cfg *configdomain.Config, provCfg *configdomain.Provider) *http.Client {
	global := ""
	if cfg != nil {
		global = cfg.Proxy
	}
	perProvider := ""
	if provCfg != nil {
		perProvider = provCfg.ProxyURL
	}
	key, proxyFunc, err := providerResolver.Resolve(global, perProvider)
	if err != nil {
		// Load-time validation rejects bad values; keep the historical
		// default rather than silently downgrading to direct.
		return &http.Client{Transport: upstreamproxy.AutoTransport()}
	}
	providerClientsMu.Lock()
	defer providerClientsMu.Unlock()
	if client, ok := providerClients[key]; ok {
		return client
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = proxyFunc
	client := &http.Client{Transport: transport}
	providerClients[key] = client
	return client
}
