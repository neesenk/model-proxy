package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"model-proxy/provider"
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
	opts := &codexLoginServerOptions{usercodeURL: srv.URL, httpClient: &http.Client{Timeout: 5 * time.Second}}
	uc, err := requestUserCode(opts, provider.CodexOAuthClientID)
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
			fmt.Fprint(w, `{"error":"pending"}`)
			return
		}
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"authorization_code":"authcode-1","code_challenge":"cc","code_verifier":"cv-1"}`)
	}))
	defer srv.Close()
	opts := &codexLoginServerOptions{deviceTokURL: srv.URL, httpClient: &http.Client{Timeout: 5 * time.Second}}
	cs, err := pollForToken(opts, "daid", "uc", 0) // interval=0 → defaults to 5
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
	opts := &codexLoginServerOptions{deviceTokURL: srv.URL, httpClient: &http.Client{Timeout: 5 * time.Second}}
	_, err := pollForToken(opts, "d", "u", 0)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
}

// TestPollForToken_AccessDenied verifies error propagation.
func TestPollForToken_AccessDenied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		fmt.Fprint(w, `{"error":"access_denied"}`)
	}))
	defer srv.Close()
	opts := &codexLoginServerOptions{deviceTokURL: srv.URL, httpClient: &http.Client{Timeout: 5 * time.Second}}
	_, err := pollForToken(opts, "d", "u", 0)
	if err == nil {
		t.Fatal("expected error for access_denied")
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
	opts := &codexLoginServerOptions{tokenURL: srv.URL, httpClient: &http.Client{Timeout: 5 * time.Second}}
	af, err := exchangeCodeForTokens(opts, provider.CodexOAuthClientID, "authcode", "verifier")
	if err != nil {
		t.Fatal(err)
	}
	want := "authorization_code|authcode|" + codexOAuthCallback + "|" + provider.CodexOAuthClientID + "|verifier"
	if gotForm != want {
		t.Errorf("form=%q want %q", gotForm, want)
	}
	if af.Tokens.AccessToken != "at" || af.Tokens.RefreshToken != "rt" || af.Tokens.IDToken != "it" {
		t.Errorf("tokens=%+v", af.Tokens)
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
