package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// newHitServer returns a mock upstream whose handler decides the response per
// hit count (1-based) and increments an atomic counter on each hit.
func newHitServer(h func(hit int) (status int, body string, headers http.Header, delay time.Duration)) (*httptest.Server, *atomic.Int32) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		c := int(n.Add(1))
		status, body, headers, delay := h(c)
		if delay > 0 {
			time.Sleep(delay)
		}
		for k, vs := range headers {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.Header().Set("content-type", "application/json")
		if status != 200 {
			w.WriteHeader(status)
		}
		w.Write([]byte(body))
	}))
	return srv, &n
}

func intHdr(k, v string) http.Header {
	h := http.Header{}
	h.Set(k, v)
	return h
}

// schedCfg returns a Scheduling with short durations for fast tests.
func schedCfg(threshold int, cooldown, rateBackoff, timeout, dwell string) Scheduling {
	return Scheduling{
		CircuitThreshold: threshold,
		CircuitCooldown:  cooldown,
		RateLimitBackoff: rateBackoff,
		UpstreamTimeout:  timeout,
		StickyDwell:      dwell,
	}
}

// TestCircuit_OpensAfter3Failures: 3 consecutive 5xx open the circuit; the 4th
// request skips the (now-unavailable) primary and goes straight to the fallback.
func TestCircuit_OpensAfter3Failures(t *testing.T) {
	primary, pHits := newHitServer(func(int) (int, string, http.Header, time.Duration) {
		return 500, `{"e":"primary"}`, nil, 0
	})
	defer primary.Close()
	fallback, fHits := newHitServer(func(int) (int, string, http.Header, time.Duration) {
		return 200, `{"ok":true}`, nil, 0
	})
	defer fallback.Close()
	cfg := &Config{
		Providers: map[string]Provider{
			"primary":  {OpenAIBaseURL: primary.URL, Provider: testProviderID},
			"fallback": {OpenAIBaseURL: fallback.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{
			"m1": {
				{Provider: "primary", Model: "m1", Priority: 1},
				{Provider: "fallback", Model: "m1", Priority: 2},
			},
		},
		Scheduling: schedCfg(3, "50ms", "10s", "5s", "0s"),
	}
	p := newTestProxy(t, cfg)
	p.providers["primary"] = &testProv{key: "p"}
	p.providers["fallback"] = &testProv{key: "f"}
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	for i := 0; i < 4; i++ {
		postOK(t, px.URL+"/v1/chat/completions", `{"model":"m1","messages":[]}`)
	}
	if got := pHits.Load(); got != 3 {
		t.Errorf("primary hits after circuit open: got %d, want 3 (4th request should skip primary)", got)
	}
	if got := fHits.Load(); got != 4 {
		t.Errorf("fallback hits: got %d, want 4", got)
	}
}

// TestCircuit_HalfOpenClosesOnSuccess: after the circuit opens + cooldown
// expires, one probe is allowed; success closes the circuit (subsequent
// requests use the primary again).
func TestCircuit_HalfOpenClosesOnSuccess(t *testing.T) {
	primary, pHits := newHitServer(func(c int) (int, string, http.Header, time.Duration) {
		if c <= 3 {
			return 500, `{"e":"primary"}`, nil, 0
		}
		return 200, `{"ok":true}`, nil, 0 // recovered
	})
	defer primary.Close()
	fallback, fHits := newHitServer(func(int) (int, string, http.Header, time.Duration) {
		return 200, `{"ok":true}`, nil, 0
	})
	defer fallback.Close()
	cfg := &Config{
		Providers: map[string]Provider{
			"primary":  {OpenAIBaseURL: primary.URL, Provider: testProviderID},
			"fallback": {OpenAIBaseURL: fallback.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{
			"m1": {
				{Provider: "primary", Model: "m1", Priority: 1},
				{Provider: "fallback", Model: "m1", Priority: 2},
			},
		},
		Scheduling: schedCfg(3, "50ms", "10s", "5s", "0s"),
	}
	p := newTestProxy(t, cfg)
	p.providers["primary"] = &testProv{key: "p"}
	p.providers["fallback"] = &testProv{key: "f"}
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	for i := 0; i < 3; i++ { // trip the circuit (3 failures)
		postOK(t, px.URL+"/v1/chat/completions", `{"model":"m1","messages":[]}`)
	}
	time.Sleep(80 * time.Millisecond)                                        // cooldown expires → half-open
	postOK(t, px.URL+"/v1/chat/completions", `{"model":"m1","messages":[]}`) // probe: primary recovered → close
	postOK(t, px.URL+"/v1/chat/completions", `{"model":"m1","messages":[]}`) // primary available again

	if got := pHits.Load(); got != 5 {
		t.Errorf("primary hits: got %d, want 5 (3 failures + 2 successes after half-open close)", got)
	}
	if got := fHits.Load(); got != 3 {
		t.Errorf("fallback hits: got %d, want 3 (only during the 3 failures)", got)
	}
}

// TestRateLimit_SkipsProvider: a 429 (with Retry-After) marks the provider
// rate-limited; the next request skips it and uses the fallback.
func TestRateLimit_SkipsProvider(t *testing.T) {
	primary, pHits := newHitServer(func(int) (int, string, http.Header, time.Duration) {
		return 429, `{"e":"rate"}`, intHdr("Retry-After", "30"), 0
	})
	defer primary.Close()
	fallback, fHits := newHitServer(func(int) (int, string, http.Header, time.Duration) {
		return 200, `{"ok":true}`, nil, 0
	})
	defer fallback.Close()
	cfg := &Config{
		Providers: map[string]Provider{
			"primary":  {OpenAIBaseURL: primary.URL, Provider: testProviderID},
			"fallback": {OpenAIBaseURL: fallback.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{
			"m1": {
				{Provider: "primary", Model: "m1", Priority: 1},
				{Provider: "fallback", Model: "m1", Priority: 2},
			},
		},
		Scheduling: schedCfg(3, "50ms", "10s", "5s", "0s"),
	}
	p := newTestProxy(t, cfg)
	p.providers["primary"] = &testProv{key: "p"}
	p.providers["fallback"] = &testProv{key: "f"}
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	postOK(t, px.URL+"/v1/chat/completions", `{"model":"m1","messages":[]}`) // 429 → rate-limit primary
	postOK(t, px.URL+"/v1/chat/completions", `{"model":"m1","messages":[]}`) // primary skipped

	if got := pHits.Load(); got != 1 {
		t.Errorf("primary hits: got %d, want 1 (rate-limited after the 429)", got)
	}
	if got := fHits.Load(); got != 2 {
		t.Errorf("fallback hits: got %d, want 2", got)
	}
}

// TestStickyDwell_HoldsThenReEvaluates: after failing over to the fallback, the
// route stays on it for the sticky_dwell window even once the primary recovers;
// after the dwell, it re-evaluates and returns to the (priority-1) primary.
func TestStickyDwell_HoldsThenReEvaluates(t *testing.T) {
	primary, pHits := newHitServer(func(c int) (int, string, http.Header, time.Duration) {
		if c <= 3 {
			return 500, `{"e":"primary"}`, nil, 0
		}
		return 200, `{"ok":true}`, nil, 0
	})
	defer primary.Close()
	fallback, fHits := newHitServer(func(int) (int, string, http.Header, time.Duration) {
		return 200, `{"ok":true}`, nil, 0
	})
	defer fallback.Close()
	cfg := &Config{
		Providers: map[string]Provider{
			"primary":  {OpenAIBaseURL: primary.URL, Provider: testProviderID},
			"fallback": {OpenAIBaseURL: fallback.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{
			"m1": {
				{Provider: "primary", Model: "m1", Priority: 1},
				{Provider: "fallback", Model: "m1", Priority: 2},
			},
		},
		Scheduling: schedCfg(3, "50ms", "10s", "5s", "150ms"),
	}
	p := newTestProxy(t, cfg)
	p.providers["primary"] = &testProv{key: "p"}
	p.providers["fallback"] = &testProv{key: "f"}
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	for i := 0; i < 3; i++ { // trip primary circuit (3 failures)
		post(t, px.URL+"/v1/chat/completions", `{"model":"m1","messages":[]}`)
	}
	post(t, px.URL+"/v1/chat/completions", `{"model":"m1","messages":[]}`) // primary open → fallback, sticky=fallback
	fbAfterFailover := fHits.Load()

	time.Sleep(80 * time.Millisecond) // primary cooldown expired (half-open) but fallback dwell (150ms) still active
	post(t, px.URL+"/v1/chat/completions", `{"model":"m1","messages":[]}`)
	if got := fHits.Load(); got != fbAfterFailover+1 {
		t.Errorf("within dwell: fallback should still serve (sticky), got fHits %d→%d", fbAfterFailover, got)
	}
	if got := pHits.Load(); got != 3 {
		t.Errorf("within dwell: primary should not be retried yet, got pHits %d (want 3)", got)
	}

	time.Sleep(100 * time.Millisecond) // dwell (150ms) now expired → re-evaluate
	post(t, px.URL+"/v1/chat/completions", `{"model":"m1","messages":[]}`)
	if got := pHits.Load(); got != 4 {
		t.Errorf("after dwell: primary should be re-evaluated (half-open probe), got pHits %d (want 4)", got)
	}
}

// TestUpstreamTimeout_Failover: a hanging upstream exceeds upstream_timeout and
// triggers failover to the next target.
func TestUpstreamTimeout_Failover(t *testing.T) {
	primary, pHits := newHitServer(func(int) (int, string, http.Header, time.Duration) {
		return 200, `{"ok":true}`, nil, 200 * time.Millisecond // hangs past the timeout
	})
	defer primary.Close()
	fallback, fHits := newHitServer(func(int) (int, string, http.Header, time.Duration) {
		return 200, `{"ok":true}`, nil, 0
	})
	defer fallback.Close()
	cfg := &Config{
		Providers: map[string]Provider{
			"primary":  {OpenAIBaseURL: primary.URL, Provider: testProviderID},
			"fallback": {OpenAIBaseURL: fallback.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{
			"m1": {
				{Provider: "primary", Model: "m1", Priority: 1},
				{Provider: "fallback", Model: "m1", Priority: 2},
			},
		},
		Scheduling: schedCfg(3, "50ms", "10s", "50ms", "0s"),
	}
	p := newTestProxy(t, cfg)
	p.providers["primary"] = &testProv{key: "p"}
	p.providers["fallback"] = &testProv{key: "f"}
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	start := time.Now()
	resp, err := http.Post(px.URL+"/v1/chat/completions", "application/json", stringReader(`{"model":"m1","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	elapsed := time.Since(start)

	if resp.StatusCode != 200 {
		t.Errorf("timeout failover: status=%d body=%s, want 200 from fallback", resp.StatusCode, body)
	}
	if elapsed > 1500*time.Millisecond {
		t.Errorf("timeout failover was slow: %v (upstream_timeout=50ms should failover fast)", elapsed)
	}
	if got := pHits.Load(); got != 1 {
		t.Errorf("primary should be tried once (then timed out), got %d", got)
	}
	if got := fHits.Load(); got != 1 {
		t.Errorf("fallback should serve after timeout, got %d", got)
	}
}

// TestHalfOpen_4xxReleasesSlot (P0-4): a provider whose half-open probe gets a
// 4xx (client error, committed) must NOT stay stuck — the probe slot is
// released (failure history kept) and the provider is available for the next
// request. Before the fix, halfOpenInFlight stayed true forever (starvation).
func TestHalfOpen_4xxReleasesSlot(t *testing.T) {
	primary, pHits := newHitServer(func(c int) (int, string, http.Header, time.Duration) {
		if c <= 3 {
			return 500, `{"e":"broken"}`, nil, 0 // trip the circuit
		}
		return 400, `{"e":"bad request"}`, nil, 0 // half-open probe: client error
	})
	defer primary.Close()
	fallback, _ := newHitServer(func(int) (int, string, http.Header, time.Duration) {
		return 200, `{"ok":true}`, nil, 0
	})
	defer fallback.Close()
	cfg := &Config{
		Providers: map[string]Provider{
			"primary":  {OpenAIBaseURL: primary.URL, Provider: testProviderID},
			"fallback": {OpenAIBaseURL: fallback.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{
			"m1": {
				{Provider: "primary", Model: "m1", Priority: 1},
				{Provider: "fallback", Model: "m1", Priority: 2},
			},
		},
		Scheduling: schedCfg(3, "50ms", "10s", "5s", "0s"),
	}
	p := newTestProxy(t, cfg)
	p.providers["primary"] = &testProv{key: "p"}
	p.providers["fallback"] = &testProv{key: "f"}
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	for i := 0; i < 3; i++ { // trip the circuit (3× 500)
		post(t, px.URL+"/v1/chat/completions", `{"model":"m1","messages":[]}`)
	}
	time.Sleep(80 * time.Millisecond) // cooldown expires → half-open
	st := postStatus(t, px.URL+"/v1/chat/completions", `{"model":"m1","messages":[]}`)
	if st != 400 {
		t.Fatalf("half-open probe: status = %d, want 400 (committed client error)", st)
	}
	h, ok := p.runtimeState.Dashboard(time.Now()).Providers["primary"]
	if !ok {
		t.Fatal("primary health missing")
	}
	if h.HalfOpenInFlight {
		t.Error("halfOpenInFlight stuck after 4xx commit — provider would starve")
	}
	if h.ConsecutiveFailures == 0 {
		t.Error("4xx commit must NOT clear failure history (release-neutral)")
	}
	if !h.Available {
		t.Error("primary must be available again after the 4xx probe released the slot")
	}
	// Next request reaches the primary again (single-flight freed).
	before := pHits.Load()
	post(t, px.URL+"/v1/chat/completions", `{"model":"m1","messages":[]}`)
	if pHits.Load() != before+1 {
		t.Errorf("primary not retried after slot release: hits %d → %d", before, pHits.Load())
	}
}
