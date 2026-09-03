package config

import (
	"strings"
	"testing"
)

const proxyTestBase = `listen: 127.0.0.1:1
providers:
  zhipu:
    provider_id: zhipu
    openai_base_url: https://example.com/api/v1
`

func loadProxyTestConfig(t *testing.T, extra string) (*Config, error) {
	t.Helper()
	return LoadConfigFromBytes("proxy_test", []byte(proxyTestBase+extra))
}

func TestLoadProxySettings(t *testing.T) {
	t.Run("global proxy URL accepted", func(t *testing.T) {
		cfg, err := loadProxyTestConfig(t, "proxy: socks5://127.0.0.1:1080\n")
		if err != nil {
			t.Fatalf("LoadConfigFromBytes error = %v", err)
		}
		if cfg.Proxy != "socks5://127.0.0.1:1080" {
			t.Fatalf("Proxy = %q", cfg.Proxy)
		}
	})
	t.Run("global off accepted", func(t *testing.T) {
		if _, err := loadProxyTestConfig(t, "proxy: off\n"); err != nil {
			t.Fatalf("LoadConfigFromBytes error = %v", err)
		}
	})
	t.Run("invalid global scheme rejected", func(t *testing.T) {
		_, err := loadProxyTestConfig(t, "proxy: ftp://proxy:21\n")
		if err == nil || !strings.Contains(err.Error(), "proxy") {
			t.Fatalf("error = %v, want proxy scheme rejection", err)
		}
	})
	t.Run("provider proxy_url accepted", func(t *testing.T) {
		cfg, err := LoadConfigFromBytes("proxy_test", []byte(strings.Replace(
			proxyTestBase,
			"openai_base_url: https://example.com/api/v1",
			"openai_base_url: https://example.com/api/v1\n    proxy_url: http://127.0.0.1:7890",
			1,
		)))
		if err != nil {
			t.Fatalf("LoadConfigFromBytes error = %v", err)
		}
		if cfg.Providers["zhipu"].ProxyURL != "http://127.0.0.1:7890" {
			t.Fatalf("ProxyURL = %q", cfg.Providers["zhipu"].ProxyURL)
		}
	})
	t.Run("invalid provider proxy_url rejected", func(t *testing.T) {
		_, err := LoadConfigFromBytes("proxy_test", []byte(strings.Replace(
			proxyTestBase,
			"openai_base_url: https://example.com/api/v1",
			"openai_base_url: https://example.com/api/v1\n    proxy_url: proxy:8080",
			1,
		)))
		if err == nil || !strings.Contains(err.Error(), "proxy_url") {
			t.Fatalf("error = %v, want proxy_url rejection", err)
		}
	})
}
