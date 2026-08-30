package app

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// webauth_forward_test.go pins the S2 forward-surface auth behavior at the
// proxy handler: with web.auth.api_keys_file configured, protocol endpoints
// (and /v1/models) reject requests without a listed key; /health stays open
// for liveness probes; admin endpoints riding the proxy handler (web
// disabled) honor the admin token file. With no auth configured the
// loopback-trust default is untouched.

// newAuthForwardProxy builds a proxy with api-key auth enabled over a token
// file containing sk-test-key.
func newAuthForwardProxy(t *testing.T) *Proxy {
	t.Helper()
	dir := t.TempDir()
	keysFile := filepath.Join(dir, "api_keys")
	if err := os.WriteFile(keysFile, []byte("# team keys\nsk-test-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	adminFile := filepath.Join(dir, "admin_token")
	if err := os.WriteFile(adminFile, []byte("adm-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		Listen:    "192.0.2.10:8123",
		Providers: map[string]Provider{"a": {OpenAIBaseURL: "http://127.0.0.1:1", Provider: "a", Models: []string{"m"}}},
		Web:       WebConfig{Auth: WebAuthConfig{AdminTokenFile: adminFile, APIKeysFile: keysFile}},
	}
	p := newTestProxy(t, cfg)
	return p
}

func TestForwardAuthRejectsMissingOrWrongKey(t *testing.T) {
	p := newAuthForwardProxy(t)
	srv := httptest.NewServer(http.HandlerFunc(p.Handler))
	t.Cleanup(srv.Close)

	// No key → 401 before any upstream attempt.
	res, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no key = %d, want 401", res.StatusCode)
	}
	// Wrong key → 401.
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("Authorization", "Bearer sk-wrong")
	res2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res2.Body.Close()
	if res2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong key = %d, want 401", res2.StatusCode)
	}
	// Right key passes the gate (upstream is unreachable: any non-401 proves
	// the auth layer let it through to routing).
	req2, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	req2.Header.Set("Authorization", "Bearer sk-test-key")
	res3, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	res3.Body.Close()
	if res3.StatusCode == http.StatusUnauthorized {
		t.Fatal("valid key rejected by the auth gate")
	}
	// x-api-key (Anthropic convention) is accepted symmetrically.
	req3, _ := http.NewRequest("POST", srv.URL+"/v1/messages", strings.NewReader(`{"model":"claude-x"}`))
	req3.Header.Set("x-api-key", "sk-test-key")
	res4, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatal(err)
	}
	res4.Body.Close()
	if res4.StatusCode == http.StatusUnauthorized {
		t.Fatal("x-api-key form rejected by the auth gate")
	}
}

func TestForwardAuthHealthStaysOpenAndModelsGated(t *testing.T) {
	p := newAuthForwardProxy(t)
	srv := httptest.NewServer(http.HandlerFunc(p.Handler))
	t.Cleanup(srv.Close)

	res, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("/health = %d, want 200 (liveness probes bypass auth)", res.StatusCode)
	}
	res2, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	res2.Body.Close()
	if res2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/v1/models without key = %d, want 401", res2.StatusCode)
	}
}

func TestForwardAuthAdminEndpointsRidingProxyHandler(t *testing.T) {
	p := newAuthForwardProxy(t)
	srv := httptest.NewServer(http.HandlerFunc(p.Handler))
	t.Cleanup(srv.Close)

	// /debug/schedule without the admin token → 401 (this endpoint normally
	// rides the proxy handler only when web.enabled=false).
	res, err := http.Get(srv.URL + "/debug/schedule")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/debug/schedule without admin token = %d, want 401", res.StatusCode)
	}
	req, _ := http.NewRequest("GET", srv.URL+"/debug/schedule", nil)
	req.Header.Set("Authorization", "Bearer adm-1")
	res2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res2.Body.Close()
	if res2.StatusCode == http.StatusUnauthorized {
		t.Fatal("/debug/schedule with valid admin token rejected")
	}
}

func TestForwardAuthAdminDebugEndpointsAllowLANAndRejectRebinding(t *testing.T) {
	p := newAuthForwardProxy(t)
	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{name: "schedule", method: http.MethodGet, path: "/debug/schedule"},
		{name: "route", method: http.MethodPost, path: "/debug/route", body: `{"model":"m","messages":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := func(host string) *http.Request {
				req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
				req.Host = host
				req.Header.Set("Origin", "http://"+host)
				req.Header.Set("Authorization", "Bearer adm-1")
				return req
			}
			allowed := httptest.NewRecorder()
			p.Handler(allowed, request("192.0.2.10:8123"))
			if allowed.Code != http.StatusOK {
				t.Fatalf("LAN request = %d body=%q, want 200", allowed.Code, allowed.Body.String())
			}

			rebound := httptest.NewRecorder()
			p.Handler(rebound, request("attacker.rebound:8123"))
			if rebound.Code != http.StatusForbidden {
				t.Fatalf("rebound request = %d body=%q, want 403", rebound.Code, rebound.Body.String())
			}
		})
	}
}

func TestForwardAuthDisabledKeepsLoopbackTrust(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{"a": {OpenAIBaseURL: "http://127.0.0.1:1", Provider: "a", Models: []string{"m"}}},
	}
	p := newTestProxy(t, cfg)
	srv := httptest.NewServer(http.HandlerFunc(p.Handler))
	t.Cleanup(srv.Close)

	res, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("auth off: /v1/models = %d, want 200 (loopback-trust default)", res.StatusCode)
	}
}
