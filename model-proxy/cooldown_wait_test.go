package main

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// cooldownCfg builds a two-target route over the given upstreams with the
// wait-retry knobs set for fast tests (retry_wait 10s = real default; short
// backoffs elsewhere).
func cooldownCfg(primary, fallback *httptest.Server, retryWait string) *Config {
	return &Config{
		Providers: map[string]Provider{
			"primary":  {OpenAIBaseURL: primary.URL, Provider: "static"},
			"fallback": {OpenAIBaseURL: fallback.URL, Provider: "static"},
		},
		Routes: map[string][]RouteTarget{
			"m1": {
				{Provider: "primary", Model: "m1", Priority: 1},
				{Provider: "fallback", Model: "m1", Priority: 2},
			},
		},
		Scheduling: Scheduling{
			CircuitThreshold: 3, CircuitCooldown: "5m", RateLimitBackoff: "10s",
			UpstreamTimeout: "5s", StickyDwell: "0s", RetryWait: retryWait,
		},
	}
}

// TestCooldownWait_ShortCooldownSucceeds: every target 429s with a 1s
// Retry-After → instead of an immediate error the proxy waits out the cooldown
// and the retried pass succeeds.
func TestCooldownWait_ShortCooldownSucceeds(t *testing.T) {
	once429 := func() func(int) (int, string, http.Header, time.Duration) {
		return func(c int) (int, string, http.Header, time.Duration) {
			if c == 1 {
				return 429, `{"e":"rate"}`, intHdr("Retry-After", "1"), 0
			}
			return 200, `{"ok":true}`, nil, 0
		}
	}
	primary, pHits := newHitServer(once429())
	defer primary.Close()
	fallback, fHits := newHitServer(once429())
	defer fallback.Close()
	cfg := cooldownCfg(primary, fallback, "10s")
	_, px := newModelLockProxy(t, cfg)

	start := time.Now()
	st := postStatus(t, px.URL+"/v1/chat/completions", `{"model":"m1","messages":[]}`)
	elapsed := time.Since(start)
	if st != 200 {
		t.Fatalf("status = %d, want 200 (wait + retry should recover)", st)
	}
	if elapsed < 900*time.Millisecond {
		t.Errorf("elapsed = %v, want ≥ ~1s (the cooldown was waited out)", elapsed)
	}
	if got := pHits.Load(); got != 2 {
		t.Errorf("primary hits = %d, want 2 (429 then recovered probe)", got)
	}
	if got := fHits.Load(); got != 1 {
		t.Errorf("fallback hits = %d, want 1 (only the first pass)", got)
	}
}

