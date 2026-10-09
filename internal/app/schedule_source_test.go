// schedule_source_test.go — composition coverage for the Bifrost-borrowed
// scheduling semantics: schedule.source provenance in the request log, the
// x-mp-sticky: off per-request override, and session-key hygiene at the
// forward boundary. Cross-module wiring lives here (owner-package behavior
// is pinned in internal/runtime and internal/protocol).
package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/forward"
	"model-proxy/internal/observe/requestlog"
	"model-proxy/internal/provider"
	runtimestate "model-proxy/internal/runtime"
)

// scheduleSourceFixture builds a proxy with one route over two providers and
// a request logger; the shared upstream answers 200. The quota snapshots are
// set AFTER construction on generation 1 (the fixture's generation).
func scheduleSourceFixture(t *testing.T) (*Proxy, string) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(upstream.Close)

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"a": {Provider: testProviderID, OpenAIBaseURL: upstream.URL},
			"b": {Provider: testProviderID, OpenAIBaseURL: upstream.URL},
		},
		Routes: map[string][]configdomain.RouteTarget{
			"m": {{Provider: "a", Model: "m"}, {Provider: "b", Model: "m"}},
		},
	}
	p := newTestProxy(t, cfg)
	p.providers["a"] = &testProv{key: "ka"}
	p.providers["b"] = &testProv{key: "kb"}
	now := time.Now()
	p.runtimeState.SetQuota("a", &provider.QuotaSnapshot{
		Billing: provider.BillingPlan, AsOf: now,
		RemainingPct: .5,
		Windows:      []provider.QuotaWindow{{Ultimate: true, RemainingPct: .5, Total: 100, ResetsAt: now, Duration: time.Hour}},
	}, 1)
	p.runtimeState.SetQuota("b", &provider.QuotaSnapshot{
		Billing: provider.BillingPlan, AsOf: now,
		RemainingPct: 1,
		Windows:      []provider.QuotaWindow{{Ultimate: true, RemainingPct: 1, Total: 100, ResetsAt: now, Duration: time.Hour}},
	}, 1)

	logDir := t.TempDir()
	p.reqLog = requestlog.New(requestlog.Options{Directory: logDir, MaxFileSize: 1 << 20, MaxBodyBytes: 1 << 10})
	p.reqLogStarted = p.lifecycle.Run(func(<-chan struct{}) { p.reqLog.Run() })
	t.Cleanup(p.Close)

	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	t.Cleanup(px.Close)
	return p, px.URL
}

