package app

import (
	"encoding/json"
	"io"
	configdomain "model-proxy/internal/config"
	runtimestate "model-proxy/internal/runtime"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ---- health_test.go ----

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
func schedCfg(threshold int, cooldown, rateBackoff, timeout, dwell string) configdomain.Scheduling {
	return configdomain.Scheduling{
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
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"primary":  {OpenAIBaseURL: primary.URL, Provider: testProviderID},
			"fallback": {OpenAIBaseURL: fallback.URL, Provider: testProviderID},
		},
		Routes: map[string][]configdomain.RouteTarget{
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
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"primary":  {OpenAIBaseURL: primary.URL, Provider: testProviderID},
			"fallback": {OpenAIBaseURL: fallback.URL, Provider: testProviderID},
		},
		Routes: map[string][]configdomain.RouteTarget{
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
	// Cooldown expiry is observable (provider available again) — poll instead
	// of sleeping a fixed 80ms.
	waitUntil(t, "primary circuit cooldown → half-open", func() bool {
		h, ok := p.runtimeState.Dashboard(time.Now()).Providers["primary"]
		return ok && h.Available
	})
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
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"primary":  {OpenAIBaseURL: primary.URL, Provider: testProviderID},
			"fallback": {OpenAIBaseURL: fallback.URL, Provider: testProviderID},
		},
		Routes: map[string][]configdomain.RouteTarget{
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
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"primary":  {OpenAIBaseURL: primary.URL, Provider: testProviderID},
			"fallback": {OpenAIBaseURL: fallback.URL, Provider: testProviderID},
		},
		Routes: map[string][]configdomain.RouteTarget{
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
	postOK(t, px.URL+"/v1/chat/completions", `{"model":"m1","messages":[]}`) // primary open → fallback, sticky=fallback
	fbAfterFailover := fHits.Load()
	stickyFrom := time.Now()

	// Wait for the cooldown to lapse (observable), not a fixed sleep: this
	// ends at the earliest moment primary is probeable, leaving the whole
	// dwell window as margin.
	waitUntil(t, "primary circuit cooldown → half-open", func() bool {
		h, ok := p.runtimeState.Dashboard(time.Now()).Providers["primary"]
		return ok && h.Available
	})
	postOK(t, px.URL+"/v1/chat/completions", `{"model":"m1","messages":[]}`)
	if got := fHits.Load(); got != fbAfterFailover+1 {
		t.Errorf("within dwell: fallback should still serve (sticky), got fHits %d→%d", fbAfterFailover, got)
	}
	if got := pHits.Load(); got != 3 {
		t.Errorf("within dwell: primary should not be retried yet, got pHits %d (want 3)", got)
	}

	// Dwell (150ms, set at the failover post) now expired → re-evaluate. Sleep
	// to the KNOWN deadline captured at the dwell anchor, not a fixed guess.
	if rest := time.Until(stickyFrom.Add(200 * time.Millisecond)); rest > 0 {
		time.Sleep(rest)
	}
	postOK(t, px.URL+"/v1/chat/completions", `{"model":"m1","messages":[]}`)
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
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"primary":  {OpenAIBaseURL: primary.URL, Provider: testProviderID},
			"fallback": {OpenAIBaseURL: fallback.URL, Provider: testProviderID},
		},
		Routes: map[string][]configdomain.RouteTarget{
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
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"primary":  {OpenAIBaseURL: primary.URL, Provider: testProviderID},
			"fallback": {OpenAIBaseURL: fallback.URL, Provider: testProviderID},
		},
		Routes: map[string][]configdomain.RouteTarget{
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
	// Cooldown expiry is observable (provider available again) — poll instead
	// of sleeping a fixed 80ms.
	waitUntil(t, "primary circuit cooldown → half-open", func() bool {
		h, ok := p.runtimeState.Dashboard(time.Now()).Providers["primary"]
		return ok && h.Available
	})
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
	// Next request reaches the primary again (single-flight freed). The
	// upstream always answers 400 here, so the client sees the committed 400.
	before := pHits.Load()
	if st := postStatus(t, px.URL+"/v1/chat/completions", `{"model":"m1","messages":[]}`); st != 400 {
		t.Fatalf("post-release request: status = %d, want 400 (committed upstream client error)", st)
	}
	if pHits.Load() != before+1 {
		t.Errorf("primary not retried after slot release: hits %d → %d", before, pHits.Load())
	}
}

// TestHalfOpen_FailedProbeReopensCircuit (P0): the missing half-open terminal.
// A probe that gets a 5xx must re-open the circuit with a fresh cooldown AND
// release the single-flight slot — otherwise later requests would either keep
// hitting the dead primary or starve on a stuck slot. The unit-level manager
// test covers the state transition; this is the HTTP-level end state.
func TestHalfOpen_FailedProbeReopensCircuit(t *testing.T) {
	primary, pHits := newHitServer(func(c int) (int, string, http.Header, time.Duration) {
		return 500, `{"e":"broken"}`, nil, 0 // trips the circuit AND fails the probe
	})
	defer primary.Close()
	fallback, _ := newHitServer(func(int) (int, string, http.Header, time.Duration) {
		return 200, `{"ok":true}`, nil, 0
	})
	defer fallback.Close()
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"primary":  {OpenAIBaseURL: primary.URL, Provider: testProviderID},
			"fallback": {OpenAIBaseURL: fallback.URL, Provider: testProviderID},
		},
		Routes: map[string][]configdomain.RouteTarget{
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

	// Trip the circuit (3× 500 → open), then wait out the cooldown.
	for i := 0; i < 3; i++ {
		postStatus(t, px.URL+"/v1/chat/completions", `{"model":"m1","messages":[]}`)
	}
	waitUntil(t, "primary circuit cooldown → half-open", func() bool {
		h, ok := p.runtimeState.Dashboard(time.Now()).Providers["primary"]
		return ok && h.Available
	})

	// The half-open probe goes to the primary, gets 500, and the request
	// still succeeds via the fallback (client-visible outcome is a 200).
	probeHits := pHits.Load()
	if st := postStatus(t, px.URL+"/v1/chat/completions", `{"model":"m1","messages":[]}`); st != 200 {
		t.Fatalf("probe request: status = %d, want 200 (fallback serves)", st)
	}
	if got := pHits.Load(); got != probeHits+1 {
		t.Fatalf("probe hits: primary %d → %d, want exactly one probe", probeHits, got)
	}

	// The failed probe must re-open the circuit with a fresh cooldown and
	// release the single-flight slot.
	h, ok := p.runtimeState.Dashboard(time.Now()).Providers["primary"]
	if !ok {
		t.Fatal("primary health missing")
	}
	if h.Available || !h.CircuitOpenUntil.After(time.Now()) {
		t.Errorf("failed probe did not re-open the circuit: %+v", h)
	}
	if h.HalfOpenInFlight {
		t.Error("half-open slot stuck after a failed probe")
	}

	// While re-opened, the next request must skip the primary entirely.
	before := pHits.Load()
	if st := postStatus(t, px.URL+"/v1/chat/completions", `{"model":"m1","messages":[]}`); st != 200 {
		t.Fatalf("post-reopen request: status = %d, want 200", st)
	}
	if pHits.Load() != before {
		t.Errorf("primary hit after circuit re-opened: %d → %d", before, pHits.Load())
	}
}

// ---- health_persist_test.go ----

// TestHealthPersist_RoundTrip: frozen health state (rate-limit + kind, circuit,
// model locks, param blocklist) persists to quota_state.json and is restored
// by a fresh Proxy; expired cooldowns are dropped.
func TestHealthPersist_RoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := &configdomain.Config{Providers: map[string]configdomain.Provider{"a": {OpenAIBaseURL: "http://x", Provider: testProviderID}}}
	statePath := filepath.Join(t.TempDir(), "quota_state.json")

	p1 := newTestProxyAt(t, cfg, statePath)
	now := time.Now()
	rateLimitUntil := now.Add(2 * time.Hour)
	p1.recordRateLimit("a", rateLimitUntil, rlQuota)
	modelLockBefore := time.Now().Add(time.Hour)
	p1.recordModelFailure("a", "m1", configdomain.Scheduling{ModelLockout: "1h"})
	modelLockAfter := time.Now().Add(time.Hour)
	p1.learnParamBlock("a", "m1", "max_tokens")
	// Circuit on a second provider + an already-expired rate limit (must drop).
	circuitUntil := now.Add(10 * time.Minute)
	p1.runtimeState.RestoreHealth(map[string]runtimestate.PersistedHealth{
		"b": {CircuitOpenUntil: circuitUntil},
		"c": {RateLimitedUntil: now.Add(-time.Minute)},
	}, now, cfg.Scheduling.Threshold())
	if err := p1.quota.Persist(); err != nil {
		t.Fatalf("persist source state: %v", err)
	}
	p1.Close()

	p2 := newTestProxyAt(t, cfg, statePath)
	runtimeSnapshot := p2.runtimeState.Dashboard(now)
	h, ok := runtimeSnapshot.Providers["a"]
	if !ok {
		t.Fatalf("provider a rate-limit not restored: %+v", runtimeSnapshot.Providers)
	}
	if !h.RateLimitedUntil.Equal(rateLimitUntil) {
		t.Errorf("rateLimitedUntil = %s, want %s", h.RateLimitedUntil, rateLimitUntil)
	}
	if h.RateLimitKind != rlQuota {
		t.Errorf("rateLimitKind = %v want rlQuota", h.RateLimitKind)
	}
	locks := runtimeSnapshot.ModelLocks["a"]
	if len(locks) != 1 || locks[0].Model != "m1" {
		t.Errorf("model lock (a,m1) not restored: %+v", locks)
	} else if locks[0].LockedUntil.Before(modelLockBefore) || locks[0].LockedUntil.After(modelLockAfter) {
		t.Errorf("model lock until %s outside [%s, %s]", locks[0].LockedUntil, modelLockBefore, modelLockAfter)
	}
	if !p2.runtimeState.ParamBlocked("a", "m1", "max_tokens") {
		t.Error("param blocklist for a not restored")
	}

	hb, ok := runtimeSnapshot.Providers["b"]
	if !ok {
		t.Fatalf("provider b circuit not restored: %+v", runtimeSnapshot.Providers)
	}
	if !hb.CircuitOpenUntil.Equal(circuitUntil) {
		t.Errorf("circuitOpenUntil = %s, want %s", hb.CircuitOpenUntil, circuitUntil)
	}
	if hb.ConsecutiveFailures != cfg.Scheduling.Threshold() {
		t.Errorf("restored circuit failures = %d, want threshold %d (next failure re-opens)",
			hb.ConsecutiveFailures, cfg.Scheduling.Threshold())
	}

	if _, ok := runtimeSnapshot.Providers["c"]; ok {
		t.Error("expired rate-limit on c must be dropped, not restored")
	}
}

// TestHealthPersist_FingerprintMismatch: health frozen under one config must
// NOT restore onto a different config that reuses the same provider names
// (test binaries share the state file; daemons restart with edited configs).
func TestHealthPersist_FingerprintMismatch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfgA := &configdomain.Config{Providers: map[string]configdomain.Provider{"a": {OpenAIBaseURL: "http://x", Provider: testProviderID}}}
	statePath := filepath.Join(t.TempDir(), "quota_state.json")
	p1 := newTestProxyAt(t, cfgA, statePath)
	p1.recordRateLimit("a", time.Now().Add(2*time.Hour), rlQuota)
	if err := p1.quota.Persist(); err != nil {
		t.Fatalf("persist source state: %v", err)
	}
	p1.Close()

	// Same provider NAME, different upstream URL → different fingerprint.
	cfgB := &configdomain.Config{Providers: map[string]configdomain.Provider{"a": {OpenAIBaseURL: "http://y", Provider: testProviderID}}}
	p2 := newTestProxyAt(t, cfgB, statePath)
	_, frozen := p2.runtimeState.Dashboard(time.Now()).Providers["a"]
	if frozen {
		t.Error("health must not restore under a mismatched config fingerprint")
	}
}

