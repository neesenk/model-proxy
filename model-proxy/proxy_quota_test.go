package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"model-proxy/provider"
)

// TestProxy_QuotaRefreshOnRateLimit: a 429 on a provider triggers an async
// quota refresh of that provider.
func TestProxy_QuotaRefreshOnRateLimit(t *testing.T) {
	primary, _ := newHitServer(func(int) (int, string, http.Header, time.Duration) {
		return 429, `{}`, intHdr("Retry-After", "30"), 0
	})
	defer primary.Close()
	fallback, _ := newHitServer(func(int) (int, string, http.Header, time.Duration) {
		return 200, `{"ok":true}`, nil, 0
	})
	defer fallback.Close()
	cfg := &Config{
		Providers: map[string]Provider{
			"primary":  {OpenAIBaseURL: primary.URL, Provider: "static"},
			"fallback": {OpenAIBaseURL: fallback.URL, Provider: "static"},
		},
		Routes: map[string][]RouteTarget{"m1": {
			{Provider: "primary", Model: "m1", Priority: 1},
			{Provider: "fallback", Model: "m1", Priority: 2},
		}},
		Scheduling: schedCfg(3, "50ms", "10s", "5s", "0s"),
	}
	p := NewProxy(cfg)
	p.providers["primary"] = &quotaCountProv{}
	p.providers["fallback"] = &testProv{key: "f"}
	var refreshes atomic.Int32
	p.quota = &quotaTracker{
		state: map[string]*provider.QuotaSnapshot{},
		cfg:   func() *Config { return cfg },
		provs: func() map[string]provider.Provider { return p.providers },
	}
	// refreshHook lets the test count refreshes without running a real poll.
	p.quota.refreshHook = func(name string) { refreshes.Add(1) }
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()
	post(t, px.URL+"/v1/chat/completions", `{"model":"m1","messages":[]}`)
	// The refresh is launched as a goroutine from recordRateLimit; give it a
	// brief moment to land before asserting (the failover HTTP round-trip to
	// the fallback normally dominates, but poll briefly to stay deterministic).
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) && refreshes.Load() == 0 {
		time.Sleep(2 * time.Millisecond)
	}
	if got := refreshes.Load(); got != 1 {
		t.Errorf("expected 1 quota refresh after 429, got %d", got)
	}
}

// quotaCountProv is a testProv whose Quota() is callable.
type quotaCountProv struct{ testProv }

func (q *quotaCountProv) Quota() (*provider.QuotaSnapshot, error) {
	return &provider.QuotaSnapshot{Billing: provider.BillingPlan, RemainingPct: 0.5}, nil
}
