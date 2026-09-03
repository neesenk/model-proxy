package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// recordingHTTPProxy is a minimal forward proxy: it counts absolute-form
// requests and relays them to the origin with a direct transport.
type recordingHTTPProxy struct {
	server *httptest.Server
	hits   atomic.Int64
}

func newRecordingHTTPProxy(t *testing.T) *recordingHTTPProxy {
	t.Helper()
	proxy := &recordingHTTPProxy{}
	proxy.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy.hits.Add(1)
		if !r.URL.IsAbs() {
			http.Error(w, "proxy received origin-form request", http.StatusBadRequest)
			return
		}
		out := r.Clone(r.Context())
		out.RequestURI = ""
		resp, err := http.DefaultTransport.RoundTrip(out)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for key, values := range resp.Header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(proxy.server.Close)
	return proxy
}

func proxyTestConfig(upstreamURL string) *Config {
	return &Config{
		Listen: "127.0.0.1:0",
		Providers: map[string]Provider{
			"proxied": {Provider: "static", OpenAIBaseURL: upstreamURL, Models: []string{"m"}},
			"plain":   {Provider: "static", OpenAIBaseURL: upstreamURL, Models: []string{"m"}},
		},
	}
}

func TestClientForPerProviderProxy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer upstream.Close()
	proxy := newRecordingHTTPProxy(t)

	cfg := proxyTestConfig(upstream.URL)
	cfg.Providers["proxied"] = Provider{
		Provider: "static", OpenAIBaseURL: upstream.URL, Models: []string{"m"},
		ProxyURL: proxy.server.URL,
	}
	p := newTestProxy(t, cfg)

	post := func(provider string) string {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, upstream.URL+"/v1/chat/completions", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := p.clientFor(cfg, nil, provider).Do(req)
		if err != nil {
			t.Fatalf("%s: %v", provider, err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return string(body)
	}

	if body := post("proxied"); body != "ok" {
		t.Fatalf("proxied request body = %q", body)
	}
	if proxy.hits.Load() != 1 {
		t.Fatalf("proxy hits after proxied provider = %d, want 1", proxy.hits.Load())
	}
	if body := post("plain"); body != "ok" {
		t.Fatalf("plain request body = %q", body)
	}
	if proxy.hits.Load() != 1 {
		t.Fatalf("proxy hits after unproxied provider = %d, want still 1", proxy.hits.Load())
	}
}

func TestClientForGlobalProxyAndOffOverride(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer upstream.Close()
	proxy := newRecordingHTTPProxy(t)

	cfg := proxyTestConfig(upstream.URL)
	cfg.Proxy = proxy.server.URL
	cfg.Providers["plain"] = Provider{
		Provider: "static", OpenAIBaseURL: upstream.URL, Models: []string{"m"},
		ProxyURL: "off",
	}
	p := newTestProxy(t, cfg)

	post := func(provider string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, upstream.URL+"/v1/chat/completions", nil)
		resp, err := p.clientFor(cfg, nil, provider).Do(req)
		if err != nil {
			t.Fatalf("%s: %v", provider, err)
		}
		resp.Body.Close()
	}

	post("proxied") // inherits the global proxy
	if proxy.hits.Load() != 1 {
		t.Fatalf("proxy hits after global-proxy provider = %d, want 1", proxy.hits.Load())
	}
	post("plain") // proxy_url: off forces direct
	if proxy.hits.Load() != 1 {
		t.Fatalf("proxy hits after off provider = %d, want still 1", proxy.hits.Load())
	}
}

func TestPooledTransportReuse(t *testing.T) {
	cfg := proxyTestConfig("http://127.0.0.1:1")
	cfg.Proxy = "http://proxy.example:8080"
	p := newTestProxy(t, cfg)

	first := p.clientFor(cfg, nil, "proxied").(*http.Client).Transport
	second := p.clientFor(cfg, nil, "plain").(*http.Client).Transport
	if first != second {
		t.Fatal("same effective proxy must share one pooled transport")
	}
	direct := p.clientFor(proxyTestConfig("http://127.0.0.1:1"), nil, "proxied").(*http.Client).Transport
	if direct == first {
		t.Fatal("different effective proxies must not share a transport")
	}
}