// TestHealthPersist_ParamBlockOnlyProvider: a provider with ONLY learned params
// (never failed → no health entry) must still persist + restore its blocklist.
func TestHealthPersist_ParamBlockOnlyProvider(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := &configdomain.Config{Providers: map[string]configdomain.Provider{"a": {OpenAIBaseURL: "http://x", Provider: testProviderID}}}
	statePath := filepath.Join(t.TempDir(), "quota_state.json")
	p1 := newTestProxyAt(t, cfg, statePath)
	p1.learnParamBlock("a", "m1", "max_tokens")
	if err := p1.quota.Persist(); err != nil {
		t.Fatalf("persist source state: %v", err)
	}
	p1.Close()

	p2 := newTestProxyAt(t, cfg, statePath)
	blocked := p2.runtimeState.ParamBlocked("a", "m1", "max_tokens")
	_, hasHealth := p2.runtimeState.Dashboard(time.Now()).Providers["a"]
	if !blocked {
		t.Error("param blocklist must persist even without a health entry")
	}
	if hasHealth {
		t.Error("no health entry should be synthesized for a param-only provider")
	}
}

// ---- unfreeze_test.go ----

// TestResetHealth: the proxy method clears circuit/rate-limit state + model
// locks for the named provider (pooled parent = all its virtual accounts), or
// everything when empty — and never touches param blocklists, sticky, or pins.
func TestResetHealth(t *testing.T) {
	cfg := &configdomain.Config{Providers: map[string]configdomain.Provider{
		"a": {OpenAIBaseURL: "http://x", Provider: testProviderID},
		"b": {OpenAIBaseURL: "http://y", Provider: testProviderID},
	}}
	p := newTestProxy(t, cfg)
	p.mu.Lock()
	p.parentOf = map[string]string{"a#v1": "a", "a#v2": "a"}
	p.mu.Unlock()
	now := time.Now()
	p.recordRateLimit("a#v1", now.Add(time.Hour), rlQuota)
	p.recordRateLimit("a#v2", now.Add(time.Hour), rlTransient)
	p.recordRateLimit("b", now.Add(time.Hour), rlDaily)
	p.recordModelFailure("a#v1", "m1", configdomain.Scheduling{ModelLockout: "1h"})
	p.recordModelFailure("b", "m2", configdomain.Scheduling{ModelLockout: "1h"})
	p.learnParamBlock("a#v1", "m1", "max_tokens")
	seedRuntimeSticky(t, p, "route1", "a#v1", now)
	p.runtimeState.SetPin("route2", runtimestate.Pin{Provider: "a#v1"})

	cleared, locks := p.resetHealth("a")
	if len(cleared) != 2 || cleared[0] != "a#v1" || cleared[1] != "a#v2" {
		t.Errorf("cleared = %v, want [a#v1 a#v2] (pooled parent matches virtuals)", cleared)
	}
	if locks != 1 {
		t.Errorf("locks = %d, want 1 (only (a#v1,m1))", locks)
	}
	snapshot := p.runtimeState.Dashboard(now)
	_, bFrozen := snapshot.Providers["b"]
	bLock := p.runtimeState.ModelLocked("b", "m2", now)
	blocked := p.runtimeState.ParamBlocked("a#v1", "m1", "max_tokens")
	_, stickyOK := p.runtimeState.Sticky("route1")
	_, pinOK := p.runtimeState.Pins(now)["route2"]
	if !bFrozen || !bLock {
		t.Error("provider b state must survive resetHealth(\"a\")")
	}
	if !blocked {
		t.Error("param blocklist must NOT be cleared by resetHealth")
	}
	if !stickyOK || !pinOK {
		t.Error("sticky/pins must NOT be cleared by resetHealth")
	}

	cleared, locks = p.resetHealth("")
	if len(cleared) != 1 || cleared[0] != "b" || locks != 1 {
		t.Errorf("reset all: cleared = %v locks = %d, want [b], 1", cleared, locks)
	}
}

