package forward

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
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
