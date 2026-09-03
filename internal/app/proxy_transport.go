// proxy_transport.go — per-provider upstream proxy resolution and the
// process-lifetime transport pool. The pool is keyed by the effective proxy
// identity (direct / env / url:<…> / system:<…>), so reload only changes
// which key a provider resolves to; in-flight requests keep their captured
// client. Config values always come from the request snapshot passed in by
// the forward pipeline (single-snapshot red line) — never re-read here.
package app

import (
	"net/http"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/observe/logx"
	"model-proxy/internal/targetexec"
	"model-proxy/internal/upstreamproxy"
)

// clientFor resolves the upstream HTTP client for one route target: the
// provider's proxy_url overrides the top-level proxy, and with neither set
// the chain falls back to env → system → direct (see internal/upstreamproxy).
func (p *Proxy) clientFor(cfg *Config, parentOf map[string]string, provider string) targetexec.Doer {
	return &http.Client{Timeout: 0, Transport: p.transportFor(cfg, parentOf, provider)}
}

// transportFor is clientFor's transport half, exposed for callers that need
// their own timeout budget (shadow).
func (p *Proxy) transportFor(cfg *Config, parentOf map[string]string, provider string) *http.Transport {
	global, perProvider := "", ""
	if cfg != nil {
		global = cfg.Proxy
		if provCfg, ok := configdomain.ProviderConfig(cfg, parentOf, provider); ok {
			perProvider = provCfg.ProxyURL
		}
	}
	key, proxyFunc, err := p.proxyResolver.Resolve(global, perProvider)
	if err != nil {
		// Load-time validation rejects bad proxy settings, so this is only
		// reachable with an unvalidated Config — keep the historical default
		// rather than silently downgrading to direct.
		logx.Warnf("[proxy] provider %s: %v — falling back to the default transport", provider, err)
		if p.client != nil {
			if transport, ok := p.client.Transport.(*http.Transport); ok {
				return transport
			}
		}
		return http.DefaultTransport.(*http.Transport)
	}
	return p.pooledTransport(key, proxyFunc)
}

// pooledTransport returns the shared transport for one effective proxy
// identity, creating it with the same pool sizing as the default client.
func (p *Proxy) pooledTransport(key string, proxyFunc upstreamproxy.ProxyFunc) *http.Transport {
	p.transportsMu.Lock()
	defer p.transportsMu.Unlock()
	if transport, ok := p.transports[key]; ok {
		return transport
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 256
	transport.MaxIdleConnsPerHost = 100
	transport.Proxy = proxyFunc
	p.transports[key] = transport
	return transport
}
