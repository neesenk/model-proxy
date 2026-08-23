package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"model-proxy/internal/observe/counters"
)

// TestForward_AttemptOutcomeCounters: every upstream attempt is classified
// into the virtual ("attempts", outcome) series — a failing first target and
// a committed second target must yield exactly one hard + one ok, with the
// real per-provider counters unchanged in shape.
func TestForward_AttemptOutcomeCounters(t *testing.T) {
	failingUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"boom"}`))
	}))
	defer failingUp.Close()
	okUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	}))
	defer okUp.Close()

	cfg := &Config{
		Providers: map[string]Provider{
			"bad-prov":  {OpenAIBaseURL: failingUp.URL, Provider: testProviderID},
			"good-prov": {OpenAIBaseURL: okUp.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{
			"glm": {{Provider: "bad-prov", Model: "m"}, {Provider: "good-prov", Model: "m"}},
		},
	}
	p := newTestProxy(t, cfg)
	p.providers["bad-prov"] = &testProv{key: "x"}
	p.providers["good-prov"] = &testProv{key: "y"}
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	resp, err := http.DefaultClient.Post(px.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"glm","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want failover to good-prov and 200", resp.StatusCode)
	}

	snap := p.metrics.Snapshot()
	hard := snap[counters.PMKey{Provider: "attempts", Model: "hard"}].Requests
	ok := snap[counters.PMKey{Provider: "attempts", Model: "ok"}].Requests
	rateLimited := snap[counters.PMKey{Provider: "attempts", Model: "rate_limited"}].Requests
	if hard != 1 || ok != 1 || rateLimited != 0 {
		t.Errorf("attempt outcomes = hard %d, ok %d, rate_limited %d; want 1/1/0", hard, ok, rateLimited)
	}
	// The real provider rows keep their own accounting: one request committed
	// on good-prov, one failure on bad-prov.
	if snap[counters.PMKey{Provider: "good-prov", Model: "m"}].Requests != 1 {
		t.Errorf("good-prov requests = %d, want 1", snap[counters.PMKey{Provider: "good-prov", Model: "m"}].Requests)
	}
	if snap[counters.PMKey{Provider: "bad-prov", Model: "m"}].Failures < 1 {
		t.Errorf("bad-prov failures = %d, want ≥1", snap[counters.PMKey{Provider: "bad-prov", Model: "m"}].Failures)
	}

	// Routing-decision overhead: one observation per request under the virtual
	// ("routing","decision") row, latency sum populated.
	routing := snap[counters.PMKey{Provider: "routing", Model: "decision"}]
	if routing.Requests < 1 {
		t.Errorf("routing observations = %d, want ≥1", routing.Requests)
	}
}
