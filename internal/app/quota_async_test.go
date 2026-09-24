package app

import (
	"context"
	"encoding/json"
	"io"
	configdomain "model-proxy/internal/config"
	observestats "model-proxy/internal/observe/stats"
	"model-proxy/internal/provider"
	runtimestate "model-proxy/internal/runtime"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---- quota_async_test.go ----

// blockQuotaProv embeds testProv but blocks in Quota until release is closed,
// and records that Quota was entered. Used to prove pollAsync/refreshAsync are
// tracked by the poller WaitGroup (stop waits) and stop-aware (no-op after stop).
type blockQuotaProv struct {
	testProv
	release   chan struct{}
	entered   chan struct{}
	enterOnce sync.Once
	ran       atomic.Bool
	done      atomic.Bool
}

// TestQuotaLaunch_StopWaitsForAdmittedTask establishes the lifecycle contract
// without scheduler timing: launch returns only after the task is admitted and
// counted; once the callback has started, stop must remain blocked until it is
// released and completes.
func TestQuotaLaunch_StopWaitsForAdmittedTask(t *testing.T) {
	tr := newBlockTracker(t, &blockQuotaProv{testProv: testProv{key: "x"}, release: make(chan struct{})})
	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	if admitted := tr.Launch(func() {
		close(started)
		<-release
		close(finished)
	}); !admitted {
		t.Fatal("task was rejected before stop")
	}
	<-started

	stopped := make(chan struct{})
	go func() { tr.Stop(); close(stopped) }()
	waitForTrackerAdmissionClosed(t, tr)
	select {
	case <-stopped:
		t.Fatal("stop returned while an admitted task was blocked")
	default:
	}
	close(release)
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("admitted task did not finish after release")
	}
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("stop did not return after the admitted task finished")
	}
}

func TestQuotaLaunch_RejectsAfterStop(t *testing.T) {
	tr := newBlockTracker(t, &blockQuotaProv{testProv: testProv{key: "x"}, release: make(chan struct{})})
	tr.Stop()
	called := false
	if admitted := tr.Launch(func() { called = true }); admitted {
		t.Fatal("task admitted after stop")
	}
	if called {
		t.Fatal("callback ran after stop")
	}
}

// TestAsyncDispatch_ConcurrentStop supplements the deterministic launch tests
// with repeated contention at the public async-dispatch boundary.
func TestAsyncDispatch_ConcurrentStop(t *testing.T) {
	for i := 0; i < 200; i++ {
		release := make(chan struct{})
		prov := &blockQuotaProv{testProv: testProv{key: "x"}, release: release}
		tr := newBlockTracker(t, prov)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); tr.PollAsync(time.Now()) }()
		go func() { defer wg.Done(); tr.Stop() }()
		close(release)
		wg.Wait()
		if prov.ran.Load() && !prov.done.Load() {
			t.Fatal("stop returned before an admitted callback completed")
		}
	}
}

// TestPollAll_DiscardsStaleGeneration proves a slow quota response from the old
// config cannot overwrite the new generation after reload.
func TestPollAll_DiscardsStaleGeneration(t *testing.T) {
	var generation atomic.Uint64
	generation.Store(1)
	release := make(chan struct{})
	prov := &blockQuotaProv{testProv: testProv{key: "x"}, release: release}
	tr := newBlockTracker(t, prov)
	tr.Generation = generation.Load
	done := make(chan struct{})
	go func() {
		tr.PollAllGeneration(time.Now(), 1)
		close(done)
	}()
	pollFor(t, prov.ran.Load, time.Second, "old-generation quota poll did not start")
	generation.Store(2)
	tr.Runtime().ReplaceGeneration(2)
	tr.ClearForGeneration(2)
	sentinel := &provider.QuotaSnapshot{Billing: provider.BillingPlan, RemainingPct: 0.75}
	if !tr.CommitSnapshot(2, "x", sentinel) {
		t.Fatal("current-generation sentinel was rejected")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("old-generation quota poll did not finish after release")
	}
	if got := tr.Snapshot("x"); got == nil || got.Billing != sentinel.Billing || got.RemainingPct != sentinel.RemainingPct {
		t.Fatalf("stale generation replaced current snapshot: got=%+v want=%+v", got, sentinel)
	}
}

