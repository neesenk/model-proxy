// system.go — OS system-proxy detection entry plus the pure parsers. The
// parsers are platform-neutral so every OS can unit-test them; only the
// exec/registry reads live in build-tagged files.
package upstreamproxy

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// parseScutilProxy parses `scutil --proxy` output (macOS). HTTPS entries win
// over HTTP; SOCKS is used when neither is enabled. Returns nil when no
// manual proxy is enabled. PAC (ProxyAutoConfigEnable) is ignored by design.
func parseScutilProxy(output string) *url.URL {
	values := map[string]string{}
	for _, line := range strings.Split(output, "\n") {
		key, value, ok := strings.Cut(line, " : ")
		if !ok {
			continue
		}
		values[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	enabled := func(key string) bool { return values[key] == "1" }
	hostPort := func(hostKey, portKey string) *url.URL {
		host := values[hostKey]
		if host == "" {
			return nil
		}
		port := values[portKey]
		if port == "" {
			port = "8080"
		}
		if _, err := strconv.Atoi(port); err != nil {
			return nil
		}
		return &url.URL{Scheme: "http", Host: host + ":" + port}
	}
	if enabled("HTTPSEnable") {
		if u := hostPort("HTTPSProxy", "HTTPSPort"); u != nil {
			return u
		}
	}
	if enabled("HTTPEnable") {
		if u := hostPort("HTTPProxy", "HTTPPort"); u != nil {
			return u
		}
	}
	if enabled("SOCKSEnable") {
		if u := hostPort("SOCKSProxy", "SOCKSPort"); u != nil {
			u.Scheme = "socks5"
			return u
		}
	}
	return nil
}

// parseWindowsProxyServer parses the Windows registry ProxyServer string.
// Forms: "host:port" (all protocols) or per-protocol
// "http=host:port;https=host:port;socks=host:port". Returns nil when empty.
func parseWindowsProxyServer(value string) *url.URL {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if !strings.Contains(value, "=") {
		if strings.Contains(value, ";") {
			return nil // per-protocol form without any scheme is malformed
		}
		return &url.URL{Scheme: "http", Host: value}
	}
	preference := []string{"https", "http", "socks"}
	entries := map[string]string{}
	for _, part := range strings.Split(value, ";") {
		scheme, host, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || host == "" {
			continue
		}
		entries[strings.ToLower(scheme)] = host
	}
	for _, scheme := range preference {
		if host := entries[scheme]; host != "" {
			u := &url.URL{Scheme: "http", Host: host}
			if scheme == "socks" {
				u.Scheme = "socks5"
			}
			return u
		}
	}
	return nil
}

// parseGnomeProxy builds a proxy URL from GNOME gsettings values. mode must
// be "manual"; host must be non-empty. port 0 defaults to 8080.
func parseGnomeProxy(mode, host string, port int) *url.URL {
	if mode != "manual" || host == "" {
		return nil
	}
	if port <= 0 {
		port = 8080
	}
	return &url.URL{Scheme: "http", Host: fmt.Sprintf("%s:%d", host, port)}
}
