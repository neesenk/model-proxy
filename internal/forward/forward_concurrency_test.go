package forward

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"model-proxy/internal/targetexec"
)

// allSaturated arranges the harness so every target is at its concurrency
// limit: all-down, rate-limit class, no time horizon, and waitable.
func allSaturated(h *harness) {
	h.state.allDown, h.state.allRateLimited, h.state.saturated = true, true, true
}

// TestServeAllSaturatedWaitsForASlotThenCommits: with every target at its
// concurrency limit the pipeline parks until a request completes on one of
// them, then re-runs the pass. Before this the state had no representation —
// the earliest recovery was "now", so the request was answered 429 with a
// Retry-After of the whole transient backoff while a slot was freeing within
// seconds.
func TestServeAllSaturatedWaitsForASlotThenCommits(t *testing.T) {
	hits := 0
	up := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		openaiOKResponder("after-slot")(w, r)
	})
	h := newHarness()
	allSaturated(h)
	var waited []time.Duration
	h.state.waitSlot = func(max time.Duration) (bool, bool) {
		waited = append(waited, max)
		return true, false
	}
	cfg := &Config{
		Providers:  map[string]Provider{"up": {OpenAIBaseURL: up.srv.URL, Provider: "test-static"}},
		Routes:     map[string][]RouteTarget{"m": {{Provider: "up", Model: "real-model"}}},
		Scheduling: Scheduling{RetryWait: "3s"},
	}
	w := h.serve(h.snapshot(cfg), "openai", "/v1/chat/completions", openaiChatBody(), nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "after-slot") {
		t.Fatalf("status = %d body = %s, want the post-wait commit", w.Code, w.Body.String())
	}
	// The wait is bounded by retry_wait, and its event is the freed slot —
	// not the cooldown horizon, which here is zero.
	if !reflect.DeepEqual(waited, []time.Duration{3 * time.Second}) {
		t.Errorf("slot waits = %v, want one wait bounded by retry_wait (3s)", waited)
	}
}

// TestServeAllSaturatedAnswers429AfterTheWaits: a slot that never frees ends
// in 429 after the round budget is spent — with a one-second Retry-After,
// since a slot frees within a request's lifetime, not a backoff's.
func TestServeAllSaturatedAnswers429AfterTheWaits(t *testing.T) {
	up := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTooManyRequests) })
	h := newHarness()
	allSaturated(h)
	waits := 0
	h.state.waitSlot = func(time.Duration) (bool, bool) {
		waits++
		return false, false
	}
	cfg := &Config{
		Providers:  map[string]Provider{"up": {OpenAIBaseURL: up.srv.URL, Provider: "test-static"}},
		Routes:     map[string][]RouteTarget{"m": {{Provider: "up", Model: "real-model"}}},
		Scheduling: Scheduling{RetryWait: "3s"},
	}
	w := h.serve(h.snapshot(cfg), "openai", "/v1/chat/completions", openaiChatBody(), nil)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d body = %s, want 429", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want 1 (a slot frees within a request's lifetime)", got)
	}
	if waits != 2 {
		t.Errorf("slot waits = %d, want the two budgeted rounds", waits)
	}
}

// TestServeClientGoneDuringInflightWait: a client that leaves while parked on
// a slot ends as 499 with nothing written, exactly like the cooldown wait.
func TestServeClientGoneDuringInflightWait(t *testing.T) {
	up := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTooManyRequests) })
	h := newHarness()
	allSaturated(h)
	h.state.waitSlot = func(time.Duration) (bool, bool) { return false, true }
	cfg := &Config{
		Providers:  map[string]Provider{"up": {OpenAIBaseURL: up.srv.URL, Provider: "test-static"}},
		Routes:     map[string][]RouteTarget{"m": {{Provider: "up", Model: "real-model"}}},
		Scheduling: Scheduling{RetryWait: "3s"},
	}
	w := h.serve(h.snapshot(cfg), "openai", "/v1/chat/completions", openaiChatBody(), nil)
	if ends := h.endEvents("req-test"); len(ends) != 1 || ends[0].Status != statusClientGone {
		t.Errorf("end events = %+v, want one 499 (client closed)", ends)
	}
	if w.Body.Len() != 0 {
		t.Errorf("body = %q, want nothing written to a client that left", w.Body.String())
	}
}

// TestServeSaturatedWithoutRetryWaitSkipsTheWait: retry_wait "0" disables the
// slot wait along with the cooldown wait.
func TestServeSaturatedWithoutRetryWaitSkipsTheWait(t *testing.T) {
	up := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTooManyRequests) })
	h := newHarness()
	allSaturated(h)
	waits := 0
	h.state.waitSlot = func(time.Duration) (bool, bool) {
		waits++
		return true, false
	}
	cfg := &Config{
		Providers:  map[string]Provider{"up": {OpenAIBaseURL: up.srv.URL, Provider: "test-static"}},
		Routes:     map[string][]RouteTarget{"m": {{Provider: "up", Model: "real-model"}}},
		Scheduling: Scheduling{RetryWait: "0"},
	}
	w := h.serve(h.snapshot(cfg), "openai", "/v1/chat/completions", openaiChatBody(), nil)
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "1" || waits != 0 {
		t.Fatalf("status = %d retry-after = %q waits = %d; want 429 / 1 / no wait", w.Code, w.Header().Get("Retry-After"), waits)
	}
}

