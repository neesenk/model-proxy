package app

import (
	"encoding/json"
	"model-proxy/internal/appapi"
	"model-proxy/internal/observe/seclog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// ---- security_web_api_test.go ----

// writeSecLogFile seeds one audit-log file (plus any raw extra lines) in dir.
func writeSecLogFile(t *testing.T, dir string, extraLines []string, records ...seclog.Record) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(filepath.Join(dir, "security-20260101-000000.log"))
	if err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(file)
	for _, record := range records {
		if err := encoder.Encode(record); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
	}
	for _, line := range extraLines {
		if _, err := file.WriteString(line + "\n"); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func serveSecurity(t *testing.T, w *WebServer, path string) (int, appapi.SecurityResult) {
	t.Helper()
	mux := http.NewServeMux()
	w.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	var result appapi.SecurityResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("parse /api/security response %q: %v", rec.Body.String(), err)
	}
	return rec.Code, result
}

// TestAPISecurityProjection: /api/security projects seclog records into DTOs
// (newest first, unreadable lines counted in skipped) and passes the kind
// filter through to the audit query.
func TestAPISecurityProjection(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeSecLogFile(t, filepath.Join(home, ".model-proxy", "log", "security"), []string{"not-json"},
		seclog.Record{Ts: 1700000000000, Kind: "secret", RequestID: "r1", Agent: "codex", Protocol: "anthropic", Exposed: "gpt-x", Names: []string{"aws-access-key"}, Action: "blocked"},
		seclog.Record{Ts: 1700000001000, Kind: "path", Agent: "pi", Exposed: "gpt-x", Names: []string{"home-outside-root"}, Action: "warn", Detail: "outside allowed roots"},
		seclog.Record{Ts: 1700000002000, Kind: "drift", Agent: "codex", Exposed: "gpt-x", Names: []string{"credential-shape"}, Action: "warn"},
	)
	w, _ := newTestWeb(t)

	code, all := serveSecurity(t, w, "/api/security")
	if code != http.StatusOK || !all.Enabled || all.Skipped != 1 || len(all.Records) != 3 {
		t.Fatalf("all = (%d, %#v)", code, all)
	}
	// Newest first, and every DTO field projects from the record.
	first := all.Records[0]
	if first.Kind != "drift" || first.Ts != 1700000002000 {
		t.Fatalf("newest-first order broken: %#v", all.Records)
	}
	secret := all.Records[2]
	wantSecret := appapi.SecurityRecord{Ts: 1700000000000, Kind: "secret", RequestID: "r1", Agent: "codex", Protocol: "anthropic", Exposed: "gpt-x", Names: []string{"aws-access-key"}, Action: "blocked"}
	if !reflect.DeepEqual(secret, wantSecret) {
		t.Fatalf("secret DTO = %#v, want %#v", secret, wantSecret)
	}
	if all.Records[1].Detail != "outside allowed roots" {
		t.Fatalf("path DTO detail = %#v", all.Records[1])
	}

	code, filtered := serveSecurity(t, w, "/api/security?kind=secret")
	if code != http.StatusOK || len(filtered.Records) != 1 || filtered.Records[0].Kind != "secret" {
		t.Fatalf("kind filter = (%d, %#v)", code, filtered)
	}

	code, limited := serveSecurity(t, w, "/api/security?limit=2")
	if code != http.StatusOK || len(limited.Records) != 2 || limited.Records[0].Kind != "drift" || limited.Records[1].Kind != "path" {
		t.Fatalf("limit = (%d, %#v)", code, limited)
	}
}

// TestAPISecurityDisabledOrMissing: guard.audit off — or the audit directory
// simply absent — yields the request-log-style disabled response instead of
// an error.
func TestAPISecurityDisabledOrMissing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg, _ := LoadConfigFromBytes("test", []byte(`listen: 127.0.0.1:0
providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://x}
guard: {audit: false}
`))
	w := NewWebServer(newTestProxy(t, cfg), "test-config.yaml")
	code, off := serveSecurity(t, w, "/api/security")
	if code != http.StatusOK || off.Enabled || off.Records == nil || len(off.Records) != 0 || off.Skipped != 0 {
		t.Fatalf("audit off = (%d, %#v)", code, off)
	}

	// Audit on (default) but the directory was never created.
	w2, _ := newTestWeb(t)
	code, missing := serveSecurity(t, w2, "/api/security")
	if code != http.StatusOK || missing.Enabled || missing.Records == nil || len(missing.Records) != 0 {
		t.Fatalf("missing dir = (%d, %#v)", code, missing)
	}
}

// ---- webauth_forward_test.go ----

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
