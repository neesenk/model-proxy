package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	runtimestate "model-proxy/internal/runtime"
)

// callCounter is the race-clean call tally for the synthetic upstreams
// (server goroutine writes, test goroutine reads).
type callCounter struct{ n atomic.Int32 }

func (c *callCounter) add() int32  { return c.n.Add(1) }
func (c *callCounter) load() int32 { return c.n.Load() }

// Dashboard is the assertion surface for the shared health state.
type Dashboard = runtimestate.DashboardSnapshot

// Behavior of the scheduling seam for headless model calls (guard
// adjudication): scheduled ordering with cooldown skip, per-attempt budget
// slicing, outcome classification recorded into the SHARED health state, and
// the fail-fast all-cooling error. The production incident this pins: a
// hanging first route target used to consume the whole call budget and
// verdicts degraded to error; now it burns only its own slice and the next
// target decides. All upstreams are synthetic (AGENTS.md credential red
// line); hanging handlers release via Cleanup before their server closes.

// hangServer never answers until released (t.Cleanup closes the channel;
// registered after srv.Close so LIFO frees the handler before the close).
func hangServer(t *testing.T) (*httptest.Server, *callCounter) {
	t.Helper()
	release := make(chan struct{})
	calls := &callCounter{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.add()
		io.Copy(io.Discard, r.Body)
		<-release
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	return srv, calls
}

func replyServer(t *testing.T, status int, body string) (*httptest.Server, *callCounter) {
	t.Helper()
	calls := &callCounter{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.add()
		io.Copy(io.Discard, r.Body)
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, calls
}

const exchangeOKReply = `{"content":[{"type":"text","text":"{\"risk\":\"low\",\"reason\":\"fixture\"}"}]}`

// newExchangeProxy wires judge providers onto one route, lowest
// route-target priority first, plus a circuit threshold of 1 so one failure
// opens the circuit (deterministic cooldown assertions).
func newExchangeProxy(t *testing.T, providers map[string]Provider, targets []RouteTarget) (*Proxy, func() Dashboard) {
	t.Helper()
	home := t.TempDir()
	setPoolHome(t, home)
	cfg := &Config{
		Providers:  providers,
		Routes:     map[string][]RouteTarget{"judge": targets},
		Scheduling: Scheduling{CircuitThreshold: 1, CircuitCooldown: "10m"},
	}
	keys := make(map[string]string, len(providers))
	for name, prov := range providers {
		prov.Provider = "test-static"
		keys[name] = "k-" + name
		cfg.Providers[name] = prov
	}
	stateDir := t.TempDir()
	p := newProxyWithStaticAt(t, cfg, filepath.Join(stateDir, "quota_state.json"), keys)
	return p, func() Dashboard { return p.runtimeState.Dashboard(time.Now()) }
}

func exchangeBody() []byte {
	return []byte(`{"max_tokens":16,"messages":[{"role":"user","content":"judge this"}]}`)
}

// A hanging first target burns only its own budget slice, the healthy second
// target decides, and the hang feeds the shared breaker.
func TestScheduledExchange_HangingFirstTargetFailsOverWithinBudget(t *testing.T) {
	hang, hangCalls := hangServer(t)
	good, goodCalls := replyServer(t, 200, exchangeOKReply)
	p, dash := newExchangeProxy(t,
		map[string]Provider{
			"judge-hang": {AnthropicBaseURL: hang.URL},
			"judge-good": {AnthropicBaseURL: good.URL},
		},
		[]RouteTarget{
			{Provider: "judge-hang", Model: "m", Priority: 1},
			{Provider: "judge-good", Model: "m", Priority: 2},
		})
	snap := p.SnapshotRuntime()

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	res, err := scheduledExchange{p: p}.exchange(ctx, snap, "judge", exchangeBody(), nil)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if res.Provider != "judge-good" {
		t.Fatalf("winner = %s, want judge-good", res.Provider)
	}
	if hangCalls.load() != 1 {
		t.Errorf("hanging target attempts = %d, want 1", hangCalls.load())
	}
	if goodCalls.load() != 1 {
		t.Errorf("good target attempts = %d, want 1", goodCalls.load())
	}
	// Attempt 1's slice is half the budget (300ms); the verdict must land well
	// inside the total budget instead of timing out on the hang.
	if elapsed > 500*time.Millisecond {
		t.Errorf("elapsed = %v, want < 500ms (hang must not consume the budget)", elapsed)
	}
	if got := dash().Providers["judge-hang"].ConsecutiveFailures; got != 1 {
		t.Errorf("hang consecutive failures = %d, want 1 (shared breaker fed)", got)
	}
}

// Once the hanging target's circuit is open (threshold 1), the next exchange
// skips it entirely — the cooldown the forward pipeline relies on now also
// protects the judge leg.
func TestScheduledExchange_CooldownSkipsCircuitOpenTarget(t *testing.T) {
	hang, hangCalls := hangServer(t)
	good, goodCalls := replyServer(t, 200, exchangeOKReply)
	p, _ := newExchangeProxy(t,
		map[string]Provider{
			"judge-hang": {AnthropicBaseURL: hang.URL},
			"judge-good": {AnthropicBaseURL: good.URL},
		},
		[]RouteTarget{
			{Provider: "judge-hang", Model: "m", Priority: 1},
			{Provider: "judge-good", Model: "m", Priority: 2},
		})
	snap := p.SnapshotRuntime()
	x := scheduledExchange{p: p}

	// First call: hang fails its slice (circuit opens at threshold 1), good decides.
	ctx1, cancel1 := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel1()
	if _, err := x.exchange(ctx1, snap, "judge", exchangeBody(), nil); err != nil {
		t.Fatalf("first exchange: %v", err)
	}
	if hangCalls.load() != 1 {
		t.Fatalf("hang calls after first = %d, want 1", hangCalls.load())
	}

	// Second call: the open circuit removes hang from the schedule.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel2()
	res, err := x.exchange(ctx2, snap, "judge", exchangeBody(), nil)
	if err != nil {
		t.Fatalf("second exchange: %v", err)
	}
	if res.Provider != "judge-good" {
		t.Fatalf("second winner = %s, want judge-good", res.Provider)
	}
	if hangCalls.load() != 1 {
		t.Errorf("hang calls after second = %d, want still 1 (cooldown skip)", hangCalls.load())
	}
	if goodCalls.load() != 2 {
		t.Errorf("good calls = %d, want 2", goodCalls.load())
	}
}

// A 429 goes through the single rate-limit classification entry and cools
// the provider out of the schedule (probe replies carry no Retry-After
// headers; the transient default applies).
func TestScheduledExchange_RateLimitCoolsProvider(t *testing.T) {
	bad, badCalls := replyServer(t, http.StatusTooManyRequests, `{"error":"rate limited"}`)
	good, goodCalls := replyServer(t, 200, exchangeOKReply)
	p, dash := newExchangeProxy(t,
		map[string]Provider{
			"judge-429":  {AnthropicBaseURL: bad.URL},
			"judge-good": {AnthropicBaseURL: good.URL},
		},
		[]RouteTarget{
			{Provider: "judge-429", Model: "m", Priority: 1},
			{Provider: "judge-good", Model: "m", Priority: 2},
		})
	snap := p.SnapshotRuntime()
	x := scheduledExchange{p: p}

	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
		res, err := x.exchange(ctx, snap, "judge", exchangeBody(), nil)
		cancel()
		if err != nil {
			t.Fatalf("exchange %d: %v", i, err)
		}
		if res.Provider != "judge-good" {
			t.Fatalf("exchange %d winner = %s, want judge-good", i, res.Provider)
		}
	}
	if badCalls.load() != 1 {
		t.Errorf("429 target calls = %d, want 1 (rate-limit cooldown skips it afterwards)", badCalls.load())
	}
	if goodCalls.load() != 2 {
		t.Errorf("good calls = %d, want 2", goodCalls.load())
	}
	st := dash().Providers["judge-429"]
	if st.RateLimitedUntil.IsZero() {
		t.Error("429 left no rate-limit cooldown in the shared health state")
	}
	if st.RateLimitKind != runtimestate.RateLimitTransient {
		t.Errorf("429 kind = %q, want transient (unclassified body)", st.RateLimitKind)
	}
	if st.ConsecutiveFailures != 0 {
		t.Errorf("429 counted toward the breaker (failures = %d), want rate-limit-only", st.ConsecutiveFailures)
	}
}

// Every target cooling: the exchange fails fast with an explicit message
// instead of hanging — the caller's fail-open path takes over.
func TestScheduledExchange_AllTargetsCoolingFailsFast(t *testing.T) {
	bad, badCalls := replyServer(t, http.StatusInternalServerError, `{"error":"down"}`)
	p, _ := newExchangeProxy(t,
		map[string]Provider{"judge-bad": {AnthropicBaseURL: bad.URL}},
		[]RouteTarget{{Provider: "judge-bad", Model: "m"}})
	snap := p.SnapshotRuntime()
	x := scheduledExchange{p: p}

	ctx1, cancel1 := context.WithTimeout(context.Background(), 600*time.Millisecond)
	_, err1 := x.exchange(ctx1, snap, "judge", exchangeBody(), nil)
	cancel1()
	if err1 == nil {
		t.Fatal("first exchange unexpectedly succeeded")
	}

	start := time.Now()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 600*time.Millisecond)
	_, err2 := x.exchange(ctx2, snap, "judge", exchangeBody(), nil)
	cancel2()
	if err2 == nil || !strings.Contains(err2.Error(), "unavailable (cooldown)") {
		t.Fatalf("second exchange error = %v, want all-unavailable", err2)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Errorf("all-cooling failure took %v, want fast-fail", elapsed)
	}
	if badCalls.load() != 1 {
		t.Errorf("cooling target calls = %d, want 1 (never retried while cooling)", badCalls.load())
	}
}

