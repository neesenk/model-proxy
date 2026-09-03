// Package upstreamproxy owns the outbound (upstream) proxy policy for every
// HTTP call model-proxy makes: the per-provider/global config chain, the
// environment fallback, and OS system-proxy detection.
//
// Resolution chain for the forwarding path (first hit wins):
//
//	providers.<name>.proxy_url → top-level proxy → HTTPS_PROXY/HTTP_PROXY
//	(+ NO_PROXY) → OS system proxy → direct
//
// The special value "off" (alias "direct") at either config level forces a
// direct connection and stops the chain. Explicit proxy URLs support the
// http, https and socks5 schemes (userinfo allowed). System-proxy mode always
// bypasses loopback so local services and tests are never captured by a
// machine-wide proxy.
//
// System proxy settings are detected once per process (sync.Once), mirroring
// net/http's ProxyFromEnvironment caching; changing OS settings requires a
// restart. PAC/WPAD (macOS AutoProxy, Windows AutoConfigURL) is not evaluated.
package upstreamproxy

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
)

// ProxyFunc matches http.Transport.Proxy.
type ProxyFunc func(*http.Request) (*url.URL, error)

// Resolver resolves the effective proxy for one provider from the config
// chain. SystemProxy is the only platform-dependent seam: NewResolver binds
// the cached OS detection, tests substitute a stub.
type Resolver struct {
	SystemProxy func() *url.URL
}

// NewResolver returns a Resolver backed by once-per-process OS system-proxy
// detection.
func NewResolver() *Resolver {
	var once sync.Once
	var cached *url.URL
	return &Resolver{SystemProxy: func() *url.URL {
		once.Do(func() { cached = detectSystemProxy() })
		return cached
	}}
}

// Resolve maps the config chain onto a transport-cache key and a ProxyFunc.
// perProvider overrides global; either may be "off"/"direct" to force a
// direct connection. With both empty the chain falls back to the environment
// (HTTPS_PROXY/HTTP_PROXY/NO_PROXY via http.ProxyFromEnvironment), then the
// OS system proxy, then direct. The key is stable for identical effective
// settings so callers can pool transports by it.
func (r *Resolver) Resolve(global, perProvider string) (string, ProxyFunc, error) {
	for _, setting := range []string{perProvider, global} {
		setting = strings.TrimSpace(setting)
		if setting == "" {
			continue
		}
		if isOff(setting) {
			return "direct", nil, nil
		}
		u, err := parseProxyURL(setting)
		if err != nil {
			return "", nil, err
		}
		return "url:" + u.String(), staticProxy(u), nil
	}
	if envHasProxy() {
		return "env", http.ProxyFromEnvironment, nil
	}
	if r != nil && r.SystemProxy != nil {
		if u := r.SystemProxy(); u != nil {
			return "system:" + u.String(), loopbackBypassProxy(u), nil
		}
	}
	return "direct", nil, nil
}

// ValidateSetting reports whether s is a legal proxy setting: empty, off,
// direct, or an http/https/socks5 URL.
func ValidateSetting(s string) error {
	s = strings.TrimSpace(s)
	if s == "" || isOff(s) {
		return nil
	}
	_, err := parseProxyURL(s)
	return err
}

func isOff(s string) bool {
	return strings.EqualFold(s, "off") || strings.EqualFold(s, "direct")
}

func parseProxyURL(s string) (*url.URL, error) {
	u, err := url.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("invalid proxy URL %q: %w", s, err)
	}
	switch u.Scheme {
	case "http", "https", "socks5":
	default:
		return nil, fmt.Errorf("invalid proxy URL %q: scheme must be http, https or socks5", s)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("invalid proxy URL %q: missing host", s)
	}
	return u, nil
}

// staticProxy proxies everything through u. Explicitly configured proxies
// apply to loopback destinations too — an explicit setting is user intent
// (e.g. a local debugging proxy).
func staticProxy(u *url.URL) ProxyFunc {
	return func(*http.Request) (*url.URL, error) {
		return u, nil
	}
}

// loopbackBypassProxy proxies everything through u except loopback
// destinations: an auto-detected system proxy must never capture local
// services (or tests).
func loopbackBypassProxy(u *url.URL) ProxyFunc {
	return func(req *http.Request) (*url.URL, error) {
		if isLoopback(req.URL.Hostname()) {
			return nil, nil
		}
		return u, nil
	}
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	switch host {
	case "127.0.0.1", "::1":
		return true
	}
	if strings.HasPrefix(host, "127.") {
		return true
	}
	return false
}

// envHasProxy reports whether any proxy environment variable is set. It only
// decides WHICH chain link applies; the per-request bypass semantics
// (NO_PROXY, loopback) stay with http.ProxyFromEnvironment.
func envHasProxy() bool {
	for _, name := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		if os.Getenv(name) != "" {
			return true
		}
	}
	return false
}
