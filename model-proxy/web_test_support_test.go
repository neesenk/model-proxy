package main

import "testing"

func newTestWeb(t *testing.T) (*webServer, *Proxy) {
	t.Helper()
	cfg, _ := LoadConfigFromBytes("test", []byte(`listen: 127.0.0.1:0
providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://x}
`))
	p := newTestProxy(t, cfg)
	return newWebServer(p, "test-config.yaml"), p
}