// The outer budget expiring is the caller's cancel: no circuit tick for the
// target whose attempt merely straddled the deadline (the client-cancel
// rule), while the target that genuinely burned its own slice still counts.
func TestScheduledExchange_OuterBudgetExpiryRecordsNothingForStraddledTarget(t *testing.T) {
	hangA, aCalls := hangServer(t)
	hangB, bCalls := hangServer(t)
	p, dash := newExchangeProxy(t,
		map[string]Provider{
			"judge-a": {AnthropicBaseURL: hangA.URL},
			"judge-b": {AnthropicBaseURL: hangB.URL},
		},
		[]RouteTarget{
			{Provider: "judge-a", Model: "m", Priority: 1},
			{Provider: "judge-b", Model: "m", Priority: 2},
		})
	snap := p.SnapshotRuntime()

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	_, err := scheduledExchange{p: p}.exchange(ctx, snap, "judge", exchangeBody(), nil)
	if err == nil {
		t.Fatal("exchange with two hanging targets unexpectedly succeeded")
	}
	if aCalls.load() != 1 || bCalls.load() != 1 {
		t.Fatalf("attempts a=%d b=%d, want 1 each", aCalls.load(), bCalls.load())
	}
	if got := dash().Providers["judge-a"].ConsecutiveFailures; got != 1 {
		t.Errorf("a consecutive failures = %d, want 1 (its own slice expired)", got)
	}
	if got := dash().Providers["judge-b"].ConsecutiveFailures; got != 0 {
		t.Errorf("b consecutive failures = %d, want 0 (outer-budget expiry is a caller cancel)", got)
	}
}

