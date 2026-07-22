package main

import (
	"testing"
	"time"
)

// TestHealthPersist_RoundTrip: frozen health state (rate-limit + kind, circuit,
// model locks, param blocklist) persists to quota_state.json and is restored
// by a fresh Proxy; expired cooldowns are dropped.
func TestHealthPersist_RoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := &Config{Providers: map[string]Provider{"a": {OpenAIBaseURL: "http://x", Provider: "static"}}}

	p1 := NewProxy(cfg)
	now := time.Now()
	p1.recordRateLimit("a", now.Add(2*time.Hour), rlQuota)
	p1.recordModelFailure("a", "m1", Scheduling{ModelLockout: "1h"})
	p1.learnParamBlock("a", "m1", "max_tokens")
	// Circuit on a second provider + an already-expired rate limit (must drop).
	p1.healthMu.Lock()
	p1.health["b"] = &providerHealth{consecutiveFailures: 3, circuitOpenUntil: now.Add(10 * time.Minute)}
	p1.health["c"] = &providerHealth{rateLimitedUntil: now.Add(-time.Minute)}
	p1.healthMu.Unlock()
	p1.quota.persist()

	p2 := NewProxy(cfg)
	p2.healthMu.Lock()
	defer p2.healthMu.Unlock()

	h := p2.health["a"]
	if h == nil || !now.Before(h.rateLimitedUntil) {
		t.Fatalf("provider a rate-limit not restored: %+v", h)
	}
	if h.rateLimitKind != rlQuota {
		t.Errorf("rateLimitKind = %v want rlQuota", h.rateLimitKind)
	}
	if e := p2.modelLocks[modelLockKey{provider: "a", model: "m1"}]; e == nil || !now.Before(e.lockedUntil) {
		t.Errorf("model lock (a,m1) not restored: %+v", e)
	}
	if !p2.paramBlock[modelLockKey{provider: "a", model: "m1"}]["max_tokens"] {
		t.Error("param blocklist for a not restored")
	}

	hb := p2.health["b"]
	if hb == nil || !now.Before(hb.circuitOpenUntil) {
		t.Fatalf("provider b circuit not restored: %+v", hb)
	}
	if hb.consecutiveFailures != cfg.Scheduling.threshold() {
		t.Errorf("restored circuit failures = %d, want threshold %d (next failure re-opens)",
			hb.consecutiveFailures, cfg.Scheduling.threshold())
	}

	if _, ok := p2.health["c"]; ok {
		t.Error("expired rate-limit on c must be dropped, not restored")
	}
}

// TestHealthPersist_FingerprintMismatch: health frozen under one config must
// NOT restore onto a different config that reuses the same provider names
// (test binaries share the state file; daemons restart with edited configs).
func TestHealthPersist_FingerprintMismatch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfgA := &Config{Providers: map[string]Provider{"a": {OpenAIBaseURL: "http://x", Provider: "static"}}}
	p1 := NewProxy(cfgA)
	p1.recordRateLimit("a", time.Now().Add(2*time.Hour), rlQuota)
	p1.quota.persist()

	// Same provider NAME, different upstream URL → different fingerprint.
	cfgB := &Config{Providers: map[string]Provider{"a": {OpenAIBaseURL: "http://y", Provider: "static"}}}
	p2 := NewProxy(cfgB)
	p2.healthMu.Lock()
	_, frozen := p2.health["a"]
	p2.healthMu.Unlock()
	if frozen {
		t.Error("health must not restore under a mismatched config fingerprint")
	}
}

// TestHealthPersist_ParamBlockOnlyProvider: a provider with ONLY learned params
// (never failed → no health entry) must still persist + restore its blocklist.
func TestHealthPersist_ParamBlockOnlyProvider(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := &Config{Providers: map[string]Provider{"a": {OpenAIBaseURL: "http://x", Provider: "static"}}}
	p1 := NewProxy(cfg)
	p1.learnParamBlock("a", "m1", "max_tokens")
	p1.quota.persist()

	p2 := NewProxy(cfg)
	p2.healthMu.Lock()
	blocked := p2.paramBlock[modelLockKey{provider: "a", model: "m1"}]["max_tokens"]
	_, hasHealth := p2.health["a"]
	p2.healthMu.Unlock()
	if !blocked {
		t.Error("param blocklist must persist even without a health entry")
	}
	if hasHealth {
		t.Error("no health entry should be synthesized for a param-only provider")
	}
}
