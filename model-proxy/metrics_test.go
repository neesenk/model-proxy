package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMetricsCounters(t *testing.T) {
	m := newMetricsStore()
	m.inc("zhipu", "requests")
	m.inc("zhipu", "requests")
	m.inc("zhipu", "failures")
	m.inc("deepseek", "rate_limited_429")
	m.inc("zhipu", "failovers")

	snap := m.snapshot()
	if snap["zhipu"].Requests != 2 {
		t.Errorf("zhipu requests = %d, want 2", snap["zhipu"].Requests)
	}
	if snap["zhipu"].Failures != 1 {
		t.Errorf("zhipu failures = %d, want 1", snap["zhipu"].Failures)
	}
	if snap["zhipu"].Failovers != 1 {
		t.Errorf("zhipu failovers = %d, want 1", snap["zhipu"].Failovers)
	}
	if snap["deepseek"].RateLimited429 != 1 {
		t.Errorf("deepseek 429 = %d, want 1", snap["deepseek"].RateLimited429)
	}
	if snap["missing"].Requests != 0 { // unseen provider → zero value
		t.Errorf("missing provider should be zero-valued")
	}
}

func TestMetricsStartedAt(t *testing.T) {
	before := time.Now()
	m := newMetricsStore()
	sa := m.startedAt()
	if sa.Before(before) || sa.After(time.Now().Add(time.Second)) {
		t.Errorf("startedAt %v not ~now", sa)
	}
}

// TestMetricsForwardWiring verifies the forward hot path bumps the right
// counters. Each scenario uses a fresh Proxy so prior cases don't leave
// scheduling state (a 429 marks the provider rate-limited for ~60s, which
// would make a later 500 case skip the provider and never bump Failures).
func TestMetricsForwardWiring(t *testing.T) {
	const cfgYAML = `
listen: 127.0.0.1:0
providers:
  zhipu:
    provider_id: zhipu
    openai_base_url: %s
routes:
  m: [{provider: zhipu, model: m}]
`
	makeProxy := func(upURL string) *Proxy {
		cfg, err := LoadConfigFromBytes("test", []byte(strings.Replace(cfgYAML, "%s", upURL, 1)))
		if err != nil {
			t.Fatalf("LoadConfigFromBytes: %v", err)
		}
		return NewProxy(cfg)
	}
	doRequest := func(p *Proxy) {
		body := []byte(`{"model":"m","stream":false}`)
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(string(body)))
		rec := httptest.NewRecorder()
		p.handler(rec, r)
		io.Copy(io.Discard, rec.Result().Body)
		rec.Result().Body.Close()
	}

	t.Run("2xx bumps Requests", func(t *testing.T) {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
			w.Write([]byte(`{"ok":true}`))
		}))
		defer up.Close()
		p := makeProxy(up.URL)
		doRequest(p)
		got := p.metrics.snapshot()["zhipu"].Requests
		if got != 1 {
			t.Fatalf("after 2xx, requests=%d want 1", got)
		}
	})

	t.Run("429 bumps RateLimited429", func(t *testing.T) {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(429)
			w.Write([]byte(`{"error":"rate"}`))
		}))
		defer up.Close()
		p := makeProxy(up.URL)
		doRequest(p)
		snap := p.metrics.snapshot()["zhipu"]
		if snap.RateLimited429 != 1 {
			t.Fatalf("after 429, rate_limited_429=%d want 1", snap.RateLimited429)
		}
		if snap.Requests != 1 {
			t.Errorf("after 429, requests=%d want 1", snap.Requests)
		}
		if snap.Failovers != 1 {
			t.Errorf("after 429, failovers=%d want 1 (429 abandons target)", snap.Failovers)
		}
	})

	t.Run("500 bumps Failures and Failovers", func(t *testing.T) {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(500)
			w.Write([]byte(`{"error":"boom"}`))
		}))
		defer up.Close()
		p := makeProxy(up.URL)
		doRequest(p)
		snap := p.metrics.snapshot()["zhipu"]
		if snap.Failures != 1 {
			t.Fatalf("after 500, failures=%d want 1", snap.Failures)
		}
		if snap.Failovers != 1 {
			t.Errorf("after 500, failovers=%d want 1 (5xx abandons target)", snap.Failovers)
		}
		if snap.Requests != 1 {
			t.Errorf("after 500, requests=%d want 1", snap.Requests)
		}
	})

	t.Run("connection error bumps Failures and Failovers", func(t *testing.T) {
		// A server that immediately closes the connection without responding.
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatalf("server doesn't support hijack")
			}
			c, _, _ := hj.Hijack()
			c.Close()
		}))
		defer up.Close()
		p := makeProxy(up.URL)
		doRequest(p)
		snap := p.metrics.snapshot()["zhipu"]
		if snap.Failures != 1 {
			t.Fatalf("after conn error, failures=%d want 1", snap.Failures)
		}
		if snap.Failovers != 1 {
			t.Errorf("after conn error, failovers=%d want 1", snap.Failovers)
		}
	})
}
