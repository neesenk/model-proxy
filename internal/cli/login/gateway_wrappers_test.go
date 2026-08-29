package login

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Covers the thin production wrappers the web login flow relies on:
// NewAqpClient with a test Base override must produce a working client against
// that base URL (BootstrapLoginURL hits Base + AqpAuthLoginPath), and PoolPath
// must resolve the plural pool file under the caller's HOME.
func TestAqpClientWithBaseBootstrapsAndPoolPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	aqp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != AqpAuthLoginPath {
			http.NotFound(w, r)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "SSO_A", Value: "anon-key"})
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"status":401,"result":"http://soup.test/login/google_login?anonymous_sso_key=anon-key&next="}`)
	}))
	defer aqp.Close()

	client := NewAqpClient(filepath.Join(home, "google_oauth_auth.json"))
	client.Base = aqp.URL
	if client.Base != aqp.URL {
		t.Fatalf("Base = %q, want %q", client.Base, aqp.URL)
	}
	loginURL, err := client.BootstrapLoginURL()
	if err != nil {
		t.Fatalf("bootstrap via base: %v", err)
	}
	if !strings.Contains(loginURL, "google_login") || !strings.Contains(loginURL, "next=") {
		t.Fatalf("loginURL = %q", loginURL)
	}

	want := filepath.Join(home, ".model-proxy", "zhipu_apikeys.json")
	if got := PoolPath("zhipu"); got != want {
		t.Errorf("PoolPath = %q, want %q", got, want)
	}
}

// Covers the convenience wrappers end-to-end against one stub gateway:
// PollSession (through PollSessionContext → the Base-relative auth/info path),
// CheckSessionAt, and FetchAPIKey (through FetchAPIKeyContext, with the
// session cookie flowing from the poll's jar).
func TestAqpConvenienceWrappersRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	aqp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case AqpAuthInfoPath:
			http.SetCookie(w, &http.Cookie{Name: "SSO_C", Value: "sess", Path: "/"})
			fmt.Fprint(w, `{"retcode":0,"data":{"user":{"userid":7,"email":"dev@example.test","is_active":true}}}`)
		case AqpAPIKeyGetGenPath:
			if r.Method != http.MethodPost {
				http.NotFound(w, r)
				return
			}
			fmt.Fprint(w, `{"retcode":0,"data":{"generated":true,"api_key":"key-1"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer aqp.Close()

	client := NewAqpClient(filepath.Join(home, "google_oauth_auth.json"))
	client.Base = aqp.URL
	authed, err := client.PollSession(2 * time.Second)
	if err != nil {
		t.Fatalf("PollSession: %v", err)
	}
	if authed.EmployeeEmail != "dev@example.test" || !authed.HasAccess {
		t.Fatalf("PollSession data = %+v", authed)
	}
	if again, err := client.CheckSessionAt(aqp.URL + AqpAuthInfoPath); err != nil || again.EmployeeEmail != "dev@example.test" {
		t.Fatalf("CheckSessionAt = (%+v, %v)", again, err)
	}
	key, err := client.FetchAPIKey()
	if err != nil {
		t.Fatalf("FetchAPIKey: %v", err)
	}
	if key.APIKey != "key-1" || !key.Generated {
		t.Fatalf("FetchAPIKey data = %+v", key)
	}
}

// WithNextCallback appends the loopback callback as the login URL's next
// parameter; a malformed URL passes through unchanged.
func TestWithNextCallback(t *testing.T) {
	got := WithNextCallback("http://sso.test/login?x=1", "http://127.0.0.1:1/cb")
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if u.Query().Get("next") != "http://127.0.0.1:1/cb" || u.Query().Get("x") != "1" {
		t.Fatalf("next not appended: %q", got)
	}
	if got := WithNextCallback("://bad url", "http://cb"); got != "://bad url" {
		t.Fatalf("malformed URL not passed through: %q", got)
	}
}

// WaitForLoginSignal surfaces the deadline as the user-facing timeout error.
func TestWaitForLoginSignalTimeout(t *testing.T) {
	ls, err := NewLoopbackServer()
	if err != nil {
		t.Fatal(err)
	}
	defer ls.Stop()
	if err := WaitForLoginSignal(ls, 10*time.Millisecond); err == nil ||
		!strings.Contains(err.Error(), "timed out") {
		t.Fatalf("WaitForLoginSignal timeout error = %v", err)
	}
}