// TestServeAllDownWithoutSaturationKeepsHorizonRules: saturation is opt-in
// through ConcurrencyWaitable — an all-down route with no waitable target
// still follows the dated-horizon rules (here: no horizon, so the terminal
// Retry-After is the transient backoff).
func TestServeAllDownWithoutSaturationKeepsHorizonRules(t *testing.T) {
	up := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTooManyRequests) })
	h := newHarness()
	h.state.allDown, h.state.allRateLimited = true, true
	waits := 0
	h.state.waitSlot = func(time.Duration) (bool, bool) {
		waits++
		return true, false
	}
	cfg := &Config{
		Providers:  map[string]Provider{"up": {OpenAIBaseURL: up.srv.URL, Provider: "test-static"}},
		Routes:     map[string][]RouteTarget{"m": {{Provider: "up", Model: "real-model"}}},
		Scheduling: Scheduling{RetryWait: "3s", RateLimitBackoff: "90s"},
	}
	w := h.serve(h.snapshot(cfg), "openai", "/v1/chat/completions", openaiChatBody(), nil)
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "90" || waits != 0 {
		t.Fatalf("status = %d retry-after = %q waits = %d; want 429 / 90 / no slot wait", w.Code, w.Header().Get("Retry-After"), waits)
	}
}

// racingGate holds every request at the gate's take until `racers` of them
// have arrived: all of them are past the scheduler before any of them takes a
// slot. That is the shape of every freed slot with queued requests — the
// scheduler hands the slot to everyone who asks, and the gate picks the winner.
type racingGate struct {
	*fakeHealthGate
	racers  int
	arrived chan struct{}
	full    chan struct{}
	once    sync.Once
}

func newRacingGate(racers int) *racingGate {
	return &racingGate{
		fakeHealthGate: newFakeHealthGate(),
		racers:         racers,
		arrived:        make(chan struct{}, racers),
		full:           make(chan struct{}),
	}
}

func (g *racingGate) TakeInflight(provider string) (targetexec.InflightSlot, bool) {
	g.arrived <- struct{}{}
	g.once.Do(func() {
		for i := 0; i < g.racers; i++ {
			<-g.arrived
		}
		close(g.full)
	})
	<-g.full
	return g.fakeHealthGate.TakeInflight(provider)
}

// TestServeLostSlotRaceReleasesNoVerdict: two requests are both handed the
// account's single free slot and race for it at the gate. One is dispatched;
// the other is refused before any network contact, and what it gives back to
// the ledger is "no verdict" — never a failure of the account. Counting the
// loser as a failure zeroed the inferred cap's probe-up streak on every slot
// turnover for as long as requests were queued, so the cap could not climb.
func TestServeLostSlotRaceReleasesNoVerdict(t *testing.T) {
	up := newFakeUpstream(t, openaiOKResponder("won"))
	h := newHarness()
	gate := newRacingGate(2)
	h.svc.NewHealthGate = func(Snapshot) targetexec.HealthGate { return gate }
	gate.RecordConcurrencyLimit("up", 1, 0, 1) // a one-slot account
	cfg := &Config{
		Providers: map[string]Provider{"up": {OpenAIBaseURL: up.srv.URL, Provider: "test-static"}},
		Routes:    map[string][]RouteTarget{"m": {{Provider: "up", Model: "real-model"}}},
	}
	snap := h.snapshot(cfg)

	results := make(chan *httptest.ResponseRecorder, 2)
	for i := 0; i < 2; i++ {
		go func() { results <- h.serve(snap, "openai", "/v1/chat/completions", openaiChatBody(), nil) }()
	}
	statuses := []int{}
	for len(statuses) < 2 {
		select {
		case w := <-results:
			statuses = append(statuses, w.Code)
		case <-time.After(10 * time.Second):
			t.Fatalf("statuses so far = %v: both racing requests must finish", statuses)
		}
	}
	sort.Ints(statuses)
	// The winner is served; the loser has no other target and ends as an
	// ordinary all-targets-failed 502.
	if want := []int{http.StatusOK, http.StatusBadGateway}; !reflect.DeepEqual(statuses, want) {
		t.Fatalf("statuses = %v, want %v", statuses, want)
	}
	if up.hits() != 1 {
		t.Errorf("upstream hits = %d, want 1: the refused race never reached the network", up.hits())
	}
	released := gate.releasedOutcomes("up")
	if len(released) != 2 {
		t.Fatalf("slot releases = %v, want exactly two", released)
	}
	verdicts, noVerdicts := 0, 0
	for _, outcome := range released {
		switch outcome {
		case targetexec.InflightSucceeded:
			verdicts++
		case targetexec.InflightNoVerdict:
			noVerdicts++
		case targetexec.InflightFailed:
			t.Errorf("slot releases = %v: the account has no failed request to report", released)
		}
	}
	if verdicts != 1 || noVerdicts != 1 {
		t.Errorf("slot releases = %v, want one success and one no-verdict", released)
	}
}
