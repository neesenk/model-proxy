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
	serveWebRequest(server, recorder, request)
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
	request := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	request.Header.Set("Sec-Fetch-Site", "cross-site")
	request.Host = "127.0.0.1:8123"
	recorder := httptest.NewRecorder()
	serveWebRequest(newGuardServer(t), recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Errorf("Sec-Fetch-Site cross-site without Origin: status=%d, want 403", recorder.Code)
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
	// Same-origin browser fetch: Origin matches the loopback Host. Assert the
	// exact 200 — `!= 403` would also pass a 500 from a broken handler.
	if got := guardRequest(t, http.MethodGet, "/api/config", "http://127.0.0.1:8123", "127.0.0.1:8123").Code; got != http.StatusOK {
		t.Errorf("same-origin browser request = %d, want 200", got)
	}
	if got := guardRequest(t, http.MethodGet, "/api/config", "http://localhost:8123", "localhost:8123").Code; got != http.StatusOK {
		t.Errorf("localhost same-origin request = %d, want 200", got)
	}
	if got := guardRequest(t, http.MethodGet, "/api/config", "http://[::1]:8123", "[::1]:8123").Code; got != http.StatusOK {
		t.Errorf("ipv6 loopback same-origin request = %d, want 200", got)
	}
	// CLI/curl (no Origin, no Sec-Fetch-Site) with an arbitrary Host: the
	// daemonctl client and scripts address the port directly.
	if got := guardRequest(t, http.MethodGet, "/api/config", "", "").Code; got != http.StatusOK {
		t.Errorf("plain client request = %d, want 200", got)
	}
}

func TestGuardServesUIDenyListing(t *testing.T) {
	// The static UI must not be reachable through a rebound domain either.
	request := httptest.NewRequest(http.MethodGet, "/ui/", nil)
	request.Header.Set("Origin", "http://attacker.rebound:8123")
	request.Host = "attacker.rebound:8123"
	recorder := httptest.NewRecorder()
	serveWebRequest(newGuardServer(t), recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Errorf("ui via rebinding: status=%d, want 403", recorder.Code)
	}
}

// GuardForwardBrowserOrigin: the quota-spending forward surface (/v1/*, /mcp/*)
// carries no credential on the default loopback deployment, so a cross-site
// page's CORS simple request must not spend subscription quota, and DNS
// rebinding must not ride a non-loopback Host.
func TestGuardForwardBrowserOrigin(t *testing.T) {
	pass := func(name string, request *http.Request, keyAuthEnabled bool, trusted string) {
		t.Helper()
		recorder := httptest.NewRecorder()
		if !GuardForwardBrowserOrigin(recorder, request, keyAuthEnabled, trusted) {
			t.Errorf("%s: rejected, want pass (status=%d)", name, recorder.Code)
		} else if recorder.Code != 200 {
			t.Errorf("%s: pass but status written = %d", name, recorder.Code)
		}
	}
	reject := func(name string, request *http.Request, keyAuthEnabled bool, trusted string) {
		t.Helper()
		recorder := httptest.NewRecorder()
		if GuardForwardBrowserOrigin(recorder, request, keyAuthEnabled, trusted) || recorder.Code != http.StatusForbidden {
			t.Errorf("%s: status=%d allowed=%v, want 403 reject", name, recorder.Code, recorder.Code == 200)
		}
	}
	forwardRequest := func(origin, site, host string) *http.Request {
		request := httptest.NewRequest(http.MethodPost, "http://proxy.invalid/v1/messages", strings.NewReader(`{}`))
		if origin != "" {
			request.Header.Set("Origin", origin)
		}
		if site != "" {
			request.Header.Set("Sec-Fetch-Site", site)
		}
		if host != "" {
			request.Host = host
		}
		return request
	}

	// Non-browser clients never see this check, whatever their Host.
	pass("cli client", forwardRequest("", "", "example.com:443"), false, "")
	// Same-machine browser tool: loopback origin, different port than the proxy.
	pass("loopback browser tool", forwardRequest("http://localhost:3000", "", "127.0.0.1:8425"), false, "")
	pass("ipv6 loopback origin", forwardRequest("http://[::1]:3000", "", "[::1]:8425"), false, "")
	// Same-origin browser (UI-style) fetch.
	pass("same-origin browser", forwardRequest("http://127.0.0.1:8425", "", "127.0.0.1:8425"), false, "")
	// LAN deployment on the configured listen: browser from the same address.
	pass("configured listen origin", forwardRequest("http://100.81.1.4:8425", "", "100.81.1.4:8425"), false, "100.81.1.4:8425")

	// Cross-site page on the attacker's own site: the LLM-CSRF shape.
	reject("cross-site page", forwardRequest("http://evil.example", "cross-site", "127.0.0.1:8425"), false, "")
	reject("cross-site without origin", forwardRequest("", "cross-site", "127.0.0.1:8425"), false, "")
	// A browser page served from another machine on the LAN.
	reject("lan origin", forwardRequest("http://192.168.1.5:8080", "", "127.0.0.1:8425"), false, "")
	// DNS rebinding: page origin equals the attacker hostname that now resolves
	// here; only the Host check sees through it.
	reject("dns rebinding", forwardRequest("http://attacker.rebound:8425", "", "attacker.rebound:8425"), false, "")
	// "null" origin (sandboxed frame) is not a loopback identity either.
	reject("null origin", forwardRequest("null", "", "127.0.0.1:8425"), false, "")

	// Authenticated callers presented a key: they pass the origin/Host shape
	// checks (a LAN-served browser tool holding a key is legitimate). The
	// cross-site rejection above stays unconditional — no legitimate browser
	// client of this proxy is ever cross-site, key or no key (a cross-site
	// page cannot attach the key header without a preflight this proxy never
	// grants).
	pass("key-authed browser", forwardRequest("http://192.168.1.5:8080", "", "attacker.rebound:8425"), true, "")
	reject("key-authed cross-site still rejected", forwardRequest("http://evil.example", "cross-site", "127.0.0.1:8425"), true, "")
}