// TestCooldownWait_LongCooldown429: every target rate-limited for 60s (beyond
// retry_wait) → no waiting; terminal 429 + Retry-After, not an opaque 502.
func TestCooldownWait_LongCooldown429(t *testing.T) {
	always429 := func(int) (int, string, http.Header, time.Duration) {
		return 429, `{"e":"rate"}`, intHdr("Retry-After", "60"), 0
	}
	primary, _ := newHitServer(always429)
	defer primary.Close()
	fallback, _ := newHitServer(always429)
	defer fallback.Close()
	cfg := cooldownCfg(primary, fallback, "10s")
	_, px := newModelLockProxy(t, cfg)

	start := time.Now()
	resp, err := http.Post(px.URL+"/v1/chat/completions", "application/json", stringReader(`{"model":"m1","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	ra := resp.Header.Get("Retry-After")
	resp.Body.Close()
	if resp.StatusCode != 429 {
		t.Errorf("status = %d, want 429 (pure rate-limit terminal)", resp.StatusCode)
	}
	if secs, _ := strconv.Atoi(ra); secs < 50 || secs > 61 {
		t.Errorf("Retry-After = %q, want ~60", ra)
	}
	if elapsed > 5*time.Second {
		t.Errorf("elapsed = %v, want no waiting (cooldown beyond retry_wait)", elapsed)
	}
}

// TestCooldownWait_MixedFailures502: a hard-failing target (500) mixed with a
// rate-limited one → NOT a pure rate-limit situation → terminal stays 502.
func TestCooldownWait_MixedFailures502(t *testing.T) {
	primary, _ := newHitServer(func(int) (int, string, http.Header, time.Duration) {
		return 500, `{"e":"broken"}`, nil, 0
	})
	defer primary.Close()
	fallback, _ := newHitServer(func(int) (int, string, http.Header, time.Duration) {
		return 429, `{"e":"rate"}`, intHdr("Retry-After", "60"), 0
	})
	defer fallback.Close()
	cfg := cooldownCfg(primary, fallback, "10s")
	_, px := newModelLockProxy(t, cfg)

	if st := postStatus(t, px.URL+"/v1/chat/completions", `{"model":"m1","messages":[]}`); st != 502 {
		t.Errorf("status = %d, want 502 (hard failure mixed in)", st)
	}
}

// TestCooldownWait_Disabled: retry_wait "0" disables waiting; classification
// still applies (429 terminal), and the answer is immediate.
func TestCooldownWait_Disabled(t *testing.T) {
	always429 := func(int) (int, string, http.Header, time.Duration) {
		return 429, `{"e":"rate"}`, intHdr("Retry-After", "1"), 0
	}
	primary, pHits := newHitServer(always429)
	defer primary.Close()
	fallback, _ := newHitServer(always429)
	defer fallback.Close()
	cfg := cooldownCfg(primary, fallback, "0")
	_, px := newModelLockProxy(t, cfg)

	start := time.Now()
	st := postStatus(t, px.URL+"/v1/chat/completions", `{"model":"m1","messages":[]}`)
	elapsed := time.Since(start)
	if st != 429 {
		t.Errorf("status = %d, want 429", st)
	}
	if elapsed > time.Second {
		t.Errorf("elapsed = %v, want no waiting (retry_wait disabled)", elapsed)
	}
	if got := pHits.Load(); got != 1 {
		t.Errorf("primary hits = %d, want 1 (no retry rounds)", got)
	}
}

// TestCooldownWait_RetriesExhausted: cooldowns keep being re-armed on every
// retry → after the round budget is spent the proxy gives up with the terminal
// 429 (+ Retry-After). Assertions are on the FINAL STATE and the total wait
// budget — NOT on exact per-round hit counts, which legitimately vary by
// millisecond-level cooldown stagger (a pass may skip a still-cooling target).
func TestCooldownWait_RetriesExhausted(t *testing.T) {
	always429 := func(int) (int, string, http.Header, time.Duration) {
		return 429, `{"e":"rate"}`, intHdr("Retry-After", "1"), 0
	}
	primary, pHits := newHitServer(always429)
	defer primary.Close()
	fallback, fHits := newHitServer(always429)
	defer fallback.Close()
	cfg := cooldownCfg(primary, fallback, "10s")
	_, px := newModelLockProxy(t, cfg)

	start := time.Now()
	resp, err := http.Post(px.URL+"/v1/chat/completions", "application/json", stringReader(`{"model":"m1","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	ra := resp.Header.Get("Retry-After")
	resp.Body.Close()
	if resp.StatusCode != 429 {
		t.Errorf("status = %d, want 429 (retries exhausted, pure rate-limit terminal)", resp.StatusCode)
	}
	if secs, _ := strconv.Atoi(ra); secs < 1 {
		t.Errorf("Retry-After = %q, want ≥ 1 (earliest cooldown horizon)", ra)
	}
	// Budget: 1 initial pass + ≤2 retry rounds. A retry round may be a ~1s sleep
	// OR a zero-wait recovered-target re-pass, so wall-clock varies by
	// millisecond-level cooldown stagger: [~1s, ~6s].
	if elapsed < 900*time.Millisecond {
		t.Errorf("elapsed = %v, want ≥ ~0.9s (at least one cooldown wait)", elapsed)
	}
	if elapsed > 6*time.Second {
		t.Errorf("elapsed = %v, exceeds the total wait budget", elapsed)
	}
	// At least one retry round must have happened (initial 2 hits + ≥2 more).
	if got := pHits.Load() + fHits.Load(); got < 4 {
		t.Errorf("total hits = %d, want ≥ 4 (initial pass + retry rounds)", got)
	}
	// Upper bound: initial 2 hits + ≤2 retry rounds × 2 targets = ≤6. Catches a
	// retry-budget off-by-one (e.g. round < 5) that the lower bound can't.
	if got := pHits.Load() + fHits.Load(); got > 6 {
		t.Errorf("total hits = %d, want ≤ 6 (initial pass + ≤2 retry rounds)", got)
	}
}

// TestCooldownWait_RecoveredTargetGetsAChance (P0-5 TOCTOU): one target keeps
// re-arming its 429 while the other recovers — the recovered target must be
// tried (zero-wait re-schedule), never skipped to a premature 502/429.
func TestCooldownWait_RecoveredTargetGetsAChance(t *testing.T) {
	primary, _ := newHitServer(func(int) (int, string, http.Header, time.Duration) {
		return 429, `{"e":"rate"}`, intHdr("Retry-After", "1"), 0 // never recovers
	})
	defer primary.Close()
	fallback, fHits := newHitServer(func(c int) (int, string, http.Header, time.Duration) {
		if c == 1 {
			return 429, `{"e":"rate"}`, intHdr("Retry-After", "1"), 0
		}
		return 200, `{"ok":true}`, nil, 0 // recovers after the first 429
	})
	defer fallback.Close()
	cfg := cooldownCfg(primary, fallback, "10s")
	_, px := newModelLockProxy(t, cfg)

	if st := postStatus(t, px.URL+"/v1/chat/completions", `{"model":"m1","messages":[]}`); st != 200 {
		t.Errorf("status = %d, want 200 (recovered fallback must be tried)", st)
	}
	if got := fHits.Load(); got < 2 {
		t.Errorf("fallback hits = %d, want ≥ 2 (429, then the recovered serve)", got)
	}
}
