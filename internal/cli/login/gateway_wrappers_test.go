package login

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// Covers the thin production wrappers the web login flow relies on:
// NewAqpClientWithBase must produce a working client against its base URL
// (BootstrapLoginURL hits Base + AqpAuthLoginPath), and PoolPath must resolve
// the plural pool file under the caller's HOME.
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

	client := NewAqpClientWithBase(filepath.Join(home, "google_oauth_auth.json"), aqp.URL)
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
