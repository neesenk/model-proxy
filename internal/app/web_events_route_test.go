package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// waitFor polls cond until it holds or the timeout elapses (SSE handlers answer
// asynchronously from a goroutine).
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

// newWebMux mirrors the production composition in NewRuntime: proxy.Handler on
// "/" plus the web transport's /ui/ and /api/ subtrees (web.enabled default).
func newWebMux(t *testing.T, proxy *Proxy) *http.ServeMux {
	t.Helper()
	w := NewWebServer(proxy, "test-config.yaml")
	mux := http.NewServeMux()
	mux.HandleFunc("/", proxy.Handler)
	w.Register(mux)
	return mux
}

// TestMuxServesAPIEventsWithWebEnabled: with web.enabled (the default), GET
// /api/events must reach the SSE stream, not the web layer's 404 default.
// ServeMux dispatches /api/events to the more specific "/api/" subtree, so the
// branch inside proxy.Handler is only reachable with web disabled — the Live
// tab was dead in every default deployment (EventSource stuck reconnecting).
func TestMuxServesAPIEventsWithWebEnabled(t *testing.T) {
	proxy := newTestProxy(t, &Config{
		Providers: map[string]Provider{"z": {OpenAIBaseURL: "http://x", Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"glm": {{Provider: "z", Model: "glm"}}},
	})
	mux := newWebMux(t, proxy)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/events", nil).WithContext(ctx)
	// Exactly what the UI Live tab's EventSource sends: a same-origin browser
	// request against the loopback listener.
	req.Header.Set("Origin", "http://127.0.0.1:15721")
	req.Host = "127.0.0.1:15721"
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		mux.ServeHTTP(rec, req)
		close(done)
	}()
	// Recorder.Code defaults to 200 before WriteHeader, so poll on the
	// content-type instead: every terminal path (SSE stream or 404 JSON) sets it.
	waitFor(t, 2*time.Second, func() bool { return rec.Header().Get("Content-Type") != "" })
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/events status = %d body=%q, want 200 (SSE stream)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("content-type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SSE handler did not return after client disconnect")
	}
}

// TestMuxAPIEventsGuardedAgainstRebinding: a DNS-rebinding-shaped browser
// request (non-loopback Host) must be rejected on the events endpoint too —
// the live stream carries agent/model/provider metadata, so it joins the same
// loopback trust boundary as the rest of the admin API.
func TestMuxAPIEventsGuardedAgainstRebinding(t *testing.T) {
	proxy := newTestProxy(t, &Config{
		Providers: map[string]Provider{"z": {OpenAIBaseURL: "http://x", Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"glm": {{Provider: "z", Model: "glm"}}},
	})
	mux := newWebMux(t, proxy)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/events", nil).WithContext(ctx)
	req.Header.Set("Origin", "http://evil.example")
	req.Host = "evil.example"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET /api/events with rebinding Host status = %d, want 403", rec.Code)
	}
}

// TestDebugScheduleGuardedAgainstRebinding: /debug/schedule exposes routing,
// sticky-session and pin metadata; a DNS-rebinding-shaped browser request
// (non-loopback Host) must be rejected exactly like the admin API — it rides
// the same proxy handler but must not escape the loopback trust boundary.
func TestDebugScheduleGuardedAgainstRebinding(t *testing.T) {
	proxy := newTestProxy(t, &Config{
		Providers: map[string]Provider{"z": {OpenAIBaseURL: "http://x", Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"glm": {{Provider: "z", Model: "glm"}}},
	})
	req := httptest.NewRequest(http.MethodGet, "/debug/schedule", nil)
	req.Header.Set("Origin", "http://evil.example")
	req.Host = "evil.example"
	rec := httptest.NewRecorder()
	proxy.Handler(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET /debug/schedule with rebinding Host status = %d body=%q, want 403", rec.Code, rec.Body.String())
	}
	// Plain local CLI/curl (no browser identity headers) keeps full access.
	req2 := httptest.NewRequest(http.MethodGet, "/debug/schedule", nil)
	rec2 := httptest.NewRecorder()
	proxy.Handler(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("GET /debug/schedule without browser headers status = %d, want 200", rec2.Code)
	}
}
