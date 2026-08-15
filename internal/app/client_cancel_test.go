package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// Regression: a client disconnect while waiting for upstream response HEADERS
// used to be classified as a hard upstream failure. One cancelled request then
// burned the whole failover chain (every remaining target failed instantly on
// the dead request context, each earning a circuit tick), so three user
// interrupts could open every circuit on the route. Post-fix: the loop stops
// at the first cancelled target, nothing is recorded, and the live monitor
// closes the request with a 499 end event like the cooldown-wait cancel path.
func TestUC_ClientCancelDuringHeadersStopsFailoverAndKeepsCircuitClosed(t *testing.T) {
	var blockedHits int32
	// Go's http1 transport does not promptly close an outbound connection that
	// carried a request BODY when the round trip is canceled, so the upstream
	// handler's r.Context() never fires here. Release the handlers explicitly
	// instead of relying on cancel propagation (in production the executor's
	// upstream timeout bounds the lingering connection).
	release := make(chan struct{})
	var blockedHitsDone int32
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&blockedHits, 1)
		// Never answer: hang until the proxy drops the outbound request.
		select {
		case <-r.Context().Done():
		case <-release:
		}
		atomic.AddInt32(&blockedHitsDone, 1)
	}))
	defer blocked.Close()
	fallback, fallbackSeen := newCaptureUpstream(200, `{"ok":true}`)
	defer fallback.Close()

	cfg := &Config{
		Providers: map[string]Provider{
			"blocked":  {OpenAIBaseURL: blocked.URL, Provider: testProviderID},
			"fallback": {OpenAIBaseURL: fallback.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{
			"m1": {
				{Provider: "blocked", Model: "m1", Priority: 1},
				{Provider: "fallback", Model: "m1", Priority: 2},
			},
			"m2": {{Provider: "fallback", Model: "m2"}},
		},
		// Threshold 3: pre-fix, three cancelled requests recorded 3 failures
		// per provider → both circuits open. retry_wait 0 keeps the terminal
		// contrast immediate (no cooldown-wait rounds).
		Scheduling: Scheduling{CircuitThreshold: 3, RetryWait: "0"},
	}
	p := newProxyWithStatic(t, cfg, map[string]string{"blocked": "b", "fallback": "f"})
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	cancelledEnds := func() int {
		n := 0
		for _, e := range p.events.Snapshot() {
			if e.Type == "end" && e.Status == 499 {
				n++
			}
		}
		return n
	}

	for round := 1; round <= 3; round++ {
		ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, px.URL+"/v1/chat/completions", stringReader(`{"model":"m1","messages":[]}`))
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		cancel()
		// Deterministic hand-off: the handler publishes the 499 end event right
		// before returning, so once it appears this round is fully finished.
		deadline := time.Now().Add(2 * time.Second)
		for cancelledEnds() < round && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if got := cancelledEnds(); got != round {
			t.Fatalf("round %d: 499 end events=%d — cancelled request was not classified client-gone", round, got)
		}
	}

	if hits := atomic.LoadInt32(&blockedHits); hits != 3 {
		t.Errorf("blocked upstream hits=%d want 3", hits)
	}
	// Unblock the hung upstream handlers before server teardown.
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&blockedHitsDone) < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&blockedHitsDone); got != 3 {
		t.Fatalf("blocked upstream handlers exited=%d, want 3", got)
	}
	if burned := len(*fallbackSeen); burned != 0 {
		t.Errorf("failover burned %d fallback calls on a dead request context", burned)
	}

	// No failure was ever recorded, so fallback must still serve immediately.
	// Pre-fix its circuit was open after round 3 → 502.
	resp, err := http.Post(px.URL+"/v1/chat/completions", "application/json", stringReader(`{"model":"m2","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Errorf("status=%d want 200 (cancellations must not open circuits), body=%s", resp.StatusCode, body)
	}
}
