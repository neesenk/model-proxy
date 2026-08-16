package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"model-proxy/internal/provider"
)

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

	cfg := &Config{
		Providers: map[string]Provider{
			"p1": {OpenAIBaseURL: upstream.URL, Provider: testProviderID},
			"p2": {OpenAIBaseURL: upstream.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{"m": {
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

	cfg := &Config{
		Providers: map[string]Provider{
			"plan": {OpenAIBaseURL: upstream.URL, Provider: testProviderID},
			"live": {OpenAIBaseURL: upstream.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{"m": {
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
