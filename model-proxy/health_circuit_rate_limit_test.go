package main

import (
	"net/http"
	"testing"
	"time"
)

// --- releaseHalfOpenSlot: clears halfOpenInFlight; nil-health is a no-op ---

func TestReleaseHalfOpenSlot(t *testing.T) {
	cfg := &Config{Providers: map[string]Provider{"a": {OpenAIBaseURL: "http://x", Provider: "static"}}}
	p := newTestProxy(t, cfg)
	// No health entry yet → no-op (no panic).
	p.releaseHalfOpenSlot("a")
	// Set up a half-open slot, then release it.
	p.healthMu.Lock()
	h := &providerHealth{halfOpenInFlight: true}
	p.health["a"] = h
	p.healthMu.Unlock()
	p.releaseHalfOpenSlot("a")
	p.healthMu.Lock()
	stillInFlight := p.health["a"].halfOpenInFlight
	p.healthMu.Unlock()
	if stillInFlight {
		t.Error("releaseHalfOpenSlot did not clear halfOpenInFlight")
	}
}

// --- takeHalfOpenSlot + releaseHalfOpenSlot lifecycle ---

func TestTakeHalfOpenSlot_Lifecycle(t *testing.T) {
	cfg := &Config{Providers: map[string]Provider{"a": {OpenAIBaseURL: "http://x", Provider: "static"}}}
	p := newTestProxy(t, cfg)
	// Closed circuit → take succeeds, no slot reserved.
	if !p.takeHalfOpenSlot("a") {
		t.Error("takeHalfOpenSlot on closed circuit: want true")
	}
	p.releaseHalfOpenSlot("a") // release is a no-op when no slot reserved

	// Open the circuit, then half-open allows exactly one probe.
	p.healthMu.Lock()
	p.health["a"] = &providerHealth{circuitOpenUntil: time.Now().Add(-1 * time.Minute)} // cooldown expired → half-open
	p.healthMu.Unlock()
	if !p.takeHalfOpenSlot("a") {
		t.Error("half-open first probe: want true")
	}
	// Second probe while one is in flight → blocked.
	if p.takeHalfOpenSlot("a") {
		t.Error("half-open second probe: want false (single-flight)")
	}
	// Release → next probe allowed.
	p.releaseHalfOpenSlot("a")
	if !p.takeHalfOpenSlot("a") {
		t.Error("half-open after release: want true")
	}
}

// --- takeHalfOpenSlot: circuit still open → false ---

func TestTakeHalfOpenSlot_CircuitOpen(t *testing.T) {
	cfg := &Config{Providers: map[string]Provider{"a": {OpenAIBaseURL: "http://x", Provider: "static"}}}
	p := newTestProxy(t, cfg)
	p.healthMu.Lock()
	p.health["a"] = &providerHealth{circuitOpenUntil: time.Now().Add(5 * time.Minute)} // still open
	p.healthMu.Unlock()
	if p.takeHalfOpenSlot("a") {
		t.Error("circuit open: takeHalfOpenSlot want false")
	}
}

// --- takeHalfOpenSlot: rate-limited → false ---

func TestTakeHalfOpenSlot_RateLimited(t *testing.T) {
	cfg := &Config{Providers: map[string]Provider{"a": {OpenAIBaseURL: "http://x", Provider: "static"}}}
	p := newTestProxy(t, cfg)
	p.healthMu.Lock()
	p.health["a"] = &providerHealth{rateLimitedUntil: time.Now().Add(5 * time.Minute)}
	p.healthMu.Unlock()
	if p.takeHalfOpenSlot("a") {
		t.Error("rate-limited: takeHalfOpenSlot want false")
	}
}

// --- recordRateLimit extends (not shortens) the until time ---

func TestRecordRateLimit_Extends(t *testing.T) {
	cfg := &Config{Providers: map[string]Provider{"a": {OpenAIBaseURL: "http://x", Provider: "static"}}}
	p := newTestProxy(t, cfg)
	now := time.Now()
	first := now.Add(60 * time.Second)
	p.recordRateLimit("a", first, rlQuota)
	// A shorter until must NOT overwrite the longer one — and its kind must not
	// overwrite the winning horizon's kind either.
	p.recordRateLimit("a", now.Add(10*time.Second), rlTransient)
	p.healthMu.Lock()
	got := p.health["a"].rateLimitedUntil
	kind := p.health["a"].rateLimitKind
	p.healthMu.Unlock()
	if !got.Equal(first) {
		t.Errorf("recordRateLimit extended: got %v want %v (should keep the later)", got, first)
	}
	if kind != rlQuota {
		t.Errorf("rateLimitKind = %v want rlQuota (kind follows the winning horizon)", kind)
	}
}

// --- recordSuccess clears the circuit ---

func TestRecordSuccess_ClearsCircuit(t *testing.T) {
	cfg := &Config{Providers: map[string]Provider{"a": {OpenAIBaseURL: "http://x", Provider: "static"}}}
	p := newTestProxy(t, cfg)
	p.healthMu.Lock()
	p.health["a"] = &providerHealth{consecutiveFailures: 5, circuitOpenUntil: time.Now().Add(5 * time.Minute), halfOpenInFlight: true}
	p.healthMu.Unlock()
	p.recordSuccess("a", "m1")
	p.healthMu.Lock()
	h := p.health["a"]
	p.healthMu.Unlock()
	if h.consecutiveFailures != 0 || !h.circuitOpenUntil.IsZero() || h.halfOpenInFlight {
		t.Errorf("recordSuccess did not reset health: %+v", h)
	}
}

