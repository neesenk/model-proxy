package login

import (
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"model-proxy/internal/provider"
)

// TestLogin_FullFlowWithMockAQP drives the entire real SSO flow shape
// (bootstrap → loopback signal → poll → get_or_generate) against a mock Aqp
// backend that mimics the real /auth/login 401 + /auth/info 200 contract.
func TestLogin_FullFlowWithMockAqp(t *testing.T) {
	cookieVal := "fake-sso-c-cookie-value"

	var infoCalls int
	aqp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/compass-api/v1/auth/login":
			// Bootstrap: 401 + SSO_A cookie + result login URL.
			http.SetCookie(w, &http.Cookie{Name: "SSO_A", Value: "anon-key"})
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"status":401,"result":"http://soup.test/login/google_login?anonymous_sso_key=anon-key&next="}`)
		case "/compass-api/v1/auth/info":
			infoCalls++
			// Reject until SSO_C is set (post-login); after the loopback fires we
			// simulate the authenticated state by setting SSO_C.
			hasC := false
			for _, c := range r.Cookies() {
				if c.Name == "SSO_C" {
					hasC = true
				}
			}
			if !hasC && infoCalls < 2 {
				// First poll: emulate "login done" by setting SSO_C now.
				http.SetCookie(w, &http.Cookie{Name: "SSO_C", Value: cookieVal})
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "SSO_C", Value: cookieVal})
			w.Header().Set("content-type", "application/json")
			fmt.Fprint(w, `{"retcode":0,"message":"ok","data":{"user":{"userid":1,"email":"tester@shopee.io","is_active":true}}}`)
		case "/api/v1/cqp/ccswitch/api_key/get_or_generate":
			// fetchAPIKeyAt must set the SSO cookie explicitly (the jar's cookies
			// are scoped to /compass-api and don't match this path) — deleting
			// that header would 401 in production.
			if got := r.Header.Get("Cookie"); got != "SSO_C="+cookieVal {
				t.Errorf("get_or_generate Cookie=%q, want %q", got, "SSO_C="+cookieVal)
			}
			w.Header().Set("content-type", "application/json")
			fmt.Fprint(w, `{"retcode":0,"data":{"generated":true,"api_key":"managed-aqp-key-xyz","quota_type":"enterprise","project_id":"proj-123","employee_email":"tester@shopee.io","employee_user_id":"1","employee_role":"engineer","business_name":"Shopee"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer aqp.Close()

	storePath := t.TempDir() + "/google_oauth_auth.json"
	jar, _ := cookiejar.New(nil)
	c := &AqpClient{
		HTTP:      &http.Client{Jar: jar},
		Jar:       jar,
		StorePath: storePath,
	}

	// 1. Bootstrap: get the login URL (401 + result).
	loginURL, err := c.BootstrapAt(aqp.URL + "/compass-api/v1/auth/login")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if !strings.Contains(loginURL, "google_login") || !strings.Contains(loginURL, "next=") {
		t.Fatalf("loginURL=%q", loginURL)
	}

	// 2. Loopback: simulate the browser hitting the callback after login.
	ls, err := NewLoopbackServer()
	if err != nil {
		t.Fatal(err)
	}
	if err := ls.Start(); err != nil {
		t.Fatal(err)
	}
	defer ls.Stop()
	go func() {
		req, _ := http.NewRequest("GET", ls.CallbackURL(), nil)
		resp, err := c.HTTP.Do(req)
		if err != nil {
			t.Logf("callback request: %v", err)
			return
		}
		resp.Body.Close()
	}()
	if _, err := ls.WaitForCookie(5 * time.Second); err != nil {
		t.Fatalf("wait callback: %v", err)
	}

	// 3. Poll session (jar carries SSO_A; mock upgrades to SSO_C).
	data, err := c.PollAt(aqp.URL+"/compass-api/v1/auth/info", 10*time.Second)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if data.EmployeeEmail != "tester@shopee.io" {
		t.Errorf("data=%+v", data)
	}

	// 4. Persist + fetch API key.
	a := &provider.AqpAccountData{
		AccountID:        data.EmployeeEmail,
		Email:            data.EmployeeEmail,
		SSOSessionCookie: "SSO_C=" + cookieVal,
	}
	if err := provider.SaveAqpAccount(storePath, a); err != nil {
		t.Fatal(err)
	}
	key, err := c.FetchAPIKeyAt(aqp.URL + "/api/v1/cqp/ccswitch/api_key/get_or_generate")
	if err != nil {
		t.Fatalf("fetch key: %v", err)
	}
	if key.APIKey != "managed-aqp-key-xyz" {
		t.Errorf("key=%v", key.APIKey)
	}
	if key.ProjectID != "proj-123" {
		t.Errorf("project=%q", key.ProjectID)
	}

	// 5. Managed key is NOT persisted; the store keeps the account fields.
	loaded, err := provider.LoadAqpAccount(storePath)
	if err != nil {
		t.Fatalf("reload account store: %v", err)
	}
	if loaded.Email != "tester@shopee.io" || loaded.SSOSessionCookie != "SSO_C="+cookieVal {
		t.Errorf("persisted account fields: %+v", loaded)
	}
}

// TestBootstrap_MissingLoginURL verifies the error when the body has no result.
func TestBootstrap_MissingLoginURL(t *testing.T) {
	aqp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"something":"else"}`)
	}))
	defer aqp.Close()
	jar, _ := cookiejar.New(nil)
	c := &AqpClient{HTTP: &http.Client{Jar: jar}, Jar: jar, StorePath: t.TempDir() + "/g.json"}
	_, err := c.BootstrapAt(aqp.URL + "/compass-api/v1/auth/login")
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "missing login url") {
		t.Errorf("err=%v", err)
	}
}

// TestExtractLoginURL_Coverage covers field-name variants and nested data.
func TestExtractLoginURL_Coverage(t *testing.T) {
	cases := []struct{ body, want string }{
		{`{"result":"http://x/login"}`, "http://x/login"},
		{`{"login_url":"http://x/u"}`, "http://x/u"},
		{`{"data":{"url":"http://x/d"}}`, "http://x/d"},
		{`{"foo":"bar"}`, ""},
		{`not json`, ""},
	}
	for _, tc := range cases {
		if got := ExtractLoginURL(tc.body); got != tc.want {
			t.Errorf("ExtractLoginURL(%q)=%q want %q", tc.body, got, tc.want)
		}
	}
}

// TestCookieHeader verifies both the raw-value and prefixed storage forms.
func TestCookieHeader(t *testing.T) {
	if got := provider.CookieHeader("abc"); got != "SSO_C=abc" {
		t.Errorf("bare value: got %q", got)
	}
	if got := provider.CookieHeader("SSO_C=abc"); got != "SSO_C=abc" {
		t.Errorf("prefixed: got %q", got)
	}
	if got := provider.CookieHeader(""); got != "" {
		t.Errorf("empty: got %q", got)
	}
}

// TestLoopbackServer_RejectsForeignHost verifies the callback origin check
// rejects non-loopback hosts with 403 — and, critically, that the rejection
// does NOT abort the login flow: any local process can hit the port with a
// crafted Host header, and writing the error to ErrCh used to fail the whole
// SSO login on that probe. After the hostile request the server must still
// complete a genuine callback.
func TestLoopbackServer_RejectsForeignHost(t *testing.T) {
	ls, err := NewLoopbackServer()
	if err != nil {
		t.Fatal(err)
	}
	if err := ls.Start(); err != nil {
		t.Fatal(err)
	}
	defer ls.Stop()
	// Build a request with a foreign Host directly (bypassing DNS).
	req, _ := http.NewRequest("GET", ls.CallbackURL(), nil)
	req.Host = "evil.example.com"
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatalf("foreign Host request failed before callback validation: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("foreign host status=%d want %d", resp.StatusCode, http.StatusForbidden)
	}
	select {
	case err := <-ls.ErrCh:
		t.Fatalf("origin rejection must not abort the login flow (ErrCh got %v)", err)
	default:
	}
	// The login flow keeps waiting and a genuine callback still completes.
	good, err := http.NewRequest("GET", ls.CallbackURL(), nil)
	if err != nil {
		t.Fatal(err)
	}
	gresp, err := http.DefaultTransport.RoundTrip(good)
	if err != nil {
		t.Fatalf("genuine callback request failed: %v", err)
	}
	gresp.Body.Close()
	if gresp.StatusCode != http.StatusOK {
		t.Errorf("genuine callback status=%d want %d", gresp.StatusCode, http.StatusOK)
	}
	if _, err := ls.WaitForCookie(time.Second); err != nil {
		t.Fatalf("login did not complete after hostile probe: %v", err)
	}
}
