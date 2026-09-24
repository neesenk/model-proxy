package app

import (
	"encoding/json"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/provider"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ---- schedule_parse_status_test.go ----

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

// TestScheduleStatus_PoolGrouping verifies the /debug/schedule JSON makes the
// credential pool visible: each virtual in `ordered` carries a pool_parent
// marker, and the route carries a `pools` summary (parent + total accounts).
// Backward-compatible: existing fields (provider/priority/tier/surplus/...)
// remain unchanged.
func TestScheduleStatus_PoolGrouping(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	writePoolFile(t, "zhipu", "zhipu", "K1", "K2", "K3")
	cfg := &configdomain.Config{
		Listen: "127.0.0.1:0",
		Providers: map[string]configdomain.Provider{
			"zhipu": {OpenAIBaseURL: "http://x", Provider: "zhipu"},
		},
		Routes: map[string][]configdomain.RouteTarget{
			"glm-5.2": {{Provider: "zhipu", Model: "glm-5.2", Priority: 1}},
		},
	}
	p := newTestProxy(t, cfg)
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
		map[string]configdomain.Provider{"a": {}},
		map[string][]configdomain.RouteTarget{"m": {{Provider: "a", Priority: 1}}})
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

// ---- cooldown_test.go ----

// TestServeOnce_EffectiveTargetsReflectsScheduledSet moved to internal/forward
// (serve_test.go) with the serveOnce pipeline it exercises.

// ---- cooldown_wait_test.go ----

// cooldownCfg builds a two-target route over the given upstreams with the
// wait-retry knobs set for fast tests (retry_wait 10s = real default; short
// backoffs elsewhere).
func cooldownCfg(primary, fallback *httptest.Server, retryWait string) *configdomain.Config {
	return &configdomain.Config{
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
		Scheduling: configdomain.Scheduling{
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

// TestScheduleStatus_BillingDeclaredOnUnmeasured pins the display split: the
// scheduling tier stays MEASURED-only (an unmeasured provider is "unknown"
// regardless of its config `billing:` label), but the declared label rides
// along as billing_declared so the UI can annotate "plan (unmeasured)" /
// "pay-as-you-go (unmeasured)" instead of collapsing every unmeasured
// provider into one indistinguishable label. Omitted when the config
// declares none.
func TestScheduleStatus_BillingDeclaredOnUnmeasured(t *testing.T) {
	p := newQuotaProxy(t,
		map[string]configdomain.Provider{
			"console-plan": {Provider: testProviderID, Billing: "plan"},
			"console-payg": {Provider: testProviderID, Billing: "pay-as-you-go"},
			"undeclared":   {Provider: testProviderID},
		},
		map[string][]configdomain.RouteTarget{
			"m": {
				{Provider: "console-plan", Priority: 1},
				{Provider: "console-payg", Priority: 2},
				{Provider: "undeclared", Priority: 3},
			},
		})
	// No quota snapshots: every tier is unmeasured.
	var st struct {
		Models map[string]struct {
			Ordered []struct {
				Provider        string  `json:"provider"`
				Tier            string  `json:"tier"`
				BillingDeclared string  `json:"billing_declared"`
				Surplus         float64 `json:"surplus"`
			} `json:"ordered"`
		} `json:"models"`
	}
	if err := json.Unmarshal(p.scheduleStatus(), &st); err != nil {
		t.Fatal(err)
	}
	ordered := st.Models["m"].Ordered
	if len(ordered) != 3 {
		t.Fatalf("ordered = %+v, want all three targets", ordered)
	}
	want := map[string]string{
		"console-plan": "plan",
		"console-payg": "pay-as-you-go",
		"undeclared":   "",
	}
	for _, o := range ordered {
		if o.Tier != "unknown" {
			t.Errorf("%s tier = %q, want unknown (measured-only tier must ignore the config label)", o.Provider, o.Tier)
		}
		if o.BillingDeclared != want[o.Provider] {
			t.Errorf("%s billing_declared = %q, want %q", o.Provider, o.BillingDeclared, want[o.Provider])
		}
	}
}

// TestScheduleStatus_BlockedReasonsOnEmptyChain pins the empty-chain
// explanation: when every target of a listed route is unschedulable, the
// projection carries one blocked entry per target with the most-explanatory
// reason and a recovery hint — the real-world case is a plan provider whose
// ultimate quota window measured 0% (e.g. kimi-code's weekly limit), which
// the availability filter skips to avoid a deterministic 429.
func TestScheduleStatus_BlockedReasonsOnEmptyChain(t *testing.T) {
	p := newQuotaProxy(t,
		map[string]configdomain.Provider{"a": {Provider: testProviderID}},
		map[string][]configdomain.RouteTarget{"m": {{Provider: "a", Priority: 1}}})
	// Fresh plan snapshot with the ultimate window fully spent.
	staticSurplus(p, "a", 0, 0)

	var st struct {
		Models map[string]struct {
			First   string `json:"first"`
			Ordered []struct {
				Provider string `json:"provider"`
			} `json:"ordered"`
			Blocked []struct {
				Provider string `json:"provider"`
				Reason   string `json:"reason"`
				Until    string `json:"until"`
			} `json:"blocked"`
		} `json:"models"`
	}
	if err := json.Unmarshal(p.scheduleStatus(), &st); err != nil {
		t.Fatal(err)
	}
	ri, ok := st.Models["m"]
	if !ok {
		t.Fatal("route m missing: an all-blocked route stays listed (not disabled, not impl-less)")
	}
	if len(ri.Ordered) != 0 || ri.First != "" {
		t.Fatalf("ordered = %+v first = %q, want empty chain", ri.Ordered, ri.First)
	}
	if len(ri.Blocked) != 1 {
		t.Fatalf("blocked = %+v, want one entry for target a", ri.Blocked)
	}
	b := ri.Blocked[0]
	if b.Provider != "a" || b.Reason != "quota-exhausted" {
		t.Fatalf("blocked entry = %+v, want a/quota-exhausted", b)
	}
	if b.Until == "" {
		t.Fatal("quota-exhausted carries no recovery hint (until)")
	}
	if _, err := time.Parse(time.RFC3339, b.Until); err != nil {
		t.Fatalf("until %q is not RFC3339: %v", b.Until, err)
	}
}