func (b *blockQuotaProv) Quota() (*provider.QuotaSnapshot, error) {
	b.ran.Store(true)
	b.enterOnce.Do(func() { close(b.entered) })
	<-b.release
	b.done.Store(true)
	return &provider.QuotaSnapshot{Billing: provider.BillingUnknown}, nil
}

func newBlockTracker(t *testing.T, prov *blockQuotaProv) *runtimestate.QuotaTracker {
	t.Helper()
	if prov.entered == nil {
		prov.entered = make(chan struct{})
	}
	tr := newStandaloneQuotaTracker(t.TempDir()+"/q.json",
		func() *configdomain.Config { return &configdomain.Config{} },
		func() map[string]provider.Provider { return map[string]provider.Provider{"x": prov} })
	t.Cleanup(tr.Stop)
	return tr
}

// pollFor waits up to timeout for cond, failing the test if it never holds.
func pollFor(t *testing.T, cond func() bool, timeout time.Duration, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal(msg)
}

func waitForTrackerAdmissionClosed(t *testing.T, tr *runtimestate.QuotaTracker) {
	t.Helper()
	pollFor(t, func() bool {
		return !tr.AdmissionOpen()
	}, time.Second, "stop did not close task admission")
}

// TestPollAsync_TrackedByStop: pollAsync's goroutine is tracked by the poller
// WaitGroup, so stop() must WAIT for an in-flight poll rather than racing it —
// otherwise Close()'s final persist wouldn't be the last write (the P1a bug).
func TestPollAsync_TrackedByStop(t *testing.T) {
	prov := &blockQuotaProv{testProv: testProv{key: "x"}, release: make(chan struct{})}
	tr := newBlockTracker(t, prov)
	tr.PollAsync(time.Now())
	pollFor(t, prov.ran.Load, time.Second, "pollAsync did not enter the provider's Quota")

	stopped := make(chan struct{})
	go func() { tr.Stop(); close(stopped) }()
	waitForTrackerAdmissionClosed(t, tr)
	select {
	case <-stopped:
		t.Fatal("stop() returned while pollAsync is still in-flight (not tracked by poller)")
	default:
	}
	close(prov.release)
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("stop() did not return after the in-flight poll completed")
	}
}

// TestPollAsync_NoopAfterStop: a poll dispatched after stop is a no-op — it must
// NOT run pollAll (which would persist after Close's final flush).
func TestPollAsync_NoopAfterStop(t *testing.T) {
	prov := &blockQuotaProv{testProv: testProv{key: "x"}, release: make(chan struct{})}
	tr := newBlockTracker(t, prov)
	tr.Stop()
	tr.PollAsync(time.Now())
	if prov.ran.Load() {
		t.Errorf("pollAsync dispatched a poll after stop; should be a no-op")
	}
}

// TestRefreshAsync_TrackedByStop: the 429 refreshAsync path is tracked too.
func TestRefreshAsync_TrackedByStop(t *testing.T) {
	prov := &blockQuotaProv{testProv: testProv{key: "x"}, release: make(chan struct{})}
	tr := newBlockTracker(t, prov)
	tr.RefreshAsync("x")
	select {
	case <-prov.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("refreshAsync did not start")
	}
	stopped := make(chan struct{})
	go func() { tr.Stop(); close(stopped) }()
	waitForTrackerAdmissionClosed(t, tr)
	select {
	case <-stopped:
		t.Fatal("stop() returned while refreshAsync is still in-flight (not tracked)")
	default:
	}
	close(prov.release)
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("stop() did not return after the in-flight refresh completed")
	}
}

// TestRefreshAsync_NoopAfterStop: a 429 refresh dispatched after stop is a no-op.
func TestRefreshAsync_NoopAfterStop(t *testing.T) {
	prov := &blockQuotaProv{testProv: testProv{key: "x"}, release: make(chan struct{})}
	tr := newBlockTracker(t, prov)
	tr.Stop()
	tr.RefreshAsync("x")
	if prov.ran.Load() {
		t.Errorf("refreshAsync dispatched after stop; should be a no-op")
	}
}

