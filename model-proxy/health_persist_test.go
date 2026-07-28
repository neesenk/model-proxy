package main

import (
	"path/filepath"
	"testing"
	"time"
)

// TestHealthPersist_RoundTrip: frozen health state (rate-limit + kind, circuit,
// model locks, param blocklist) persists to quota_state.json and is restored
// by a fresh Proxy; expired cooldowns are dropped.
func TestHealthPersist_RoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := &Config{Providers: map[string]Provider{"a": {OpenAIBaseURL: "http://x", Provider: "static"}}}
	statePath := filepath.Join(t.TempDir(), "quota_state.json")

	p1 := newTestProxyAt(t, cfg, statePath)
	now := time.Now()
	rateLimitUntil := now.Add(2 * time.Hour)
	p1.recordRateLimit("a", rateLimitUntil, rlQuota)
	modelLockBefore := time.Now().Add(time.Hour)
	p1.recordModelFailure("a", "m1", Scheduling{ModelLockout: "1h"})
	modelLockAfter := time.Now().Add(time.Hour)
	p1.learnParamBlock("a", "m1", "max_tokens")
	// Circuit on a second provider + an already-expired rate limit (must drop).
	circuitUntil := now.Add(10 * time.Minute)
	p1.healthMu.Lock()
	p1.health["b"] = &providerHealth{consecutiveFailures: 3, circuitOpenUntil: circuitUntil}
	p1.health["c"] = &providerHealth{rateLimitedUntil: now.Add(-time.Minute)}
	p1.healthMu.Unlock()
	if err := p1.quota.persist(); err != nil {
		t.Fatalf("persist source state: %v", err)
	}
	p1.Close()

	p2 := newTestProxyAt(t, cfg, statePath)
	p2.healthMu.Lock()
	defer p2.healthMu.Unlock()

	h := p2.health["a"]
	if h == nil {
		t.Fatalf("provider a rate-limit not restored: %+v", h)
	}
	if !h.rateLimitedUntil.Equal(rateLimitUntil) {
		t.Errorf("rateLimitedUntil = %s, want %s", h.rateLimitedUntil, rateLimitUntil)
	}
	if h.rateLimitKind != rlQuota {
		t.Errorf("rateLimitKind = %v want rlQuota", h.rateLimitKind)
	}
	if e := p2.modelLocks[modelLockKey{provider: "a", model: "m1"}]; e == nil {
		t.Errorf("model lock (a,m1) not restored: %+v", e)
	} else if e.lockedUntil.Before(modelLockBefore) || e.lockedUntil.After(modelLockAfter) {
		t.Errorf("model lock until %s outside [%s, %s]", e.lockedUntil, modelLockBefore, modelLockAfter)
	}
	if !p2.paramBlock[modelLockKey{provider: "a", model: "m1"}]["max_tokens"] {
		t.Error("param blocklist for a not restored")
	}

	hb := p2.health["b"]
	if hb == nil {
		t.Fatalf("provider b circuit not restored: %+v", hb)
	}
	if !hb.circuitOpenUntil.Equal(circuitUntil) {
		t.Errorf("circuitOpenUntil = %s, want %s", hb.circuitOpenUntil, circuitUntil)
	}
	if hb.consecutiveFailures != cfg.Scheduling.Threshold() {
		t.Errorf("restored circuit failures = %d, want threshold %d (next failure re-opens)",
			hb.consecutiveFailures, cfg.Scheduling.Threshold())
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
	statePath := filepath.Join(t.TempDir(), "quota_state.json")
	p1 := newTestProxyAt(t, cfgA, statePath)
	p1.recordRateLimit("a", time.Now().Add(2*time.Hour), rlQuota)
	if err := p1.quota.persist(); err != nil {
		t.Fatalf("persist source state: %v", err)
	}
	p1.Close()

	// Same provider NAME, different upstream URL → different fingerprint.
	cfgB := &Config{Providers: map[string]Provider{"a": {OpenAIBaseURL: "http://y", Provider: "static"}}}
	p2 := newTestProxyAt(t, cfgB, statePath)
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
	statePath := filepath.Join(t.TempDir(), "quota_state.json")
	p1 := newTestProxyAt(t, cfg, statePath)
	p1.learnParamBlock("a", "m1", "max_tokens")
	if err := p1.quota.persist(); err != nil {
		t.Fatalf("persist source state: %v", err)
	}
	p1.Close()

	p2 := newTestProxyAt(t, cfg, statePath)
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
