package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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

	// The SSE handler runs on the server's own goroutine and writes headers while
	// this test observes them — a shared httptest.ResponseRecorder is NOT
	// thread-safe for that (data race on its header map). Serving through a real
	// httptest.Server keeps the assertion honest: net/http serializes every
	// header write before the bytes hit the wire, so client.Do returning means
	// the terminal status + content-type are already decided — no polling needed.
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/events", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	// Exactly what the UI Live tab's EventSource sends: a same-origin browser
	// request against the loopback listener.
	req.Header.Set("Origin", "http://127.0.0.1:15721")
	req.Host = "127.0.0.1:15721"
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /api/events: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /api/events status = %d body=%q, want 200 (SSE stream)", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}
	// Cancelling the request context must end the SSE stream: read until EOF
	// with a deadline-ish bound via ctx (already cancelled below) — the server
	// side handler returns on r.Context().Done(), closing the response body.
	cancel()
	if _, err := io.ReadAll(resp.Body); err != nil && ctx.Err() == nil {
		t.Fatalf("read SSE stream after cancel: %v", err)
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
