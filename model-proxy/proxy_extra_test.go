package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"model-proxy/provider"
)

// proxy_extra_test.go covers small uncovered proxy.go helpers + quota.go
// stop/pollAfter + authAdapter.Refresh + the half-open slot lifecycle.

// --- main-package ensureJSONField ---

func TestEnsureJSONField_MainPkg(t *testing.T) {
	// Absent key → injected.
	got := ensureJSONField([]byte(`{"model":"x"}`), "store", false)
	var m map[string]any
	json.Unmarshal(got, &m)
	if v, ok := m["store"].(bool); !ok || v != false {
		t.Errorf("store not injected: %v", m)
	}
	// Existing key → untouched.
	got = ensureJSONField([]byte(`{"model":"x","store":true}`), "store", false)
	json.Unmarshal(got, &m)
	if v, _ := m["store"].(bool); v != true {
		t.Errorf("existing store overwritten: %v", m)
	}
	// Non-JSON body → returned unchanged.
	orig := []byte(`not-json`)
	if got := ensureJSONField(orig, "store", false); string(got) != string(orig) {
		t.Errorf("non-JSON body changed: %q", got)
	}
}

// --- releaseHalfOpenSlot: clears halfOpenInFlight; nil-health is a no-op ---

func TestReleaseHalfOpenSlot(t *testing.T) {
	cfg := &Config{Providers: map[string]Provider{"a": {OpenAIBaseURL: "http://x", Provider: "static"}}}
	p := NewProxy(cfg)
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
	p := NewProxy(cfg)
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
	p := NewProxy(cfg)
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
	p := NewProxy(cfg)
	p.healthMu.Lock()
	p.health["a"] = &providerHealth{rateLimitedUntil: time.Now().Add(5 * time.Minute)}
	p.healthMu.Unlock()
	if p.takeHalfOpenSlot("a") {
		t.Error("rate-limited: takeHalfOpenSlot want false")
	}
}

// --- quotaTracker.stop / pollAfter ---

func TestQuotaTracker_Stop(t *testing.T) {
	dir := t.TempDir()
	cfg := func() *Config { return &Config{} }
	provs := func() map[string]provider.Provider { return nil }
	tr := newQuotaTracker(dir+"/q.json", cfg, provs)
	tr.start()
	// stop must be idempotent and not block.
	tr.stop()
	tr.stop() // second stop is a no-op (sync.Once)
}

func TestQuotaTracker_PollAfter(t *testing.T) {
	dir := t.TempDir()
	var called atomic.Int32
	cfg := func() *Config {
		called.Add(1)
		return &Config{}
	}
	provs := func() map[string]provider.Provider { return nil }
	tr := newQuotaTracker(dir+"/q.json", cfg, provs)
	tr.start()
	defer tr.stop()
	tr.pollAfter(50 * time.Millisecond)
	time.Sleep(300 * time.Millisecond) // let the pollAfter-driven poll fire
	// pollAfter → pollAll → cfg(). Don't assert an exact count (the 10s bootstrap
	// poll from start() may or may not have fired within this window); just confirm
	// the pollAfter path ran at least once.
	if called.Load() < 1 {
		t.Errorf("pollAfter did not trigger a poll: cfg called %d times", called.Load())
	}
}

// --- recordRateLimit extends (not shortens) the until time ---

func TestRecordRateLimit_Extends(t *testing.T) {
	cfg := &Config{Providers: map[string]Provider{"a": {OpenAIBaseURL: "http://x", Provider: "static"}}}
	p := NewProxy(cfg)
	now := time.Now()
	first := now.Add(60 * time.Second)
	p.recordRateLimit("a", first)
	// A shorter until must NOT overwrite the longer one.
	p.recordRateLimit("a", now.Add(10*time.Second))
	p.healthMu.Lock()
	got := p.health["a"].rateLimitedUntil
	p.healthMu.Unlock()
	if !got.Equal(first) {
		t.Errorf("recordRateLimit extended: got %v want %v (should keep the later)", got, first)
	}
}

// --- recordSuccess clears the circuit ---