// ---- quota_exhausted_terminal_test.go ----

// postForStatus posts a JSON body and returns the response (unclosed body is
// drained by the caller-side helper contract used here).
func postForStatus(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp
}

// TestForwardAllTargetsQuotaExhaustedReturns429: when every target of a route
// is skipped as quota-exhausted (fresh plan snapshot, ultimate window at 0),
// the terminal response must be 429 with a Retry-After — the failure IS a
// quota limit, and clients (Claude Code etc.) back off on exactly that signal.
// A bare 502 makes them retry immediately into the same wall.
//
// Regression for the skip-quota-exhausted scheduling change: the skip filter
// removes exhausted targets from scheduling, but the failure classification
// only consulted health — exhausted providers never receive a request, never
// earn a 429 health entry, and read as "available", so the terminal fell back
// to 502 with no Retry-After (plus two idle rescheduling rounds).
func TestForwardAllTargetsQuotaExhaustedReturns429(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"p1": {OpenAIBaseURL: upstream.URL, Provider: testProviderID},
			"p2": {OpenAIBaseURL: upstream.URL, Provider: testProviderID},
		},
		Routes: map[string][]configdomain.RouteTarget{"m": {
			{Provider: "p1", Model: "m", Priority: 1},
			{Provider: "p2", Model: "m", Priority: 2},
		}},
	}
	p := newTestProxy(t, cfg)

	// Fresh plan snapshots with a fully-exhausted ultimate window (reset in two
	// hours). AsOf=now keeps them inside the 3×poll-interval freshness window.
	now := time.Now()
	for _, name := range []string{"p1", "p2"} {
		p.runtimeState.SetQuota(name, &provider.QuotaSnapshot{
			Billing: provider.BillingPlan,
			AsOf:    now,
			Windows: []provider.QuotaWindow{{
				Label: "Weekly tokens", Ultimate: true, RemainingPct: 0,
				ResetsAt: now.Add(2 * time.Hour),
			}},
		}, p.configGeneration.Load())
	}

	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()
	resp := postForStatus(t, px.URL+"/v1/chat/completions", `{"model":"m","messages":[]}`)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("all-targets-exhausted status = %d, want 429", resp.StatusCode)
	}
	if ra := resp.Header.Get("Retry-After"); ra == "" {
		t.Fatal("all-targets-exhausted response has no Retry-After header")
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("upstream hits = %d, want 0 (exhausted targets must be skipped)", got)
	}
}