// The budget math: the first attempt gets the dominant share (remaining
// minus a 4s reserve per later attempt), and the reserve clamps down to the
// equal share when the total budget is small.
func TestSliceAttemptContext_DominantShareWithReserve(t *testing.T) {
	// 20s / 3 attempts: first gets 20 - 2*4 = 12s.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	actx, acancel := sliceAttemptContext(ctx, 3)
	defer acancel()
	d, ok := actx.Deadline()
	if !ok {
		t.Fatal("no attempt deadline")
	}
	if got := time.Until(d); got < 11*time.Second || got > 13*time.Second {
		t.Errorf("first-attempt budget = %v, want ~12s (dominant share)", got)
	}

	// 600ms / 2 attempts: the reserve clamps to the equal share (300ms).
	ctx2, cancel2 := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel2()
	actx2, acancel2 := sliceAttemptContext(ctx2, 2)
	defer acancel2()
	d2, _ := actx2.Deadline()
	if got := time.Until(d2); got < 250*time.Millisecond || got > 350*time.Millisecond {
		t.Errorf("small-budget first-attempt = %v, want ~300ms (reserve clamped to equal share)", got)
	}

	// Last attempt keeps whatever remains: its deadline IS the outer one
	// (WithCancel inherits the parent deadline — no additional cutoff).
	ctx3, cancel3 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel3()
	actx3, acancel3 := sliceAttemptContext(ctx3, 1)
	defer acancel3()
	outer, _ := ctx3.Deadline()
	got3, ok := actx3.Deadline()
	if !ok || !got3.Equal(outer) {
		t.Errorf("last attempt deadline = %v, want the outer budget %v", got3, outer)
	}
}