// postWithSession posts with an optional session header and optional
// x-mp-sticky override, returning the client-visible status.
func postScheduleProbe(t *testing.T, url, body, session, stickyHeader string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if session != "" {
		req.Header.Set("x-claude-code-session-id", session)
	}
	if stickyHeader != "" {
		req.Header.Set("x-mp-sticky", stickyHeader)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// TestScheduleSourceLogged: the request log records which scheduling engine
// produced the try order — quota for a fresh binding, sticky for a held
// (session, route) binding, force when a force-provider override collapsed
// the chain before ranking.
func TestScheduleSourceLogged(t *testing.T) {
	p, base := scheduleSourceFixture(t)
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`

	for i := 0; i < 3; i++ {
		if code := postScheduleProbe(t, base+"/v1/chat/completions", body, "s1", ""); code != 200 {
			t.Fatalf("request %d status = %d, want 200", i, code)
		}
	}
	force, _ := http.NewRequest(http.MethodPost, base+"/v1/chat/completions", strings.NewReader(body))
	force.Header.Set("Content-Type", "application/json")
	force.Header.Set("x-claude-code-session-id", "s1")
	force.Header.Set("x-mp-force-provider", "a")
	resp, err := http.DefaultClient.Do(force)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("forced request status = %d, want 200", resp.StatusCode)
	}

	records := allRecords(t, p.reqLog.Directory())
	if len(records) != 4 {
		t.Fatalf("records = %d, want 4 (one per attempt)", len(records))
	}
	// allRecords walks the JSONL chronologically (oldest first).
	wantSources := []string{"quota", "sticky", "sticky", "force"}
	for i, want := range wantSources {
		rec := records[i]
		if rec.Schedule == nil || rec.Schedule.Source != want {
			t.Fatalf("attempt %d schedule = %+v, want source %q", i, rec.Schedule, want)
		}
	}
}

// TestStickyOffOverrideComposition: x-mp-sticky: off makes the request pick
// fresh (source stays quota, not sticky) and leaves the existing binding
// untouched — no refresh, no new entry.
func TestStickyOffOverrideComposition(t *testing.T) {
	p, base := scheduleSourceFixture(t)
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`

	if code := postScheduleProbe(t, base+"/v1/chat/completions", body, "s1", ""); code != 200 {
		t.Fatalf("parking request status = %d, want 200", code)
	}
	key := runtimestate.StickyKey("s1", "m")
	parked, ok := p.runtimeState.Sticky(key)
	if !ok || parked.Provider == "" {
		t.Fatalf("session binding after first request = %+v (ok=%v), want a committed park", parked, ok)
	}
	parkedSince := parked.Since

	if code := postScheduleProbe(t, base+"/v1/chat/completions", body, "s1", "off"); code != 200 {
		t.Fatalf("override request status = %d, want 200", code)
	}

	// The override request must NOT report the sticky engine.
	records := allRecords(t, p.reqLog.Directory())
	if len(records) != 2 {
		t.Fatalf("records = %d, want 2", len(records))
	}
	overridden := records[len(records)-1] // chronological latest
	if overridden.Schedule == nil || overridden.Schedule.Source != "quota" {
		t.Fatalf("override record schedule = %+v, want source quota (affinity opted out)", overridden.Schedule)
	}
	// The pre-existing binding survived untouched (same Since — no refresh).
	entry, ok := p.runtimeState.Sticky(key)
	if !ok || !entry.Since.Equal(parkedSince) {
		t.Fatalf("binding after override = %+v (ok=%v), want Since unchanged at %v", entry, ok, parkedSince)
	}
}

// TestStickyOffOverrideUnknownValueIgnored: only the exact value "off" (any
// case) opts out — a typo must not change routing.
func TestStickyOffOverrideUnknownValueIgnored(t *testing.T) {
	p, base := scheduleSourceFixture(t)
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`

	if code := postScheduleProbe(t, base+"/v1/chat/completions", body, "s1", ""); code != 200 {
		t.Fatalf("parking request status = %d", code)
	}
	if code := postScheduleProbe(t, base+"/v1/chat/completions", body, "s1", "no"); code != 200 {
		t.Fatalf("typo request status = %d", code)
	}
	records := allRecords(t, p.reqLog.Directory())
	if len(records) != 2 {
		t.Fatalf("records = %d, want 2", len(records))
	}
	latest := records[len(records)-1]
	if latest.Schedule == nil || latest.Schedule.Source != "sticky" {
		t.Fatalf("typo record schedule = %+v, want sticky (unknown override values are ignored)", latest.Schedule)
	}
}

// TestScheduleSourceForceVocabulary: forward cannot import runtime, so its
// force constant is pinned equal here — the two vocabularies must never
// drift apart.
func TestScheduleSourceForceVocabulary(t *testing.T) {
	if forward.ScheduleSourceForce != runtimestate.ScheduleSourceForce {
		t.Fatalf("forward.ScheduleSourceForce = %q, runtime.ScheduleSourceForce = %q — vocabulary drifted",
			forward.ScheduleSourceForce, runtimestate.ScheduleSourceForce)
	}
}

// TestInvalidSessionHeaderFallsBackToRouteKey: a malformed session header
// (oversized / control bytes) is treated as NO session — the sticky falls
// back to the bare route key instead of grouping under a mangled identity.
func TestInvalidSessionHeaderFallsBackToRouteKey(t *testing.T) {
	p, base := scheduleSourceFixture(t)
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`

	if code := postScheduleProbe(t, base+"/v1/chat/completions", body, strings.Repeat("x", 300), ""); code != 200 {
		t.Fatalf("oversized-session request status = %d, want 200", code)
	}
	dash := p.runtimeState.Dashboard(time.Now())
	for key := range dash.Sticky {
		if strings.HasPrefix(key, strings.Repeat("x", 256)) {
			t.Fatalf("oversized session key entered the sticky map: %q", key)
		}
	}
	if entry, ok := dash.Sticky["m"]; !ok || entry.Provider == "" {
		t.Fatalf("route-key sticky = %+v (ok=%v), want the sessionless fallback binding", entry, ok)
	}

	// Control bytes cannot cross a real HTTP client (net/http rejects the
	// header), so drive the forward handler directly — the hygiene contract
	// is about what the PIPELINE accepts, not what a spec-conforming client
	// can send.
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-claude-code-session-id", "bad\x00session")
	w := httptest.NewRecorder()
	p.forward("openai", w, req, nextRequestID())
	if w.Code != 200 {
		t.Fatalf("control-byte-session forward status = %d, want 200", w.Code)
	}
	for key := range p.runtimeState.Dashboard(time.Now()).Sticky {
		if strings.Contains(key, "bad\x00session") {
			t.Fatalf("control-byte session key entered the sticky map: %q", key)
		}
	}
}