// TestForwardMixedExhaustionPrefersLiveTarget: with one exhausted and one
// healthy target, the healthy one serves the request (the skip must not take
// the whole route down).
func TestForwardMixedExhaustionPrefersLiveTarget(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"plan": {OpenAIBaseURL: upstream.URL, Provider: testProviderID},
			"live": {OpenAIBaseURL: upstream.URL, Provider: testProviderID},
		},
		Routes: map[string][]configdomain.RouteTarget{"m": {
			{Provider: "plan", Model: "m", Priority: 1},
			{Provider: "live", Model: "m", Priority: 2},
		}},
	}
	p := newTestProxy(t, cfg)
	now := time.Now()
	p.runtimeState.SetQuota("plan", &provider.QuotaSnapshot{
		Billing: provider.BillingPlan,
		AsOf:    now,
		Windows: []provider.QuotaWindow{{
			Label: "Weekly tokens", Ultimate: true, RemainingPct: 0,
			ResetsAt: now.Add(2 * time.Hour),
		}},
	}, p.configGeneration.Load())

	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()
	resp := postForStatus(t, px.URL+"/v1/chat/completions", `{"model":"m","messages":[]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mixed-exhaustion status = %d, want 200 via the live target", resp.StatusCode)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("upstream hits = %d, want 1", got)
	}
}

// ---- proxy_quota_test.go ----

// TestProxy_QuotaRefreshOnRateLimit: a 429 on a provider triggers an async
// quota refresh of that provider.
func TestProxy_QuotaRefreshOnRateLimit(t *testing.T) {
	primary, _ := newHitServer(func(int) (int, string, http.Header, time.Duration) {
		return 429, `{}`, intHdr("Retry-After", "30"), 0
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
		Routes: map[string][]configdomain.RouteTarget{"m1": {
			{Provider: "primary", Model: "m1", Priority: 1},
			{Provider: "fallback", Model: "m1", Priority: 2},
		}},
		Scheduling: schedCfg(3, "50ms", "10s", "5s", "0s"),
	}
	p := newTestProxy(t, cfg)
	refreshed := make(chan string, 2)
	p.providers["primary"] = &quotaCountProv{name: "primary", refreshed: refreshed}
	p.providers["fallback"] = &quotaCountProv{name: "fallback", refreshed: refreshed}
	p.quota.Stop()
	p.quota = runtimestate.NewQuotaTracker(
		"",
		func() *configdomain.Config { return cfg },
		func() map[string]provider.Provider { return p.providers },
		&p.runtimeState,
	)
	p.quota.Generation = p.configGeneration.Load
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()
	postOK(t, px.URL+"/v1/chat/completions", `{"model":"m1","messages":[]}`)
	select {
	case name := <-refreshed:
		if name != "primary" {
			t.Errorf("refreshed provider=%q, want primary (the one that 429'd)", name)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("quota refresh did not run after 429")
	}
	p.quota.Stop() // drain every admitted refresh before asserting exact cardinality
	select {
	case name := <-refreshed:
		t.Fatalf("unexpected duplicate quota refresh for %q", name)
	default:
	}
}

// quotaCountProv is a controllable Provider whose real Quota call records its
// own identity. This proves the 429 path refreshes the provider that failed,
// rather than merely proving an arbitrary refresh happened.
type quotaCountProv struct {
	testProv
	name      string
	refreshed chan<- string
	calls     atomic.Int32
}

func (q *quotaCountProv) Quota() (*provider.QuotaSnapshot, error) {
	q.calls.Add(1)
	if q.refreshed != nil {
		q.refreshed <- q.name
	}
	return &provider.QuotaSnapshot{Billing: provider.BillingPlan, RemainingPct: 0.5}, nil
}

// staticSurplus sets a plan snapshot whose single ultimate window has the given
// remaining and time-left fraction (reset = now + fLeft×7d). Its surplus is
// remaining − fLeft. No short window → no peak-burn deduction.
func staticSurplus(p *Proxy, name string, remaining, fLeft float64) {
	now := time.Now()
	const dur = 7 * 24 * time.Hour
	p.quota.SetSnapshot(name, &provider.QuotaSnapshot{
		Billing:      provider.BillingPlan,
		RemainingPct: remaining,
		Windows: []provider.QuotaWindow{{
			Ultimate: true, Kind: "tokens", RemainingPct: remaining, Total: 200,
			Duration: dur, ResetsAt: now.Add(time.Duration(fLeft * float64(dur))),
		}},
		AsOf: now,
	})
}

// newQuotaProxy builds a Proxy wired with a hand-built runtimestate.QuotaTracker (no polling),
// for schedule() unit tests. Providers are backed by testProv. It constructs the
// Proxy directly (not via NewProxy) so no real poller goroutine starts — which
// avoids a data race between that goroutine reading cfg.Scheduling and these
// tests mutating it (e.g. StickyDwell) after construction.
func newQuotaProxy(t *testing.T, provs map[string]configdomain.Provider, routes map[string][]configdomain.RouteTarget) *Proxy {
	t.Helper()
	cfg := &configdomain.Config{
		Providers:  provs,
		Routes:     routes,
		Scheduling: configdomain.Scheduling{QuotaSwitchMargin: 15},
	}
	p := &Proxy{
		generationState: generationState{
			cfg:       cfg,
			providers: map[string]provider.Provider{},
			poolIndex: map[string][]string{},
			parentOf:  map[string]string{},
		},
		processServices: processServices{
			client: &http.Client{Timeout: 0},
		},
	}
	p.quota = runtimestate.NewQuotaTracker(
		"",
		func() *configdomain.Config { return cfg },
		func() map[string]provider.Provider { return p.providers },
		&p.runtimeState,
	)
	p.quota.Generation = p.configGeneration.Load
	t.Cleanup(p.Close)
	// Inject the impls BEFORE building the route table: expandTarget only
	// lists runnable providers, so a table built against the empty impl map
	// would drop every target (the construct-then-inject seam mirrors
	// login → reload, where credentials exist before the table rebuild).
	for name := range provs {
		p.providers[name] = &testProv{key: name}
	}
	p.expandedRoutes = p.buildExpandedRoutes(authNotReady(p.providers))
	return p
}

// firstProvider returns the provider the scheduler tries first for a model.
func firstProvider(p *Proxy, model string) string {
	routeKeys := map[string]bool{}
	for k := range p.cfg.Routes {
		routeKeys[k] = true
	}
	ordered := p.schedule(p.cfg, p.parentOf, model, "", p.cfg.Routes[model], routeKeys)
	if len(ordered) == 0 {
		return ""
	}
	return ordered[0].Provider
}

// TestScheduleAdapter_AppliesPeakMultiplier verifies the root adapter projects
// Provider.PeakHours into runtime.Target.PeakMultiplier. The underlying surplus
// state machine is covered in internal/runtime.
//
// A provider in peak (multiplier 2) with a
// short rate-cap window has its surplus reduced by the peak-burn deduction, so a
// non-peak peer (same ultimate remaining + time-left) ranks ahead.
func TestScheduleAdapter_AppliesPeakMultiplier(t *testing.T) {
	p := newQuotaProxy(t,
		map[string]configdomain.Provider{
			"plain": {},
			"peak":  {PeakHours: configdomain.PeakConfig{{Window: "00:00-23:59", Multiplier: 2}}},
		},
		// peak listed first: a lost peak-burn deduction leaves both surpluses at
		// 0, and sort.SliceStable would keep config order — the expected winner
		// must not be first.
		map[string][]configdomain.RouteTarget{"m": {{Provider: "peak"}, {Provider: "plain"}}})
	now := time.Now()
	const dur = 7 * 24 * time.Hour
	ult := provider.QuotaWindow{Ultimate: true, Kind: "tokens", RemainingPct: 0.5, Total: 200,
		Duration: dur, ResetsAt: now.Add(dur / 2)} // fLeft 0.5
	// plain: ultimate only → surplus 0.5 − 0.5 = 0.
	p.quota.SetSnapshot("plain", &provider.QuotaSnapshot{Billing: provider.BillingPlan, RemainingPct: 0.5,
		Windows: []provider.QuotaWindow{ult}, AsOf: now})
	// peak: ultimate + short (rem 0.6, total 100 → share 0.5); peak mult 2 →
	// remaining = 0.5 − 0.6×0.5×1 = 0.2 → surplus 0.2 − 0.5 = −0.3.
	p.quota.SetSnapshot("peak", &provider.QuotaSnapshot{Billing: provider.BillingPlan, RemainingPct: 0.5,
		Windows: []provider.QuotaWindow{ult, {Short: true, Kind: "tokens", RemainingPct: 0.6, Total: 100}}, AsOf: now})
	if got := firstProvider(p, "m"); got != "plain" {
		t.Errorf("first=%q, want plain (peak provider's surplus reduced by short-window burn)", got)
	}
}

// TestScheduleAdapter_AppliesQualityWeights verifies the root adapter projects
// Scheduling quality weights into the runtime decision: with the default
// weights a below-circuit-threshold failure burst sinks the provider; with the
// error weight explicitly disabled the same failures leave the order intact.
func TestScheduleAdapter_AppliesQualityWeights(t *testing.T) {
	build := func(disableWeight bool) *Proxy {
		p := newQuotaProxy(t,
			map[string]configdomain.Provider{"flaky": {}, "steady": {}},
			map[string][]configdomain.RouteTarget{"m": {{Provider: "flaky"}, {Provider: "steady"}}})
		if disableWeight {
			zero := 0
			p.cfg.Scheduling.QualityErrorWeight = &zero
		}
		now := time.Now()
		quota := &provider.QuotaSnapshot{Billing: provider.BillingPlan, RemainingPct: 0.5, AsOf: now}
		p.quota.SetSnapshot("flaky", quota)
		p.quota.SetSnapshot("steady", quota)
		// Two failures: below the circuit threshold (3), so the provider stays
		// available — only the quality penalty can reorder.
		p.runtimeState.RecordFailure("flaky", 3, time.Minute, 0)
		p.runtimeState.RecordFailure("flaky", 3, time.Minute, 0)
		return p
	}

	// Two fresh proxies: schedule() commits sticky to the first winner, which
	// would otherwise carry into the second phase.
	if got := firstProvider(build(false), "m"); got != "steady" {
		t.Errorf("default weights: first=%q, want steady (flaky penalized)", got)
	}
	if got := firstProvider(build(true), "m"); got != "flaky" {
		t.Errorf("disabled weight: first=%q, want flaky (config order kept)", got)
	}
}

// The parent's config `billing:` label is payment-method METADATA: it must not
// be projected into the scheduler (the old BillingOverride did exactly that,
// demoting this measured-plan pool virtual to a strict last resort). The tier
// comes from the virtual's own measured snapshot, so priority 1 beats 9.
func TestScheduleAdapter_ConfigBillingLabelDoesNotReachScheduler(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	targets := []configdomain.RouteTarget{
		{Provider: "pool#acct", Model: "pool-model", Priority: 1},
		{Provider: "plan", Model: "plan-model", Priority: 9},
	}
	p := newQuotaProxy(t,
		map[string]configdomain.Provider{
			"pool": {Billing: "pay-as-you-go"},
			"plan": {},
		},
		map[string][]configdomain.RouteTarget{"m": targets})
	p.parentOf = map[string]string{"pool#acct": "pool"}
	p.quota.SetSnapshot("pool#acct", &provider.QuotaSnapshot{
		Billing: provider.BillingPlan,
		AsOf:    now,
	})
	p.quota.SetSnapshot("plan", &provider.QuotaSnapshot{
		Billing: provider.BillingPlan,
		AsOf:    now,
	})
	if got := p.quota.Snapshot("pool#acct"); got == nil || got.Billing != provider.BillingPlan {
		t.Fatalf("virtual quota=%+v, want fresh plan snapshot", got)
	}
	if got := p.quota.Snapshot("plan"); got == nil || got.Billing != provider.BillingPlan {
		t.Fatalf("plan quota=%+v, want fresh plan snapshot", got)
	}

	ordered, _ := p.decideOrder(
		p.cfg,
		p.parentOf,
		"m",
		"",
		targets,
		now,
		false,
		map[string]bool{"m": true},
	)
	if len(ordered) != 2 {
		t.Fatalf("ordered=%+v, want two targets", ordered)
	}
	if ordered[0].Provider != "pool#acct" || ordered[0].Model != "pool-model" ||
		ordered[1].Provider != "plan" || ordered[1].Model != "plan-model" {
		t.Fatalf("ordered=%+v, want measured-plan pool virtual (p1) before plan (p9)", ordered)
	}
}

func TestScheduleAdapter_DerivesQuotaMaxAgeFromPollInterval(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	targets := []configdomain.RouteTarget{
		{Provider: "unknown", Model: "unknown-model", Priority: 1},
		{Provider: "candidate", Model: "candidate-model", Priority: 1},
	}
	p := newQuotaProxy(t,
		map[string]configdomain.Provider{"unknown": {}, "candidate": {}},
		map[string][]configdomain.RouteTarget{"m": targets})
	p.quota.SetSnapshot("candidate", &provider.QuotaSnapshot{
		Billing: provider.BillingPlan,
		AsOf:    now.Add(-5 * time.Minute),
	})

	p.cfg.Scheduling.QuotaPollInterval = "2m" // max age = 3 × 2m = 6m
	fresh, _ := p.decideOrder(
		p.cfg, p.parentOf, "m", "", targets, now, false, map[string]bool{"m": true},
	)
	if len(fresh) != 2 || fresh[0].Provider != "candidate" {
		t.Fatalf("6m max age order=%+v, want fresh plan candidate first", fresh)
	}

	p.cfg.Scheduling.QuotaPollInterval = "1m" // max age = 3m; snapshot is stale
	stale, _ := p.decideOrder(
		p.cfg, p.parentOf, "m", "", targets, now, false, map[string]bool{"m": true},
	)
	if len(stale) != 2 || stale[0].Provider != "unknown" {
		t.Fatalf("3m max age order=%+v, want stable unknown-first order", stale)
	}
}

// TestScheduleStatus: the /debug/schedule payload reports the first-choice
// provider + ordered list with tiers, read-only (no sticky mutation).
func TestScheduleStatus(t *testing.T) {
	p := newQuotaProxy(t,
		map[string]configdomain.Provider{"a": {}, "b": {}},
		map[string][]configdomain.RouteTarget{"m": {{Provider: "a", Priority: 1}, {Provider: "b", Priority: 2}}})
	staticSurplus(p, "a", 0.5, 0)   // surplus +0.5 (waste risk)
	staticSurplus(p, "b", 0.5, 0.5) // surplus 0
	var st struct {
		Models map[string]struct {
			First   string `json:"first"`
			Ordered []struct {
				Provider string `json:"provider"`
				Tier     string `json:"tier"`
			} `json:"ordered"`
		} `json:"models"`
	}
	if err := json.Unmarshal(p.scheduleStatus(), &st); err != nil {
		t.Fatal(err)
	}
	m, ok := st.Models["m"]
	if !ok {
		t.Fatal("no model m in status")
	}
	if m.First != "a" {
		t.Errorf("first=%q, want a (higher surplus)", m.First)
	}
	if len(m.Ordered) != 2 || m.Ordered[0].Provider != "a" || m.Ordered[1].Provider != "b" {
		t.Errorf("ordered=%+v, want [a, b]", m.Ordered)
	}
	if m.Ordered[0].Tier != "plan" {
		t.Errorf("tier=%q, want plan", m.Ordered[0].Tier)
	}
}

// ---- budget_watch_test.go ----

// budgetAlertDetail mirrors the budget watcher's wire payload for assertions
// without reaching into the observe/budget package internals.
type budgetAlertDetail struct {
	Scope        string  `json:"scope"`
	Month        string  `json:"month"`
	ThresholdUSD float64 `json:"threshold_usd"`
	ActualUSD    float64 `json:"actual_usd"`
}

// newBudgetTestProxy builds a Proxy with an isolated temp stats store and
// offline pricing (catalog disabled; prices: overrides still apply).
func newBudgetTestProxy(t *testing.T, budgets configdomain.BudgetsConfig) *Proxy {
	t.Helper()
	cfg := &configdomain.Config{
		Budgets: budgets,
		Pricing: configdomain.PricingConfig{Enabled: false},
		Prices: map[string]configdomain.PriceConfig{
			"glm-5.2": {Input: 1.0, Output: 2.0}, // USD per 1M tokens
		},
	}
	p := newTestProxy(t, cfg)
	p.stats = newTestStatsStore(t)
	return p
}

// flushBudgetUsage persists one minute bucket in the current local month:
// 1M input + 0.5M output on glm-5.2 = 1×$1.0 + 0.5×$2.0 = $2.00 equivalent
// cost.
func flushBudgetUsage(t *testing.T, p *Proxy, provider string) {
	t.Helper()
	now := time.Now()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.Local)
	err := p.stats.FlushContext(context.Background(), monthStart.Unix(), map[observestats.Key]observestats.Counters{
		{Provider: provider, Model: "glm-5.2"}: {
			Requests: 1, Input: 1_000_000, Output: 500_000,
		},
	})
	if err != nil {
		t.Fatalf("flush usage: %v", err)
	}
}

func TestBudgetWatcher_NotStartedWhenUnconfigured(t *testing.T) {
	p := newTestProxy(t, &configdomain.Config{})
	p.startBudgetWatcher()
	if p.budget != nil {
		t.Fatal("budget watcher started without any configured threshold")
	}
}

func TestBudgetWatcher_StartedWhenConfigured(t *testing.T) {
	// stats is nil here: the loop's startup check must tolerate it and simply
	// wait for the next tick (Close stops and waits the loop goroutine).
	p := newTestProxy(t, &configdomain.Config{Budgets: configdomain.BudgetsConfig{MonthlyUSD: 10}})
	p.startBudgetWatcher()
	if p.budget == nil {
		t.Fatal("budget watcher did not start with a configured threshold")
	}
}

// TestBudgetPorts_BudgetStateReturnsPerCallCopies: the watcher ticks once per
// minute against reload-owned state; the adapter must hand it fresh copies so
// a slow tick can never observe (or mutate) a live config generation's maps.
func TestBudgetPorts_BudgetStateReturnsPerCallCopies(t *testing.T) {
	p := newBudgetTestProxy(t, configdomain.BudgetsConfig{
		MonthlyUSD: 10,
		Providers:  map[string]float64{"zhipu": 5},
	})
	p.parentOf = map[string]string{"zhipu#work": "zhipu"}
	ports := p.budgetPorts()

	budgets, parentOf, ok := ports.BudgetState()
	if !ok {
		t.Fatal("BudgetState ok = false, want true for enabled budgets with stats")
	}
	if budgets.MonthlyUSD != 10 || budgets.Providers["zhipu"] != 5 || parentOf["zhipu#work"] != "zhipu" {
		t.Fatalf("BudgetState = %+v / %v, want configured budgets and parent mapping", budgets, parentOf)
	}

	// Mutating the returned copies must not leak into the next call.
	budgets.Providers["zhipu"] = 999
	budgets.Providers["injected"] = 1
	parentOf["injected"] = "injected"
	again, againParentOf, ok := ports.BudgetState()
	if !ok {
		t.Fatal("second BudgetState ok = false")
	}
	if len(again.Providers) != 1 || again.Providers["zhipu"] != 5 {
		t.Fatalf("providers after caller mutation = %v, want fresh copy {zhipu: 5}", again.Providers)
	}
	if len(againParentOf) != 1 || againParentOf["zhipu#work"] != "zhipu" {
		t.Fatalf("parentOf after caller mutation = %v, want fresh copy {zhipu#work: zhipu}", againParentOf)
	}
}

// TestBudgetPorts_BudgetStateUnavailable: nil stats store or disabled budgets
// must report ok=false so the watcher skips the tick.
func TestBudgetPorts_BudgetStateUnavailable(t *testing.T) {
	p := newTestProxy(t, &configdomain.Config{Budgets: configdomain.BudgetsConfig{MonthlyUSD: 10}})
	if _, _, ok := p.budgetPorts().BudgetState(); ok {
		t.Fatal("BudgetState ok = true with nil stats store, want false")
	}

	p = newTestProxy(t, &configdomain.Config{})
	p.stats = newTestStatsStore(t)
	if _, _, ok := p.budgetPorts().BudgetState(); ok {
		t.Fatal("BudgetState ok = true with budgets disabled, want false")
	}
}

// TestBudgetWatcher_EndToEndAlertThroughPorts: the started watcher's first
// tick reads stats/pricing/config through the port closures and publishes the
// alert on the Proxy's live event hub.
func TestBudgetWatcher_EndToEndAlertThroughPorts(t *testing.T) {
	p := newBudgetTestProxy(t, configdomain.BudgetsConfig{MonthlyUSD: 1.0})
	flushBudgetUsage(t, p, "zhipu")
	p.startBudgetWatcher()
	if p.budget == nil {
		t.Fatal("budget watcher did not start with a configured threshold")
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		for _, event := range p.events.Snapshot() {
			if event.Type != "budget" {
				continue
			}
			var payload budgetAlertDetail
			if err := json.Unmarshal([]byte(event.Detail), &payload); err != nil {
				t.Fatalf("budget event Detail is not the alert payload: %v", err)
			}
			if payload.Scope != "global" || payload.ThresholdUSD != 1.0 || payload.ActualUSD != 2.0 {
				t.Fatalf("payload = %+v, want {global 1 2}", payload)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no budget event published within 3s of watcher start")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
