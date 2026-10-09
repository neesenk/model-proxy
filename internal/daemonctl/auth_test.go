package daemonctl

// auth_test.go — the S2 CLI-side credential attach: path → surface token,
// explicit caller headers win, unconfigured files attach nothing (the daemon
// answers the honest 401).

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func writeTokenFile(t *testing.T, tokens ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tokens.txt")
	if err := os.WriteFile(path, []byte("# comment\n\n"+joinLines(tokens)), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func joinLines(lines []string) string {
	out := ""
	for _, l := range lines {
		out += l + "\n"
	}
	return out
}

func TestAuthorizePicksSurfaceCredentialByPath(t *testing.T) {
	admin := writeTokenFile(t, "admin-tok")
	keys := writeTokenFile(t, "key-one", "key-two")
	SetAuthFiles(admin, keys)
	t.Cleanup(func() { SetAuthFiles("", "") })

	cases := []struct {
		path string
		want string
	}{
		{"/api/status", "Bearer admin-tok"},
		{"/api/requests/x", "Bearer admin-tok"},
		{"/metrics", "Bearer admin-tok"},
		{"/debug/route", "Bearer admin-tok"},
		{"/v1/messages", "Bearer key-one"},
		{"/v1/models", "Bearer key-one"},
		{"/mcp/web-search", "Bearer key-one"},
		{"/health", ""},
	}
	for _, tc := range cases {
		req, err := NewRequest(http.MethodGet, "http://127.0.0.1:1", tc.path, nil)
		if err != nil {
			t.Fatalf("NewRequest %s: %v", tc.path, err)
		}
		if got := req.Header.Get("Authorization"); got != tc.want {
			t.Errorf("path %s: Authorization = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestAuthorizeNeverOverridesCallerHeader(t *testing.T) {
	SetAuthFiles(writeTokenFile(t, "admin-tok"), writeTokenFile(t, "key"))
	t.Cleanup(func() { SetAuthFiles("", "") })

	req, _ := NewRequest(http.MethodGet, "http://127.0.0.1:1", "/api/status", nil)
	req.Header.Set("Authorization", "Bearer caller-own")
	Authorize(req) // idempotent path: a second pass must not clobber it either
	if got := req.Header.Get("Authorization"); got != "Bearer caller-own" {
		t.Errorf("Authorization = %q, want the caller's own header", got)
	}

	xkey, _ := NewRequest(http.MethodGet, "http://127.0.0.1:1", "/v1/messages", nil)
	xkey.Header.Set("x-api-key", "caller-key")
	if got := xkey.Header.Get("x-api-key"); got != "caller-key" {
		t.Errorf("x-api-key = %q, want the caller's own key", got)
	}
}

func TestAuthorizeUnconfiguredAttachesNothing(t *testing.T) {
	SetAuthFiles("", "")
	req, _ := NewRequest(http.MethodGet, "http://127.0.0.1:1", "/api/status", nil)
	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("unconfigured auth attached %q, want none", got)
	}
}

func TestAuthorizeMissingFileAttachesNothing(t *testing.T) {
	// A configured-but-missing file is fail-closed on the serving side; the
	// CLI half must send nothing rather than a guessable placeholder.
	SetAuthFiles(filepath.Join(t.TempDir(), "missing-admin.txt"), filepath.Join(t.TempDir(), "missing-keys.txt"))
	t.Cleanup(func() { SetAuthFiles("", "") })
	for _, path := range []string{"/api/status", "/v1/messages"} {
		req, _ := NewRequest(http.MethodGet, "http://127.0.0.1:1", path, nil)
		if got := req.Header.Get("Authorization"); got != "" {
			t.Errorf("path %s with missing files attached %q, want none (daemon 401s)", path, got)
		}
	}
}

// TestDoAttachesCredentialEndToEnd: the shared Do path carries the admin
// token to a real server (the wiring LoadCmdConfig → SetAuthFiles → Do is
// what keeps `status`/`doctor --live`/… working on a non-loopback daemon).
func TestDoAttachesCredentialEndToEnd(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	SetAuthFiles(writeTokenFile(t, "admin-tok"), "")
	t.Cleanup(func() { SetAuthFiles("", "") })

	if _, status, err := Do(http.MethodGet, srv.URL, "/api/status"); err != nil || status != 200 {
		t.Fatalf("Do: status=%d err=%v", status, err)
	}
	if gotAuth != "Bearer admin-tok" {
		t.Errorf("daemon saw Authorization %q, want Bearer admin-tok", gotAuth)
	}
}

// TestRequestHelpersCarryCredential: the three manual-request helpers
// (PostJSON/NewGet/DoRaw) are the call-site migration surface for commands
// that need the raw response stream — each must carry the same path-keyed
// credential as Do/Get.
func TestRequestHelpersCarryCredential(t *testing.T) {
	var gotPinAuth, gotListAuth, gotDeleteAuth string
	var gotMethod string
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/pin":
			if r.Method == http.MethodDelete {
				gotDeleteAuth = r.Header.Get("Authorization")
				break
			}
			gotMethod = r.Method
			gotBody = r.Header.Get("content-type")
			gotPinAuth = r.Header.Get("Authorization")
		case "/api/pin/list":
			gotListAuth = r.Header.Get("Authorization")
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	SetAuthFiles(writeTokenFile(t, "admin-tok"), "")
	t.Cleanup(func() { SetAuthFiles("", "") })

	resp, err := PostJSON(srv.URL, "/api/pin", []byte(`{"route":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotPinAuth != "Bearer admin-tok" || gotMethod != "POST" || gotBody != "application/json" {
		t.Errorf("PostJSON: auth=%q method=%q ct=%q", gotPinAuth, gotMethod, gotBody)
	}

	req, err := NewGet(srv.URL, "/api/pin/list")
	if err != nil {
		t.Fatal(err)
	}
	resp, _ = Client.Do(req)
	resp.Body.Close()
	if gotListAuth != "Bearer admin-tok" {
		t.Errorf("NewGet: auth = %q, want Bearer admin-tok", gotListAuth)
	}

	dreq, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/pin?route=x", nil)
	dresp, err := DoRaw(dreq)
	if err != nil {
		t.Fatal(err)
	}
	dresp.Body.Close()
	if gotDeleteAuth != "Bearer admin-tok" {
		t.Errorf("DoRaw: auth = %q, want Bearer admin-tok", gotDeleteAuth)
	}
}