// --- recordFailure opens circuit at threshold ---

func TestRecordFailure_OpensCircuitAtThreshold(t *testing.T) {
	cfg := &Config{Providers: map[string]Provider{"a": {OpenAIBaseURL: "http://x", Provider: "static"}}, Scheduling: Scheduling{CircuitThreshold: 2, CircuitCooldown: "5m"}}
	p := newTestProxy(t, cfg)
	p.recordFailure("a", cfg.Scheduling)
	p.recordFailure("a", cfg.Scheduling) // reaches threshold
	p.healthMu.Lock()
	h := p.health["a"]
	p.healthMu.Unlock()
	if h.circuitOpenUntil.IsZero() {
		t.Error("circuit not opened after threshold failures")
	}
	if h.halfOpenInFlight {
		t.Error("halfOpenInFlight should be cleared on failure")
	}
}

// --- parseRateLimit: body hint > Retry-After > per-class default ---

func TestParseRateLimit(t *testing.T) {
	cfg := &Config{Providers: map[string]Provider{"a": {OpenAIBaseURL: "http://x", Provider: "static"}}}
	p := newTestProxy(t, cfg)
	sched := Scheduling{RateLimitBackoff: "60s", QuotaCooldown: "2h"}
	now := time.Now()

	// Seconds.
	r := &http.Response{Header: http.Header{"Retry-After": []string{"120"}}}
	got, kind := p.parseRateLimit(r, nil, now, sched)
	if d := got.Sub(now); d < 119*time.Second || d > 121*time.Second {
		t.Errorf("seconds Retry-After: got %v want ~120s", d)
	}
	if kind != rlTransient {
		t.Errorf("no body: kind = %v want transient", kind)
	}
	// Negative seconds clamped to 0.
	r = &http.Response{Header: http.Header{"Retry-After": []string{"-5"}}}
	got, _ = p.parseRateLimit(r, nil, now, sched)
	if got.Before(now) {
		t.Errorf("negative Retry-After: got %v before now", got)
	}
	// HTTP-date.
	future := now.Add(2 * time.Hour)
	r = &http.Response{Header: http.Header{"Retry-After": []string{future.UTC().Format(http.TimeFormat)}}}
	got, _ = p.parseRateLimit(r, nil, now, sched)
	if got.Unix() != future.Unix() {
		t.Errorf("HTTP-date Retry-After: got %v want %v", got, future)
	}
	// HTTP-date in the past → clamped to now.
	past := now.Add(-1 * time.Hour)
	r = &http.Response{Header: http.Header{"Retry-After": []string{past.UTC().Format(http.TimeFormat)}}}
	got, _ = p.parseRateLimit(r, nil, now, sched)
	if got.Before(now) {
		t.Errorf("past HTTP-date: got %v before now", got)
	}
	// Missing Retry-After → default backoff.
	r = &http.Response{Header: http.Header{}}
	got, _ = p.parseRateLimit(r, nil, now, sched)
	if d := got.Sub(now); d != 60*time.Second {
		t.Errorf("missing Retry-After: got %v want 60s", d)
	}
	// Garbage Retry-After → default backoff.
	r = &http.Response{Header: http.Header{"Retry-After": []string{"not-a-number-or-date"}}}
	got, _ = p.parseRateLimit(r, nil, now, sched)
	if d := got.Sub(now); d != 60*time.Second {
		t.Errorf("garbage Retry-After: got %v want 60s", d)
	}

	// Body reset hint beats the Retry-After header.
	r = &http.Response{Header: http.Header{"Retry-After": []string{"120"}}}
	got, kind = p.parseRateLimit(r, []byte(`{"error":"quota exceeded, reset after 2h5m"}`), now, sched)
	if d := got.Sub(now); d < 2*time.Hour+4*time.Minute || d > 2*time.Hour+6*time.Minute {
		t.Errorf("body hint should win over header: got %v want ~2h5m", d)
	}
	if kind != rlQuota {
		t.Errorf("quota body: kind = %v want rlQuota", kind)
	}

	// Quota class without any hint → quota_cooldown (2h here).
	r = &http.Response{Header: http.Header{}}
	got, kind = p.parseRateLimit(r, []byte(`{"error":"insufficient_quota"}`), now, sched)
	if d := got.Sub(now); d != 2*time.Hour {
		t.Errorf("quota class default: got %v want 2h", d)
	}
	if kind != rlQuota {
		t.Errorf("kind = %v want rlQuota", kind)
	}

	// Daily class without any hint → next local midnight.
	got, kind = p.parseRateLimit(r, []byte(`{"error":"today's quota exhausted"}`), now, sched)
	if kind != rlDaily {
		t.Errorf("kind = %v want rlDaily", kind)
	}
	midnight := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location())
	if !got.Equal(midnight) {
		t.Errorf("daily class: got %v want local midnight %v", got, midnight)
	}
}
