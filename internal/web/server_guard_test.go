package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newGuardServer(t *testing.T) *Server {
	t.Helper()
	server, err := New(Options{Reads: testReadAPI{}, Commands: testCommandAPI{}})
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func guardRequest(t *testing.T, method, path, origin, host string) *httptest.ResponseRecorder {
	t.Helper()
	server := newGuardServer(t)
	request := httptest.NewRequest(method, path, strings.NewReader("{}"))
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	if host != "" {
		request.Host = host
	}
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	return recorder
}

// Regression: the admin API is unauthenticated by design, so it must not be
// reachable from BROWSER context other than the UI's own origin. A page on
// evil.com can fire a CORS simple request (text/plain body, no preflight) at
// every mutation endpoint, and DNS rebinding can read GET /api/config — which
// answers with the verbatim YAML including static provider keys.
func TestGuardBlocksCrossOriginBrowserRequests(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodGet} {
		if got := guardRequest(t, method, "/api/config", "http://evil.example", "127.0.0.1:8123").Code; got != http.StatusForbidden {
			t.Errorf("%s cross-origin: status=%d, want 403", method, got)
		}
	}
	// "null" Origin (sandboxed frames) is cross-origin too.
	if got := guardRequest(t, http.MethodPost, "/api/pin", "null", "127.0.0.1:8123").Code; got != http.StatusForbidden {
		t.Errorf("null origin: status=%d, want 403", got)
	}
}

func TestGuardBlocksDNSRebinding(t *testing.T) {
	// Rebinding makes the page same-origin from the browser's view (Origin ==
	// Host), so ONLY the loopback Host check stops it.
	if got := guardRequest(t, http.MethodGet, "/api/config", "http://attacker.rebound:8123", "attacker.rebound:8123").Code; got != http.StatusForbidden {
		t.Errorf("rebinding: status=%d, want 403", got)
	}
}

func TestGuardAllowsSameOriginBrowserAndPlainClients(t *testing.T) {
	// Same-origin browser fetch: Origin matches the loopback Host.
	if got := guardRequest(t, http.MethodGet, "/api/config", "http://127.0.0.1:8123", "127.0.0.1:8123").Code; got == http.StatusForbidden {
		t.Errorf("same-origin browser request was rejected (%d)", got)
	}
	if got := guardRequest(t, http.MethodGet, "/api/config", "http://localhost:8123", "localhost:8123").Code; got == http.StatusForbidden {
		t.Errorf("localhost same-origin request was rejected (%d)", got)
	}
	if got := guardRequest(t, http.MethodGet, "/api/config", "http://[::1]:8123", "[::1]:8123").Code; got == http.StatusForbidden {
		t.Errorf("ipv6 loopback same-origin request was rejected (%d)", got)
	}
	// CLI/curl (no Origin, no Sec-Fetch-Site) with an arbitrary Host: the
	// daemonctl client and scripts address the port directly.
	if got := guardRequest(t, http.MethodGet, "/api/config", "", "").Code; got == http.StatusForbidden {
		t.Errorf("plain client request was rejected (%d)", got)
	}
}

func TestGuardServesUIDenyListing(t *testing.T) {
	// The static UI must not be reachable through a rebound domain either.
	request := httptest.NewRequest(http.MethodGet, "/ui/", nil)
	request.Header.Set("Origin", "http://attacker.rebound:8123")
	request.Host = "attacker.rebound:8123"
	recorder := httptest.NewRecorder()
	newGuardServer(t).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Errorf("ui via rebinding: status=%d, want 403", recorder.Code)
	}
}
