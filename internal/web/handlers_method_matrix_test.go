package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"model-proxy/internal/appapi"
	"model-proxy/internal/webauth"
)

// TestServeAPIWrongMethodMatrix pins the transport's method discipline for the
// UI's mutation endpoints: serveAPI routes on (path, method) pairs, so a wrong
// method must fall through to the JSON 404 — never reach the handler, never
// mutate. GET/DELETE-only endpoints are covered symmetrically.
func TestServeAPIWrongMethodMatrix(t *testing.T) {
	cases := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/tokens/reset"},
		{http.MethodPut, "/api/tokens/reset"},
		{http.MethodGet, "/api/config/edit"},
		{http.MethodPut, "/api/config"},
		{http.MethodPut, "/api/pin"},
		{http.MethodGet, "/api/accounts/zhipu"},
		{http.MethodPatch, "/api/login/aqp/start"},
		{http.MethodPost, "/api/status"},
		{http.MethodPut, "/api/requests"},
		{http.MethodPost, "/api/presets"},
		{http.MethodPut, "/api/quota/refresh"},
		{http.MethodGet, "/api/health/reset"},
		{http.MethodGet, "/api/health/freeze"},
		{http.MethodGet, "/api/models/refresh"},
		{http.MethodPut, "/api/models/refresh"},
	}
	for _, tc := range cases {
		rec := guardRequest(t, tc.method, tc.path, "", "")
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404 (no route for this method)", tc.method, tc.path, rec.Code)
			continue
		}
		if body := rec.Body.String(); !strings.Contains(body, "no api route") {
			t.Errorf("%s %s body = %s, want the JSON no-route error", tc.method, tc.path, body)
		}
	}
	// /metrics is not under serveAPI and answers 405 for non-GET.
	if got := guardRequest(t, http.MethodPost, "/metrics", "", "").Code; got != http.StatusMethodNotAllowed {
		t.Errorf("POST /metrics = %d, want 405", got)
	}
}

// TestAdminAuthGatesMutationEndpoints: the admin bearer check guards the
// whole /api/ subtree BEFORE method dispatch, so mutations (not just the GET
// surface) are 401 without a token and reachable with the valid one.
func TestAdminAuthGatesMutationEndpoints(t *testing.T) {
	s, _ := newAuthedServer(t, true)
	mutations := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/api/tokens/reset", ""},
		{http.MethodPost, "/api/quota/refresh", ""},
		{http.MethodPost, "/api/health/reset", ""},
		{http.MethodPost, "/api/health/freeze", ""},
		{http.MethodPost, "/api/pin", `{"route":"glm","provider":"zhipu"}`},
		{http.MethodDelete, "/api/pin?route=glm", ""},
		{http.MethodPost, "/api/config/validate", "listen: 127.0.0.1:0\nproviders: {}\n"},
	}
	for _, m := range mutations {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(m.method, m.path, strings.NewReader(m.body))
		serveWebRequest(s, rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without token = %d, want 401 (auth precedes dispatch)", m.method, m.path, rec.Code)
		}
	}
	// Cross-origin browser requests are rejected even with a valid token:
	// auth and origin guards stack on the mutation surface too.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/pin", strings.NewReader(`{"route":"glm","provider":"zhipu"}`))
	req.Header.Set("Authorization", "Bearer adm-secret")
	req.Header.Set("Origin", "http://evil.example")
	req.Host = "127.0.0.1:8123"
	serveWebRequest(s, rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("valid-token cross-origin mutation = %d, want 403", rec.Code)
	}
	// Valid token + same-origin: reaches the handler (200 from the stub).
	for _, m := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/quota/refresh", ""},
		{http.MethodPost, "/api/tokens/reset", ""},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(m.method, m.path, strings.NewReader(m.body))
		req.Header.Set("Authorization", "Bearer adm-secret")
		serveWebRequest(s, rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s %s with valid token = %d body=%s, want 200 (stub command)", m.method, m.path, rec.Code, rec.Body.String())
		}
	}
}

