package login

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"model-proxy/internal/provider"
)

// TestRequestUserCode verifies the usercode request format + response parsing.
func TestRequestUserCode(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 1024)
		n, _ := r.Body.Read(b)
		gotBody = string(b[:n])
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"device_auth_id":"daid-1","user_code":"ABC-123","interval":"5"}`)
	}))
	defer srv.Close()
	opts := &CodexLoginServerOptions{UsercodeURL: srv.URL, HTTPClient: &http.Client{Timeout: 5 * time.Second}}
	uc, err := RequestUserCode(opts, provider.CodexOAuthClientID)
	if err != nil {
		t.Fatal(err)
	}
	if uc.DeviceAuthID != "daid-1" || uc.UserCode != "ABC-123" || uc.Interval != "5" {
		t.Errorf("parsed=%+v", uc)
	}
	var req struct {
		ClientID string `json:"client_id"`
	}
	json.Unmarshal([]byte(gotBody), &req)
	if req.ClientID != provider.CodexOAuthClientID {
		t.Errorf("client_id=%q", req.ClientID)
	}
}

// TestPollForToken_PendingThenSuccess verifies polling waits through pending
// and returns on success.
func TestPollForToken_PendingThenSuccess(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&n, 1)
		if n < 3 {
			w.WriteHeader(400)
			// Production-real nested error shape (real OpenAI), not the flat
			// test-mock one — exercises the nested parse branch.
			fmt.Fprint(w, `{"error":{"code":"deviceauth_authorization_pending"}}`)
			return
		}
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"authorization_code":"authcode-1","code_challenge":"cc","code_verifier":"cv-1"}`)
	}))
	defer srv.Close()
	opts := &CodexLoginServerOptions{DeviceTokURL: srv.URL, HTTPClient: &http.Client{Timeout: 5 * time.Second}}
	cs, err := PollForToken(opts, "daid", "uc", 0) // interval=0 → defaults to 5
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if cs.AuthorizationCode != "authcode-1" || cs.CodeVerifier != "cv-1" {
		t.Errorf("got %+v", cs)
	}
}

// TestPollForToken_SlowDownIncreasesInterval verifies slow_down bumps interval.
func TestPollForToken_SlowDownThenSuccess(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := atomic.AddInt32(&n, 1)
		if c == 1 {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":"slow_down"}`)
			return
		}
		fmt.Fprint(w, `{"authorization_code":"a","code_verifier":"v"}`)
	}))
	defer srv.Close()
	opts := &CodexLoginServerOptions{DeviceTokURL: srv.URL, HTTPClient: &http.Client{Timeout: 5 * time.Second}}
	_, err := PollForToken(opts, "d", "u", 0)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
}

// TestPollForToken_AccessDenied verifies error propagation.
func TestPollForToken_AccessDenied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		// Production-real nested shape; the denial must surface as such (not a
		// confused "expired" message — the two branches are easy to swap).
		fmt.Fprint(w, `{"error":{"code":"deviceauth_authorization_denied"}}`)
	}))
	defer srv.Close()
	opts := &CodexLoginServerOptions{DeviceTokURL: srv.URL, HTTPClient: &http.Client{Timeout: 5 * time.Second}}
	_, err := PollForToken(opts, "d", "u", 0)
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("expected denial error, got %v", err)
	}
}

// TestExchangeCodeForTokens verifies the token exchange request + response.
func TestExchangeCodeForTokens(t *testing.T) {
	var gotForm string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotForm = r.FormValue("grant_type") + "|" + r.FormValue("code") + "|" +
			r.FormValue("redirect_uri") + "|" + r.FormValue("client_id") + "|" +
			r.FormValue("code_verifier")
		fmt.Fprint(w, `{"access_token":"at","refresh_token":"rt","id_token":"it"}`)
	}))
	defer srv.Close()
	opts := &CodexLoginServerOptions{TokenURL: srv.URL, HTTPClient: &http.Client{Timeout: 5 * time.Second}}
	af, err := ExchangeCodeForTokens(opts, provider.CodexOAuthClientID, "authcode", "verifier")
	if err != nil {
		t.Fatal(err)
	}
	want := "authorization_code|authcode|" + CodexOAuthCallback + "|" + provider.CodexOAuthClientID + "|verifier"
	if gotForm != want {
		t.Errorf("form=%q want %q", gotForm, want)
	}
	if af.Tokens.AccessToken != "at" || af.Tokens.RefreshToken != "rt" || af.Tokens.IDToken != "it" {
		t.Errorf("tokens=%+v", af.Tokens)
	}
}