func TestRecordSuccess_ClearsCircuit(t *testing.T) {
	cfg := &Config{Providers: map[string]Provider{"a": {OpenAIBaseURL: "http://x", Provider: "static"}}}
	p := NewProxy(cfg)
	p.healthMu.Lock()
	p.health["a"] = &providerHealth{consecutiveFailures: 5, circuitOpenUntil: time.Now().Add(5 * time.Minute), halfOpenInFlight: true}
	p.healthMu.Unlock()
	p.recordSuccess("a")
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
	p := NewProxy(cfg)
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

// --- parseRateLimit: Retry-After seconds / HTTP-date / negative / missing ---

func TestParseRateLimit(t *testing.T) {
	cfg := &Config{Providers: map[string]Provider{"a": {OpenAIBaseURL: "http://x", Provider: "static"}}}
	p := NewProxy(cfg)
	sched := Scheduling{RateLimitBackoff: "60s"}
	now := time.Now()

	// Seconds.
	r := &http.Response{Header: http.Header{"Retry-After": []string{"120"}}}
	got := p.parseRateLimit(r, now, sched)
	if d := got.Sub(now); d < 119*time.Second || d > 121*time.Second {
		t.Errorf("seconds Retry-After: got %v want ~120s", d)
	}
	// Negative seconds clamped to 0.
	r = &http.Response{Header: http.Header{"Retry-After": []string{"-5"}}}
	got = p.parseRateLimit(r, now, sched)
	if got.Before(now) {
		t.Errorf("negative Retry-After: got %v before now", got)
	}
	// HTTP-date.
	future := now.Add(2 * time.Hour)
	r = &http.Response{Header: http.Header{"Retry-After": []string{future.UTC().Format(http.TimeFormat)}}}
	got = p.parseRateLimit(r, now, sched)
	if got.Unix() != future.Unix() {
		t.Errorf("HTTP-date Retry-After: got %v want %v", got, future)
	}
	// HTTP-date in the past → clamped to now.
	past := now.Add(-1 * time.Hour)
	r = &http.Response{Header: http.Header{"Retry-After": []string{past.UTC().Format(http.TimeFormat)}}}
	got = p.parseRateLimit(r, now, sched)
	if got.Before(now) {
		t.Errorf("past HTTP-date: got %v before now", got)
	}
	// Missing Retry-After → default backoff.
	r = &http.Response{Header: http.Header{}}
	got = p.parseRateLimit(r, now, sched)
	if d := got.Sub(now); d != 60*time.Second {
		t.Errorf("missing Retry-After: got %v want 60s", d)
	}
	// Garbage Retry-After → default backoff.
	r = &http.Response{Header: http.Header{"Retry-After": []string{"not-a-number-or-date"}}}
	got = p.parseRateLimit(r, now, sched)
	if d := got.Sub(now); d != 60*time.Second {
		t.Errorf("garbage Retry-After: got %v want 60s", d)
	}
}

// --- rewriteModel: non-JSON body returned unchanged ---

func TestRewriteModel_NonJSON(t *testing.T) {
	orig := []byte(`not-json`)
	if got := rewriteModel(orig, "new"); string(got) != string(orig) {
		t.Errorf("rewriteModel(non-JSON) changed body: %q", got)
	}
}

func TestRewriteModel_SameModel(t *testing.T) {
	// Rewrite to the same model: still marshals (no early return in impl), but
	// the model field should be the new value.
	got := rewriteModel([]byte(`{"model":"old","input":[]}`), "new")
	if !contains(string(got), `"model":"new"`) {
		t.Errorf("rewriteModel did not set new model: %s", got)
	}
}

// --- parseHHMM edge cases ---

func TestParseHHMM(t *testing.T) {
	for _, tc := range []struct {
		in string
		ok bool
	}{
		{"00:00", true},
		{"23:59", true},
		{"24:00", false}, // hour out of range
		{"12:60", false}, // minute out of range
		{"12", false},    // missing minute
		{"", false},
		{"ab:cd", false},
	} {
		_, ok := parseHHMM(tc.in)
		if ok != tc.ok {
			t.Errorf("parseHHMM(%q) ok=%v want %v", tc.in, ok, tc.ok)
		}
	}
}

// --- parseHHMMRange ---

func TestParseHHMMRange(t *testing.T) {
	_, _, ok := parseHHMMRange("09:00-18:00")
	if !ok {
		t.Error("parseHHMMRange(09:00-18:00) want ok")
	}
	_, _, ok = parseHHMMRange("bad")
	if ok {
		t.Error("parseHHMMRange(bad) want not ok")
	}
	_, _, ok = parseHHMMRange("09:00-18:00-20:00")
	if ok {
		t.Error("parseHHMMRange(3 parts) want not ok")
	}
}

// --- billingClassName all tiers ---

func TestBillingClassName(t *testing.T) {
	if got := billingClassName(provider.BillingPlan); got != "plan" {
		t.Errorf("plan=%q", got)
	}
	if got := billingClassName(provider.BillingPayG); got != "pay-as-you-go" {
		t.Errorf("payg=%q", got)
	}
	if got := billingClassName(provider.BillingUnknown); got != "unknown" {
		t.Errorf("unknown=%q", got)
	}
}

// --- extractModel: missing / malformed ---

func TestExtractModel(t *testing.T) {
	if got := extractModel([]byte(`{}`)); got != "" {
		t.Errorf("extractModel({})=%q want empty", got)
	}
	if got := extractModel([]byte(`not-json`)); got != "" {
		t.Errorf("extractModel(bad)=%q want empty", got)
	}
	if got := extractModel([]byte(`{"model":"glm-5.2"}`)); got != "glm-5.2" {
		t.Errorf("extractModel=%q want glm-5.2", got)
	}
}

// TestScheduleStatus_PoolGrouping verifies the /debug/schedule JSON makes the
// credential pool visible: each virtual in `ordered` carries a pool_parent
// marker, and the route carries a `pools` summary (parent + total accounts).
// Backward-compatible: existing fields (provider/priority/tier/surplus/...)
// remain unchanged.
func TestScheduleStatus_PoolGrouping(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	writePoolFile(t, "zhipu", "zhipu", "K1", "K2", "K3")
	cfg := &Config{
		Listen: "127.0.0.1:0",
		Providers: map[string]Provider{
			"zhipu": {OpenAIBaseURL: "http://x", Provider: "zhipu"},
		},
		Routes: map[string][]RouteTarget{
			"glm-5.2": {{Provider: "zhipu", Model: "glm-5.2", Priority: 1}},
		},
	}
	p := NewProxy(cfg)
	data := p.scheduleStatus()

	var st struct {
		Models map[string]struct {
			Ordered []struct {
				Provider   string `json:"provider"`
				PoolParent string `json:"pool_parent"`
				Priority   int    `json:"priority"`
				Tier       string `json:"tier"`
				Available  bool   `json:"available"`
			} `json:"ordered"`
			Pools []struct {
				Parent   string `json:"parent"`
				Accounts int    `json:"accounts"`
			} `json:"pools"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("parse: %v body=%s", err, string(data))
	}
	ri, ok := st.Models["glm-5.2"]
	if !ok {
		t.Fatalf("glm-5.2 missing from status; body=%s", string(data))
	}
	if len(ri.Ordered) != 3 {
		t.Fatalf("want 3 ordered virtuals, got %d (body=%s)", len(ri.Ordered), string(data))
	}
	for _, o := range ri.Ordered {
		if o.PoolParent != "zhipu" {
			t.Errorf("virtual %q: pool_parent=%q want zhipu", o.Provider, o.PoolParent)
		}
		if !strings.HasPrefix(o.Provider, "zhipu#") {
			t.Errorf("provider %q is not a virtual id", o.Provider)
		}
	}
	if len(ri.Pools) != 1 || ri.Pools[0].Parent != "zhipu" || ri.Pools[0].Accounts != 3 {
		t.Errorf("pools = %+v, want one pool {zhipu, 3 accounts}", ri.Pools)
	}
}

// TestScheduleStatus_NoPoolWhenSingle verifies pool_parent + pools are OMITTED
// for routes whose targets are not pooled (backward-compat: no spurious fields
// for non-pooled providers).
func TestScheduleStatus_NoPoolWhenSingle(t *testing.T) {
	p := newQuotaProxy(t,
		map[string]Provider{"a": {}},
		map[string][]RouteTarget{"m": {{Provider: "a", Priority: 1}}})
	staticSurplus(p, "a", 0.5, 0.5)
	var st struct {
		Models map[string]struct {
			Ordered []struct {
				Provider   string `json:"provider"`
				PoolParent string `json:"pool_parent"`
			} `json:"ordered"`
			Pools []struct {
				Parent string `json:"parent"`
			} `json:"pools"`
		} `json:"models"`
	}
	if err := json.Unmarshal(p.scheduleStatus(), &st); err != nil {
		t.Fatal(err)
	}
	m := st.Models["m"]
	if len(m.Ordered) != 1 || m.Ordered[0].Provider != "a" {
		t.Fatalf("ordered=%+v want [a]", m.Ordered)
	}
	if m.Ordered[0].PoolParent != "" {
		t.Errorf("non-pooled provider has pool_parent=%q want empty", m.Ordered[0].PoolParent)
	}
	if len(m.Pools) != 0 {
		t.Errorf("non-pooled route has pools=%+v want empty", m.Pools)
	}
}
