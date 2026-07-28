package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"model-proxy/internal/pricing"
)

const pricingIntegrationFixture = `{
  "data": [
    {"id": "z-ai/glm-4.6", "pricing": {
      "prompt": "0.0000009",
      "completion": "0.0000018",
      "input_cache_read": "0.0000001",
      "input_cache_write": "0.0000002"
    }}
  ]
}`

func TestPricingCachePathUsesApplicationHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got, want := pricingCachePath(), filepath.Join(home, ".model-proxy", "pricing_cache.json"); got != want {
		t.Errorf("pricing cache path = %q, want %q", got, want)
	}
}

func TestProxyReadViewPricingDetachesAndConvertsOverrides(t *testing.T) {
	proxy := &Proxy{cfg: &Config{
		Pricing: PricingConfig{Enabled: false},
		Prices: map[string]PriceConfig{
			"glm-4.6": {Input: 9, Output: 18, CacheRead: 1, CacheWrite: 2},
		},
	}}

	view := proxy.readView().pricing()
	if view.catalog != nil {
		t.Fatalf("disabled pricing catalog = %+v, want nil", view.catalog)
	}
	entry, ok := pricing.Resolve(view.overrides, nil, "glm-4.6")
	if !ok {
		t.Fatal("converted override was not resolvable")
	}
	if entry.Prompt != 9e-6 || entry.Completion != 18e-6 ||
		entry.CacheRead != 1e-6 || entry.CacheWrite != 2e-6 {
		t.Errorf("override conversion = %+v", entry)
	}

	view.overrides["glm-4.6"] = pricing.Override{Input: 999}
	if got := proxy.cfg.Prices["glm-4.6"].Input; got != 9 {
		t.Errorf("pricing view mutated config override: input=%v", got)
	}
}

func TestPricingSnapshotDisabled(t *testing.T) {
	proxy := &Proxy{cfg: &Config{Pricing: PricingConfig{Enabled: false}}}
	if got := proxy.pricingSnapshot(); got != nil {
		t.Errorf("disabled pricing snapshot = %+v, want nil", got)
	}
}

func TestPricingSnapshotInitialFailureReturnsEmpty(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	proxy := &Proxy{cfg: &Config{Pricing: PricingConfig{
		Enabled:   true,
		TTL:       "24h",
		SourceURL: "://invalid-pricing-url",
	}}}
	got := proxy.pricingSnapshot()
	if got == nil || len(got.ByModel) != 0 {
		t.Errorf("failed initial refresh = %+v, want non-nil empty catalog", got)
	}
}

func TestPricingSnapshotSerializesConcurrentRefresh(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var requests atomic.Int32
	firstRequest := make(chan struct{})
	releaseFirst := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if requests.Add(1) == 1 {
			close(firstRequest)
			<-releaseFirst
		}
		response.Header().Set("Content-Type", "application/json")
		response.Header().Set("ETag", `"pricing-v1"`)
		response.WriteHeader(http.StatusOK)
		_, _ = response.Write([]byte(pricingIntegrationFixture))
	}))
	defer server.Close()
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(releaseFirst) })

	proxy := &Proxy{cfg: &Config{Pricing: PricingConfig{
		Enabled:   true,
		TTL:       "24h",
		SourceURL: server.URL,
	}}}

	firstResult := make(chan *pricing.Catalog, 1)
	go func() {
		firstResult <- proxy.pricingSnapshot()
	}()
	<-firstRequest
	if proxy.pricingMu.TryLock() {
		proxy.pricingMu.Unlock()
		t.Fatal("pricingMu was not held while the first catalog refresh was in flight")
	}

	const callers = 11
	start := make(chan struct{})
	results := make(chan *pricing.Catalog, callers)
	var wait sync.WaitGroup
	wait.Add(callers)
	for range callers {
		go func() {
			defer wait.Done()
			<-start
			results <- proxy.pricingSnapshot()
		}()
	}
	close(start)
	releaseOnce.Do(func() { close(releaseFirst) })
	wait.Wait()
	close(results)

	first := <-firstResult
	entry, ok := first.Lookup("glm-4.6")
	if !ok || entry.Prompt != 0.9e-6 || first.Etag != `"pricing-v1"` {
		t.Errorf("first snapshot = %+v, entry=%+v, ok=%v", first, entry, ok)
	}
	for catalog := range results {
		entry, ok = catalog.Lookup("glm-4.6")
		if !ok || entry.Prompt != 0.9e-6 || catalog.Etag != `"pricing-v1"` {
			t.Errorf("concurrent snapshot = %+v, entry=%+v, ok=%v", catalog, entry, ok)
		}
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("upstream refresh requests = %d, want 1", got)
	}
}