// TestRequestUserCode_MissingFieldsNoBodyEcho (C-2 regression): a 200 body
// missing device_auth_id/user_code must produce a keys-only error naming the
// missing fields; the raw body — which may carry credential fragments from a
// malformed response — must never enter the error string.
func TestRequestUserCode_MissingFieldsNoBodyEcho(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"user_code":"","device_auth_id":"","leaked":"SECRET-FRAGMENT-XYZ"}`)
	}))
	defer srv.Close()
	opts := &CodexLoginServerOptions{UsercodeURL: srv.URL, HTTPClient: &http.Client{Timeout: 5 * time.Second}}
	_, err := RequestUserCode(opts, provider.CodexOAuthClientID)
	if err == nil {
		t.Fatal("missing fields: want error, got nil")
	}
	if !strings.Contains(err.Error(), "missing device_auth_id,user_code") {
		t.Errorf("error %q must name the missing fields", err)
	}
	if !strings.Contains(err.Error(), "keys: device_auth_id,leaked,user_code") {
		t.Errorf("error %q must list the response keys", err)
	}
	if strings.Contains(err.Error(), "SECRET-FRAGMENT-XYZ") {
		t.Errorf("error must not echo the 200 body: %q", err)
	}
}

// TestExchangeCodeForTokens_MissingAccessTokenNoBodyEcho (C-2 regression): a
// 200 token response missing access_token may still carry a refresh_token
// fragment; the error must report the missing field + response keys only. The
// non-200 branch KEEPS its (truncated) body echo — that body is an error page,
// not a token payload, and carries the diagnostic value.
func TestExchangeCodeForTokens_MissingAccessTokenNoBodyEcho(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"refresh_token":"rt-SECRET-FRAGMENT","id_token":"it"}`)
	}))
	defer srv.Close()
	opts := &CodexLoginServerOptions{TokenURL: srv.URL, HTTPClient: &http.Client{Timeout: 5 * time.Second}}
	_, err := ExchangeCodeForTokens(opts, provider.CodexOAuthClientID, "authcode", "verifier")
	if err == nil {
		t.Fatal("missing access_token: want error, got nil")
	}
	const want = "token response missing access_token (keys: id_token,refresh_token)"
	if err.Error() != want {
		t.Errorf("error=%q want exact %q", err, want)
	}
	if strings.Contains(err.Error(), "rt-SECRET-FRAGMENT") {
		t.Errorf("error must not echo the 200 body: %q", err)
	}
}

// TestExchangeCodeForTokens_Non200KeepsBodyEcho pins the retained non-200
// behavior: an error page's (truncated) body stays in the message.
func TestExchangeCodeForTokens_Non200KeepsBodyEcho(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(502)
		fmt.Fprint(w, "upstream auth broker broken")
	}))
	defer srv.Close()
	opts := &CodexLoginServerOptions{TokenURL: srv.URL, HTTPClient: &http.Client{Timeout: 5 * time.Second}}
	_, err := ExchangeCodeForTokens(opts, provider.CodexOAuthClientID, "authcode", "verifier")
	if err == nil || !strings.Contains(err.Error(), "upstream auth broker broken") {
		t.Errorf("non-200 error must keep the body echo, got %v", err)
	}
}

// TestAccountIDFromTokens verifies id_token JWT parsing for account_id.
func TestAccountIDFromTokens(t *testing.T) {
	// build a fake id_token JWT with the chatgpt_account_id claim
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"acct-xyz"}}`))
	got := provider.AccountIDFromTokens("h."+payload+".s", "")
	if got != "acct-xyz" {
		t.Errorf("got %q", got)
	}
	// stored field wins
	if got := provider.AccountIDFromTokens("h."+payload+".s", "stored"); got != "stored" {
		t.Errorf("stored should win, got %q", got)
	}
	// empty
	if got := provider.AccountIDFromTokens("", ""); got != "" {
		t.Errorf("empty should give empty, got %q", got)
	}
}

// TestDevicePollErrorCode pins both error shapes: the production nested
// {"error":{"code":...}} (what real OpenAI returns — every real first poll is a
// nested "pending") and the flat {"error":"..."} kept for older mocks.
func TestDevicePollErrorCode(t *testing.T) {
	for _, tc := range []struct {
		body string
		want string
	}{
		{`{"error":{"code":"deviceauth_authorization_pending","message":"Working..."}}`, "deviceauth_authorization_pending"},
		{`{"error":{"code":"deviceauth_slow_down"}}`, "deviceauth_slow_down"},
		{`{"error":{"code":"deviceauth_authorization_expired"}}`, "deviceauth_authorization_expired"},
		{`{"error":{"code":"deviceauth_authorization_denied"}}`, "deviceauth_authorization_denied"},
		{`{"error":"pending"}`, "pending"},
		{`{"error":"slow_down"}`, "slow_down"},
		{`{"error":"expired_token"}`, "expired_token"},
		{`{"error":"access_denied"}`, "access_denied"},
		{`{"error":{}}`, ""},
		{`not json`, ""},
	} {
		if got := DevicePollErrorCode([]byte(tc.body)); got != tc.want {
			t.Errorf("DevicePollErrorCode(%s)=%q, want %q", tc.body, got, tc.want)
		}
	}
}

func TestDirOf(t *testing.T) {
	cases := map[string]string{
		"/a/b/c":    "/a/b",
		"/root":     "",
		"nopath":    ".",
		"a/b/c.txt": "a/b",
	}
	for in, want := range cases {
		if got := DirOf(in); got != want {
			t.Errorf("DirOf(%q)=%q want %q", in, got, want)
		}
	}
}