// TestHealthResetAPI: POST /api/health/reset — empty body resets all,
// {"provider":name} resets one; response carries cleared names + lock count.
func TestHealthResetAPI(t *testing.T) {
	w, p := newTestWeb(t)
	mux := http.NewServeMux()
	w.Register(mux)
	p.recordRateLimit("zhipu", time.Now().Add(time.Hour), rlQuota)
	p.recordModelFailure("zhipu", "glm-x", configdomain.Scheduling{ModelLockout: "1h"})

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/health/reset", strings.NewReader(`{"provider":"zhipu"}`)))
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var out struct {
		Cleared           []string `json:"cleared"`
		ModelLocksCleared int      `json:"model_locks_cleared"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Cleared) != 1 || out.Cleared[0] != "zhipu" || out.ModelLocksCleared != 1 {
		t.Errorf("response = %+v, want cleared [zhipu] + 1 lock", out)
	}
	if _, frozen := p.runtimeState.Dashboard(time.Now()).Providers["zhipu"]; frozen {
		t.Error("zhipu still frozen after API reset")
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/health/reset", nil))
	if rec.Code != 200 {
		t.Fatalf("empty-body reset: status = %d, want 200", rec.Code)
	}
}

// TestHealthResetAPI_MalformedJSON: a malformed body must NOT be treated as
// "empty = reset all" — it returns 400 and leaves the state untouched.
func TestHealthResetAPI_MalformedJSON(t *testing.T) {
	w, p := newTestWeb(t)
	mux := http.NewServeMux()
	w.Register(mux)
	p.recordRateLimit("zhipu", time.Now().Add(time.Hour), rlQuota)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/health/reset", strings.NewReader(`{"provider":`)))
	if rec.Code != 400 {
		t.Fatalf("malformed body: status = %d, want 400", rec.Code)
	}
	if _, frozen := p.runtimeState.Dashboard(time.Now()).Providers["zhipu"]; !frozen {
		t.Error("malformed body must leave frozen state untouched")
	}
}

// TestHealthResetAPI_PersistsClearedState (P1-3b): after a successful reset,
// the on-disk state file no longer carries the frozen entry — a restart right
// after must NOT resurrect it.
func TestHealthResetAPI_PersistsClearedState(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	w, p := newTestWeb(t)
	mux := http.NewServeMux()
	w.Register(mux)
	p.recordRateLimit("zhipu", time.Now().Add(time.Hour), rlQuota)
	if err := p.quota.Persist(); err != nil {
		t.Fatal(err)
	}
	statePath := p.quota.Path
	if data, _ := os.ReadFile(statePath); !strings.Contains(string(data), "zhipu") {
		t.Fatalf("precondition: frozen entry should be on disk: %s", data)
	}

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/health/reset", strings.NewReader(`{"provider":"zhipu"}`)))
	if rec.Code != 200 {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	// Check the HEALTH section specifically — the providers section may
	// legitimately hold a (BillingUnknown) quota snapshot for the same name.
	var wrap struct {
		Health map[string]json.RawMessage `json:"health"`
	}
	if err := json.Unmarshal(data, &wrap); err != nil {
		t.Fatal(err)
	}
	if _, frozen := wrap.Health["zhipu"]; frozen {
		t.Errorf("frozen entry still on disk after reset: %s", data)
	}
}

// ---- freeze_test.go ----

// TestFreezeHealth: the proxy method marks providers operator-frozen (pooled
// parent = all its virtual accounts, unknown/empty = no match — freeze has no
// freeze-all, unlike resetHealth) — and never touches rate-limit cooldowns,
// model locks, param blocklists, sticky, or pins.
func TestFreezeHealth(t *testing.T) {
	cfg := &configdomain.Config{Providers: map[string]configdomain.Provider{
		"a": {OpenAIBaseURL: "http://x", Provider: testProviderID},
		"b": {OpenAIBaseURL: "http://y", Provider: testProviderID},
	}}
	p := newTestProxy(t, cfg)
	p.mu.Lock()
	p.parentOf = map[string]string{"a#v1": "a", "a#v2": "a"}
	p.providers["a#v1"] = &testProv{key: "a1"}
	p.providers["a#v2"] = &testProv{key: "a2"}
	p.providers["b"] = &testProv{key: "b"}
	p.mu.Unlock()
	now := time.Now()
	p.recordRateLimit("a#v1", now.Add(time.Hour), rlQuota)
	p.recordModelFailure("a#v1", "m1", configdomain.Scheduling{ModelLockout: "1h"})
	p.learnParamBlock("a#v1", "m1", "max_tokens")
	seedRuntimeSticky(t, p, "route1", "a#v1", now)
	p.runtimeState.SetPin("route2", runtimestate.Pin{Provider: "a#v1"})

	if frozen := p.freezeHealth("missing", nil); len(frozen) != 0 {
		t.Errorf("unknown freeze = %v, want empty (no entries created)", frozen)
	}
	if frozen := p.freezeHealth("", nil); len(frozen) != 0 {
		t.Errorf("empty-name freeze = %v, want empty (no freeze-all)", frozen)
	}
	if got := len(p.runtimeState.Dashboard(now).Providers); got != 1 {
		t.Errorf("unknown/empty freeze created health entries: %d, want only the rate-limited a#v1", got)
	}

	frozen := p.freezeHealth("a", nil)
	if len(frozen) != 3 || frozen[0] != "a" || frozen[1] != "a#v1" || frozen[2] != "a#v2" {
		t.Errorf("frozen = %v, want [a a#v1 a#v2] (direct name + pooled virtuals)", frozen)
	}
	snapshot := p.runtimeState.Dashboard(now)
	for _, name := range []string{"a", "a#v1", "a#v2"} {
		status := snapshot.Providers[name]
		if !status.Frozen || status.Available {
			t.Errorf("%s status = %+v, want frozen + unavailable", name, status)
		}
	}
	// Freeze never disturbs the other state dimensions — the rate-limit
	// cooldown on a#v1 survives next to the freeze.
	if !snapshot.Providers["a#v1"].RateLimitedUntil.After(now) {
		t.Error("rate-limit cooldown lost to freeze")
	}
	if !p.runtimeState.ModelLocked("a#v1", "m1", now) ||
		!p.runtimeState.ParamBlocked("a#v1", "m1", "max_tokens") {
		t.Error("model lock / param blocklist disturbed by freeze")
	}
	if _, stickyOK := p.runtimeState.Sticky("route1"); !stickyOK || len(p.runtimeState.Pins(now)) != 1 {
		t.Error("sticky/pins disturbed by freeze")
	}
	if _, bHas := snapshot.Providers["b"]; bHas {
		t.Error("unmatched provider b got a health entry")
	}

	// Unfreeze (ResetHealth) is the only way back.
	p.resetHealth("a")
	if p.runtimeState.Dashboard(now).Providers["a#v1"].Frozen {
		t.Error("unfreeze did not clear the manual freeze")
	}
}

// TestHealthFreezeAPI: POST /api/health/freeze — {"provider":name} freezes one
// (pooled parent = all accounts); an empty/missing provider is REJECTED with
// 400 (freeze has no freeze-all, unlike /api/health/reset). The response
// carries the frozen names and /api/status reports the entry frozen +
// unavailable.
func TestHealthFreezeAPI(t *testing.T) {
	w, p := newTestWeb(t)
	mux := http.NewServeMux()
	w.Register(mux)
	p.mu.Lock()
	p.providers["zhipu"] = &testProv{key: "z"}
	p.mu.Unlock()

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/health/freeze", strings.NewReader(`{"provider":"zhipu"}`)))
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var out struct {
		Frozen []string `json:"frozen"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Frozen) != 1 || out.Frozen[0] != "zhipu" {
		t.Errorf("response = %+v, want frozen [zhipu]", out)
	}
	status := p.runtimeState.Dashboard(time.Now()).Providers["zhipu"]
	if !status.Frozen || status.Available {
		t.Errorf("zhipu status after freeze = %+v", status)
	}
	// An unknown provider freezes nothing but is not an error.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/health/freeze", strings.NewReader(`{"provider":"missing"}`)))
	if rec.Code != 200 {
		t.Fatalf("unknown freeze: status = %d, want 200", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Frozen) != 0 {
		t.Errorf("unknown provider frozen = %v, want empty", out.Frozen)
	}
	// Empty body / missing provider is rejected — freeze has no freeze-all.
	for _, body := range []string{"", `{}`, `{"provider":""}`} {
		rec = httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/health/freeze", strings.NewReader(body)))
		if rec.Code != 400 {
			t.Fatalf("empty-provider freeze (%q): status = %d, want 400", body, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "provider is required") {
			t.Errorf("empty-provider freeze (%q): body = %s, want the required error", body, rec.Body.String())
		}
	}
	if got := len(p.runtimeState.Dashboard(time.Now()).Providers); got != 1 {
		t.Errorf("rejected empty freezes touched state: %d entries, want only zhipu", got)
	}
	// Unfreeze restores scheduling eligibility.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/health/reset", strings.NewReader(`{"provider":"zhipu"}`)))
	if rec.Code != 200 {
		t.Fatalf("unfreeze after freeze: status = %d", rec.Code)
	}
	if _, has := p.runtimeState.Dashboard(time.Now()).Providers["zhipu"]; has {
		t.Error("zhipu health entry survived unfreeze")
	}
}

// TestHealthFreezeAPI_MalformedJSON: a malformed body returns 400 and freezes
// nothing — it must never be silently coerced into any default scope.
func TestHealthFreezeAPI_MalformedJSON(t *testing.T) {
	w, p := newTestWeb(t)
	mux := http.NewServeMux()
	w.Register(mux)
	p.mu.Lock()
	p.providers["zhipu"] = &testProv{key: "z"}
	p.mu.Unlock()

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/health/freeze", strings.NewReader(`{"provider":`)))
	if rec.Code != 400 {
		t.Fatalf("malformed body: status = %d, want 400", rec.Code)
	}
	if got := len(p.runtimeState.Dashboard(time.Now()).Providers); got != 0 {
		t.Errorf("malformed body froze %d providers, want none", got)
	}
}

// TestHealthFreezeAPI_PersistsFrozenState: a successful freeze lands in the
// on-disk health section (frozen: true) and a restart with the same config
// restores it — the provider stays excluded from scheduling until unfreeze.
func TestHealthFreezeAPI_PersistsFrozenState(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	w, p := newTestWeb(t)
	mux := http.NewServeMux()
	w.Register(mux)
	p.mu.Lock()
	p.providers["zhipu"] = &testProv{key: "z"}
	p.mu.Unlock()
	statePath := p.quota.Path

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/health/freeze", strings.NewReader(`{"provider":"zhipu"}`)))
	if rec.Code != 200 {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var wrap struct {
		Health map[string]json.RawMessage `json:"health"`
	}
	if err := json.Unmarshal(data, &wrap); err != nil {
		t.Fatal(err)
	}
	var entry struct {
		Frozen bool `json:"frozen"`
	}
	if err := json.Unmarshal(wrap.Health["zhipu"], &entry); err != nil || !entry.Frozen {
		t.Errorf("freeze not on disk after API freeze: %s (err=%v)", wrap.Health["zhipu"], err)
	}

	// Restart on the same state file + config: the freeze restores.
	cfg, _ := configdomain.LoadConfigFromBytes("test", []byte(`listen: 127.0.0.1:0
providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://x}
`))
	p2 := newTestProxyAt(t, cfg, statePath)
	status := p2.runtimeState.Dashboard(time.Now()).Providers["zhipu"]
	if !status.Frozen || status.Available {
		t.Errorf("freeze not restored after restart: %+v", status)
	}
	if p2.runtimeState.TargetHealthy("zhipu", "m", time.Now()) {
		t.Error("restored freeze does not block scheduling")
	}
}

// TestHealthFreezeAPI_ProviderStaysInStatusPayload (UI regression): after
// freezing a provider, /api/status must still surface it — the WebUI
// Providers card enumerates rows from the schedule preview's ordered chains,
// and scheduling drops the frozen provider from `ordered`, so `health` is the
// only place the row (frozen pill + unfreeze button) can come from. This pins
// the backend half of that contract through the real API chain: health keeps
// the provider with frozen:true + available:false, while schedule ordered
// legitimately no longer lists it (it is excluded from scheduling).
func TestHealthFreezeAPI_ProviderStaysInStatusPayload(t *testing.T) {
	cfg, _ := configdomain.LoadConfigFromBytes("test", []byte(`listen: 127.0.0.1:0
providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://x}
routes:
  glm-5.2: [{provider: zhipu, model: glm-5.2}]
`))
	p := newTestProxy(t, cfg)
	p.mu.Lock()
	p.providers["zhipu"] = &testProv{key: "z"}
	p.mu.Unlock()
	w := NewWebServer(p, "test-config.yaml")
	mux := http.NewServeMux()
	w.Register(mux)

	type statusPayload struct {
		Health map[string]struct {
			Frozen    bool `json:"frozen"`
			Available bool `json:"available"`
		} `json:"health"`
		Schedule struct {
			Models map[string]struct {
				Ordered []struct {
					Provider string `json:"provider"`
				} `json:"ordered"`
			} `json:"models"`
		} `json:"schedule"`
	}
	getStatus := func() statusPayload {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/status", nil))
		if rec.Code != 200 {
			t.Fatalf("GET /api/status: %d (%s)", rec.Code, rec.Body.String())
		}
		var st statusPayload
		if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
			t.Fatal(err)
		}
		return st
	}

	before := getStatus()
	ordered := before.Schedule.Models["glm-5.2"].Ordered
	if len(ordered) != 1 || ordered[0].Provider != "zhipu" {
		t.Fatalf("precondition: zhipu should be in the schedule chain: %+v", ordered)
	}

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/health/freeze", strings.NewReader(`{"provider":"zhipu"}`)))
	if rec.Code != 200 {
		t.Fatalf("freeze: status = %d (%s)", rec.Code, rec.Body.String())
	}

	after := getStatus()
	entry, ok := after.Health["zhipu"]
	if !ok || !entry.Frozen || entry.Available {
		t.Errorf("frozen provider missing from /api/status health: %+v (frozen=%v available=%v)",
			after.Health, entry.Frozen, entry.Available)
	}
	for _, o := range after.Schedule.Models["glm-5.2"].Ordered {
		if o.Provider == "zhipu" {
			t.Error("frozen provider must be excluded from the schedule chain")
		}
	}
}
