package main

import (
	"io"
	"model-proxy/internal/observe/counters"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMetricsCounters(t *testing.T) {
	m := counters.NewMetricsStore()
	m.Inc("zhipu", "m", counters.EvRequests)
	m.Inc("zhipu", "m", counters.EvRequests)
	m.Inc("zhipu", "m", counters.EvFailures)
	m.Inc("deepseek", "d", counters.EvRateLimited429)
	m.Inc("zhipu", "m", counters.EvFailovers)

	snap := m.Snapshot()
	zk := counters.PMKey{Provider: "zhipu", Model: "m"}
	if snap[zk].Requests != 2 {
		t.Errorf("zhipu/m requests = %d, want 2", snap[zk].Requests)
	}
	if snap[zk].Failures != 1 {
		t.Errorf("zhipu/m failures = %d, want 1", snap[zk].Failures)
	}
	if snap[zk].Failovers != 1 {
		t.Errorf("zhipu/m failovers = %d, want 1", snap[zk].Failovers)
	}
	dk := counters.PMKey{Provider: "deepseek", Model: "d"}
	if snap[dk].RateLimited429 != 1 {
		t.Errorf("deepseek/d 429 = %d, want 1", snap[dk].RateLimited429)
	}
	if snap[counters.PMKey{Provider: "missing", Model: "x"}].Requests != 0 { // unseen -> zero value
		t.Errorf("missing key should be zero-valued")
	}

	// aggregateByProvider collapses the model dimension: zhipu has both metrics
	// under model "m", deepseek under "d".
	agg := m.AggregateByProvider()
	if agg["zhipu"].Requests != 2 || agg["zhipu"].Failures != 1 || agg["zhipu"].Failovers != 1 {
		t.Errorf("aggregate zhipu = %+v, want reqs=2 fail=1 failover=1", agg["zhipu"])
	}
	if agg["deepseek"].RateLimited429 != 1 {
		t.Errorf("aggregate deepseek 429 = %d, want 1", agg["deepseek"].RateLimited429)
	}
}

func TestMetricsStartedAt(t *testing.T) {
	before := time.Now()
	m := counters.NewMetricsStore()
	sa := m.StartedAt()
	if sa.Before(before) || sa.After(time.Now().Add(time.Second)) {
		t.Errorf("startedAt %v not ~now", sa)
	}
}

// TestMetricsForwardWiring verifies the forward hot path bumps the right
// counters, attributed to (provider, model). Each scenario uses a fresh Proxy so
// prior cases don't leave scheduling state (a 429 marks the provider
// rate-limited for ~60s, which would make a later 500 case skip the provider and
// never bump Failures).
func TestMetricsForwardWiring(t *testing.T) {
	// Isolate HOME and provision a zhipu pool account so buildProviders binds a
	// real credential — without it the proxy can't build auth headers and the
	// forward never reaches the metrics-recording commit path. (The fake upstream
	// ignores auth, but the proxy must still be able to CONSTRUCT it.) This used
	// to pass only because the real ~/.model-proxy had zhipu creds on the dev box.
	setPoolHome(t, t.TempDir())
	writePoolFile(t, "zhipu", "zhipu", "KEY-A")
	const cfgYAML = `
listen: 127.0.0.1:0
providers:
  zhipu:
    provider_id: zhipu
    openai_base_url: %s
routes:
  m: [{provider: zhipu, model: glm-5}]
`
	makeProxy := func(upURL string) *Proxy {
		cfg, err := LoadConfigFromBytes("test", []byte(strings.Replace(cfgYAML, "%s", upURL, 1)))
		if err != nil {
			t.Fatalf("LoadConfigFromBytes: %v", err)
		}
		return newTestProxy(t, cfg)
	}
	doRequest := func(p *Proxy) {
		body := []byte(`{"model":"m","stream":false}`)
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(string(body)))
		rec := httptest.NewRecorder()
		p.handler(rec, r)
		io.Copy(io.Discard, rec.Result().Body)
		rec.Result().Body.Close()
	}
	zk := counters.PMKey{Provider: "zhipu", Model: "glm-5"}

	t.Run("2xx bumps Requests", func(t *testing.T) {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
			w.Write([]byte(`{"ok":true}`))
		}))
		defer up.Close()
		p := makeProxy(up.URL)
		doRequest(p)
		got := p.metrics.Snapshot()[zk].Requests
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
		snap := p.metrics.Snapshot()[zk]
		if snap.RateLimited429 != 1 {
			t.Fatalf("after 429, rate_limited_429=%d want 1", snap.RateLimited429)
		}
		if snap.Requests != 0 {
			t.Errorf("after 429, requests=%d want 0 (commit-only: failed target never served)", snap.Requests)
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
		snap := p.metrics.Snapshot()[zk]
		if snap.Failures != 1 {
			t.Fatalf("after 500, failures=%d want 1", snap.Failures)
		}
		if snap.Failovers != 1 {
			t.Errorf("after 500, failovers=%d want 1 (5xx abandons target)", snap.Failovers)
		}
		if snap.Requests != 0 {
			t.Errorf("after 500, requests=%d want 0 (commit-only: failed target never served)", snap.Requests)
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
		snap := p.metrics.Snapshot()[zk]
		if snap.Failures != 1 {
			t.Fatalf("after conn error, failures=%d want 1", snap.Failures)
		}
		if snap.Failovers != 1 {
			t.Errorf("after conn error, failovers=%d want 1", snap.Failovers)
		}
	})
}