// TestEventsRouteThroughTransportGuards exercises the composition root's real
// wiring for the UI live view: the hub handler injected via Options.Events is
// served on GET /api/events THROUGH serveAPI — i.e. behind the admin bearer
// check and the browser-origin guard, in that order.
func TestEventsRouteThroughTransportGuards(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "admin.tok")
	if err := os.WriteFile(tokenPath, []byte("adm-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := webauth.NewSource(tokenPath)
	sse := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.Write([]byte("event: end\ndata: {}\n\n"))
	})
	s, err := New(Options{
		Reads:     &readAPIStub{dashboard: appapi.Dashboard{Counters: map[string]appapi.Metrics{}}},
		Commands:  &testCommandAPI{},
		Version:   "t",
		Events:    sse,
		AdminAuth: func() *webauth.Source { return src },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)

	// No token → 401 before the SSE handler runs.
	rec := httptest.NewRecorder()
	serveWebRequest(s, rec, httptest.NewRequest(http.MethodGet, "/api/events", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /api/events without token = %d, want 401", rec.Code)
	}
	// Valid token but browser cross-origin → 403 (guard order: auth, origin).
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/events", nil)
	req.Header.Set("Authorization", "Bearer adm-secret")
	req.Header.Set("Origin", "http://evil.example")
	req.Host = "127.0.0.1:8123"
	serveWebRequest(s, rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET /api/events cross-origin = %d, want 403", rec.Code)
	}
	// Plain client with the token → the injected handler answers SSE.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/events", nil)
	req.Header.Set("Authorization", "Bearer adm-secret")
	serveWebRequest(s, rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/events with token = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("content-type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q, want text/event-stream", ct)
	}
	if body := rec.Body.String(); !strings.Contains(body, "event: end") {
		t.Errorf("SSE body missing the injected event: %q", body)
	}

	// Browser EventSource cannot set Authorization headers. Establish the
	// HttpOnly /api session, then prove the SSE route accepts its cookie.
	sessionRec := httptest.NewRecorder()
	sessionReq := httptest.NewRequest(http.MethodPost, "/api/auth/session", nil)
	sessionReq.Header.Set("Authorization", "Bearer adm-secret")
	serveWebRequest(s, sessionRec, sessionReq)
	if sessionRec.Code != http.StatusNoContent || len(sessionRec.Result().Cookies()) != 1 {
		t.Fatalf("create browser session = %d cookies=%d", sessionRec.Code, len(sessionRec.Result().Cookies()))
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/events", nil)
	req.AddCookie(sessionRec.Result().Cookies()[0])
	serveWebRequest(s, rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("content-type") != "text/event-stream" {
		t.Fatalf("GET /api/events with session = %d content-type=%q, want SSE", rec.Code, rec.Header().Get("content-type"))
	}
}

// TestMetricsEmptyDashboard: with zero counters the exposition still renders
// the full HELP/TYPE scaffolding and not a single series line — an empty
// scrape must be well-formed, not blank.
func TestMetricsEmptyDashboard(t *testing.T) {
	s, err := New(Options{
		Reads:    &readAPIStub{dashboard: appapi.Dashboard{Counters: map[string]appapi.Metrics{}}},
		Commands: &testCommandAPI{},
		Version:  "t",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	rec := httptest.NewRecorder()
	serveWebRequest(s, rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("content-type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Errorf("content-type = %q, want Prometheus 0.0.4", ct)
	}
	body := rec.Body.String()
	if strings.Count(body, "# HELP ") != 6 || strings.Count(body, "# TYPE ") != 6 {
		t.Errorf("empty dashboard rendered %d/%d HELP/TYPE blocks, want 6/6:\n%s",
			strings.Count(body, "# HELP "), strings.Count(body, "# TYPE "), body)
	}
	for _, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		if !strings.HasPrefix(line, "#") {
			t.Errorf("empty dashboard must have zero series lines, got %q", line)
		}
	}
}
