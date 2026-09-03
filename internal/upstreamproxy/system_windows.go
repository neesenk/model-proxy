//go:build windows

package upstreamproxy

import (
	"net/url"

	"golang.org/x/sys/windows/registry"
)

// detectSystemProxy reads the Windows Internet Settings proxy (manual proxy
// only; AutoConfigURL/PAC is ignored by design). Missing keys or disabled
// proxy mean "no system proxy".
func detectSystemProxy() *url.URL {
	key, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE)
	if err != nil {
		return nil
	}
	defer key.Close()
	enabled, _, err := key.GetIntegerValue("ProxyEnable")
	if err != nil || enabled == 0 {
		return nil
	}
	server, _, err := key.GetStringValue("ProxyServer")
	if err != nil {
		return nil
	}
	return parseWindowsProxyServer(server)
}
