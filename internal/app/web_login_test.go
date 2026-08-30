package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	cliframework "model-proxy/internal/cli/framework"
	clilogin "model-proxy/internal/cli/login"
	"model-proxy/internal/credstore"
	"model-proxy/internal/provider"
)

// TestAqpLoginFlow exercises the full async aqp SSO login: start bootstraps a
// login URL (against an httptest mock of the compass backend), a goroutine polls
// auth/info → fetchAPIKey → saveAccount, and poll returns "done" with the email.
// The account file must be persisted with the identity from get_or_generate.
func TestAqpLoginFlow(t *testing.T) {
	setPoolHome(t, t.TempDir())
	// Mock aqp backend: bootstrap returns a login URL; auth/info returns an
	// active user (and sets SSO_C so the jar captures it — mirroring the real
	// backend's 200 Set-Cookie); get_or_generate returns the managed key +
	// identity.
	mux := http.NewServeMux()
	mux.HandleFunc("/compass-api/v1/auth/login", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		fmt.Fprint(w, `{"result":"https://soup.shopee.io/login"}`)
	})
	mux.HandleFunc("/compass-api/v1/auth/info", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: provider.SsoCookieName, Value: "test-sso-c", Path: "/"})
		fmt.Fprint(w, `{"retcode":0,"data":{"user":{"userid":1,"email":"u@x.com","is_active":true}}}`)
	})
	mux.HandleFunc("/api/v1/cqp/ccswitch/api_key/get_or_generate", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"retcode":0,"data":{"api_key":"managed-key","project_id":"proj","employee_email":"u@x.com"}}`)
	})
	up := httptest.NewServer(mux)
	defer up.Close()

	w, p := newTestWeb(t)
	p.mu.Lock()
	p.cfg.Providers["aqp"] = Provider{Provider: "aqp", OpenAIBaseURL: "https://x"}
	p.mu.Unlock()
	// Seam: point the AQP client at the mock base so BootstrapLoginURL /
	// PollSession / fetchAPIKey hit the httptest.Server instead of the real
	// compass backend.
	w.newAqpClientFn = func(store string) *clilogin.AqpClient {
		client := clilogin.NewAqpClient(store)
		client.Base = up.URL
		return client
	}

	rec := httptest.NewRecorder()
	serveWeb(w, rec, httptest.NewRequest("POST", "/api/login/aqp/start", nil))
	if rec.Code != 200 {
		t.Fatalf("start status=%d body=%s", rec.Code, rec.Body.String())
	}
	var start struct {
		SessionID string `json:"session_id"`
		LoginURL  string `json:"login_url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &start); err != nil {
		t.Fatalf("parse start response: %v: %s", err, rec.Body.String())
	}
	if start.SessionID == "" || start.LoginURL == "" {
		t.Fatalf("bad start response: %s", rec.Body.String())
	}
	if start.LoginURL != "https://soup.shopee.io/login" {
		t.Errorf("login_url=%q want https://soup.shopee.io/login", start.LoginURL)
	}

	state := waitForLoginDone(t, w, start.SessionID)
	if state.Result != "u@x.com" {
		t.Errorf("poll result=%q want u@x.com", state.Result)
	}
	account, err := provider.LoadAqpAccount(cliframework.AuthFilePath("aqp", "oauth_auth"))
	if err != nil {
		t.Fatalf("load persisted AQP account: %v", err)
	}
	if account == nil {
		t.Fatal("aqp account file not written")
	}
	if account.Email != "u@x.com" {
		t.Errorf("persisted email=%q want u@x.com", account.Email)
	}
	if account.ProjectID != "proj" {
		t.Errorf("persisted project_id=%q want proj", account.ProjectID)
	}
	if account.SSOSessionCookie == "" {
		t.Error("persisted sso_session_cookie is empty")
	}
}

// TestAqpLoginFlow_Error asserts a bootstrap failure surfaces as a 502 with
// NO session created (BeginLogin fails before handleLoginStart creates one —
// there is nothing to poll, so leaking a pending session id here would be the
// bug).
func TestAqpLoginFlow_Error(t *testing.T) {
	setPoolHome(t, t.TempDir())
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No "result" field → bootstrapAt fails to extract a login URL.
		w.WriteHeader(401)
		fmt.Fprint(w, `{"oops":"no url here"}`)
	}))
	defer up.Close()

	w, p := newTestWeb(t)
	p.mu.Lock()
	p.cfg.Providers["aqp"] = Provider{Provider: "aqp", OpenAIBaseURL: "https://x"}
	p.mu.Unlock()
	w.newAqpClientFn = func(store string) *clilogin.AqpClient {
		client := clilogin.NewAqpClient(store)
		client.Base = up.URL
		return client
	}

	rec := httptest.NewRecorder()
	serveWeb(w, rec, httptest.NewRequest("POST", "/api/login/aqp/start", nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("start status=%d want 502 body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "session_id") {
		t.Errorf("failed bootstrap must not create a login session, body=%s", rec.Body.String())
	}
}

// TestAqpLoginFlow_JobErrorResolvesSession covers the REAL hang guard: the
// session is created (start returns 200), but the background job fails
// mid-flight (API-key provisioning) — the session must resolve to
// state="error" instead of staying "pending" forever.
func TestAqpLoginFlow_JobErrorResolvesSession(t *testing.T) {
	setPoolHome(t, t.TempDir())
	mux := http.NewServeMux()
	mux.HandleFunc("/compass-api/v1/auth/login", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		fmt.Fprint(w, `{"result":"https://soup.shopee.io/login"}`)
	})
	mux.HandleFunc("/compass-api/v1/auth/info", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: provider.SsoCookieName, Value: "test-sso-c", Path: "/"})
		fmt.Fprint(w, `{"retcode":0,"data":{"user":{"userid":1,"email":"u@x.com","is_active":true}}}`)
	})
	// Key provisioning fails → the job's FetchAPIKeyContext error path fires.
	mux.HandleFunc("/api/v1/cqp/ccswitch/api_key/get_or_generate", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "provisioning down", http.StatusInternalServerError)
	})
	up := httptest.NewServer(mux)
	defer up.Close()

	w, p := newTestWeb(t)
	p.mu.Lock()
	p.cfg.Providers["aqp"] = Provider{Provider: "aqp", OpenAIBaseURL: "https://x"}
	p.mu.Unlock()
	w.newAqpClientFn = func(store string) *clilogin.AqpClient {
		client := clilogin.NewAqpClient(store)
		client.Base = up.URL
		return client
	}

	rec := httptest.NewRecorder()
	serveWeb(w, rec, httptest.NewRequest("POST", "/api/login/aqp/start", nil))
	if rec.Code != 200 {
		t.Fatalf("start status=%d body=%s", rec.Code, rec.Body.String())
	}
	var start struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &start); err != nil || start.SessionID == "" {
		t.Fatalf("bad start response: %v: %s", err, rec.Body.String())
	}

	state := waitForLoginTerminal(t, w, start.SessionID)
	if state.State != "error" {
		t.Fatalf("terminal state=%q want error (job failure must resolve the session)", state.State)
	}
	if !strings.Contains(state.Result, "api key provisioning") {
		t.Errorf("error result=%q want the provisioning failure detail", state.Result)
	}
}

// TestLoginRouting verifies POST /api/login/<n>/start and GET
// /api/login/<id>/poll are wired into serveAPI and stay distinct from each
// other and from the existing /api/accounts routes.
func TestLoginRouting(t *testing.T) {
	w, p := newTestWeb(t)
	p.mu.Lock()
	p.cfg.Providers["aqp"] = Provider{Provider: "aqp", OpenAIBaseURL: "https://x"}
	p.cfg.Providers["codex"] = Provider{Provider: "codex", OpenAIBaseURL: "https://x"}
	p.mu.Unlock()
	mux := http.NewServeMux()
	w.Register(mux)

	// POST /api/login/codex/start → 502 (requestUserCode fails against a dead
	// server), proving the POST prefix route is wired AND dispatches to the
	// codex branch (aqp would likewise try a real bootstrap). Forcing a dead
	// server keeps it deterministic — no real network dependency.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead.Close()
	w.newCodexOptions = func() *clilogin.CodexLoginServerOptions {
		o := &clilogin.CodexLoginServerOptions{UsercodeURL: dead.URL + "/usercode"}
		o.Defaults()
		return o
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/login/codex/start", nil))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("POST /api/login/codex/start status=%d want 502: %s", rec.Code, rec.Body.String())
	}

	// POST /api/login/unknown/start → 404 (unknown provider).
	recUnk := httptest.NewRecorder()
	mux.ServeHTTP(recUnk, httptest.NewRequest("POST", "/api/login/unknown/start", nil))
	if recUnk.Code != http.StatusNotFound {
		t.Errorf("POST unknown provider status=%d want 404", recUnk.Code)
	}

	// GET /api/login/nope/poll → 404 (unknown session), proving the GET prefix
	// route is wired and distinct from POST.
	recPoll := httptest.NewRecorder()
	mux.ServeHTTP(recPoll, httptest.NewRequest("GET", "/api/login/nope/poll", nil))
	if recPoll.Code != http.StatusNotFound {
		t.Errorf("GET unknown session status=%d want 404", recPoll.Code)
	}
}

// TestCodexLoginFlow exercises the full async codex OAuth device-flow login:
// start requests a user code (against an httptest mock of the OpenAI deviceauth
// endpoints), a goroutine polls deviceauth/token → exchanges the code → writes
// the codex auth file 0600 → hot-reloads, and poll returns "done" with the
// account id parsed from the fake id_token JWT. Mirrors TestAqpLoginFlow's
// shape; the newCodexOptions seam points requestUserCode / pollForToken /
// exchangeCodeForTokens at the httptest mock's 3 endpoints.
func TestCodexLoginFlow(t *testing.T) {
	setPoolHome(t, t.TempDir())
	// Build a fake id_token JWT with a chatgpt_account_id claim (signature is
	// dummy — exchangeCodeForTokens only parses the payload claim, it doesn't
	// verify the sig). Replicates codex_login_test.go:127-128 inline.
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"acct-1"}}`))
	fakeIDToken := "h." + payload + ".s"
	// Mock the 3 codex OAuth endpoints (usercode → devtok → tok).
	mux := http.NewServeMux()
	mux.HandleFunc("/usercode", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"device_auth_id":"daid","user_code":"CODE","interval":"1"}`)
	})
	mux.HandleFunc("/devtok", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"authorization_code":"ac","code_challenge":"cc","code_verifier":"cv"}`)
	})
	mux.HandleFunc("/tok", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"access_token":"at","refresh_token":"rt","id_token":"`+fakeIDToken+`"}`)
	})
	up := httptest.NewServer(mux)
	defer up.Close()

	w, p := newTestWeb(t)
	p.mu.Lock()
	p.cfg.Providers["codex"] = Provider{Provider: "codex", OpenAIBaseURL: "https://x"}
	p.mu.Unlock()
	// Seam: point codex options at the mock so requestUserCode / pollForToken /
	// exchangeCodeForTokens hit the httptest.Server instead of the real OpenAI
	// deviceauth endpoints.
	w.newCodexOptions = func() *clilogin.CodexLoginServerOptions {
		o := &clilogin.CodexLoginServerOptions{}
		o.Defaults()
		o.UsercodeURL = up.URL + "/usercode"
		o.DeviceTokURL = up.URL + "/devtok"
		o.TokenURL = up.URL + "/tok"
		return o
	}

	rec := httptest.NewRecorder()
	serveWeb(w, rec, httptest.NewRequest("POST", "/api/login/codex/start", nil))
	if rec.Code != 200 {
		t.Fatalf("start status=%d body=%s", rec.Code, rec.Body.String())
	}
	var start struct {
		SessionID string `json:"session_id"`
		UserCode  string `json:"user_code"`
		VerifyURL string `json:"verify_url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &start); err != nil {
		t.Fatalf("parse start response: %v: %s", err, rec.Body.String())
	}
	if start.UserCode != "CODE" {
		t.Fatalf("user_code=%q want CODE: %s", start.UserCode, rec.Body.String())
	}
	if start.VerifyURL != clilogin.CodexOAuthVerifyURL {
		t.Errorf("verify_url=%q want %q", start.VerifyURL, clilogin.CodexOAuthVerifyURL)
	}
	if start.SessionID == "" {
		t.Fatal("session_id empty")
	}

	state := waitForLoginDone(t, w, start.SessionID)
	if state.Result != "acct-1" {
		t.Errorf("poll result=%q want acct-1", state.Result)
	}
	// codex auth file must exist (written 0600 by the goroutine).
	path := cliframework.AuthFilePath("codex", "oauth_auth")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("codex auth file not written: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("codex auth file perm=%o want 0600", perm)
	}
}

func TestCodexLoginFlowKeychainCommitAndDelete(t *testing.T) {
	setPoolHome(t, t.TempDir())
	keyring.MockInit()
	t.Cleanup(keyring.MockInit)
	t.Setenv("MP_CRED_STORE", string(credstore.ModeKeychain))
	credstore.SetProcessMode(credstore.ModeFile)
	t.Cleanup(func() { credstore.SetProcessMode(credstore.ModeFile) })

	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"acct-keychain"}}`))
	fakeIDToken := "h." + payload + ".s"
	mux := http.NewServeMux()
	mux.HandleFunc("/usercode", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"device_auth_id":"daid","user_code":"CODE","interval":"1"}`)
	})
	mux.HandleFunc("/devtok", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"authorization_code":"ac","code_challenge":"cc","code_verifier":"cv"}`)
	})
	mux.HandleFunc("/tok", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"access_token":"at","refresh_token":"rt","id_token":"`+fakeIDToken+`"}`)
	})
	up := httptest.NewServer(mux)
	defer up.Close()

	w, p := newTestWeb(t)
	p.mu.Lock()
	p.cfg.Providers["codex"] = Provider{Provider: "codex", OpenAIBaseURL: "https://x"}
	p.mu.Unlock()
	w.newCodexOptions = func() *clilogin.CodexLoginServerOptions {
		o := &clilogin.CodexLoginServerOptions{}
		o.Defaults()
		o.UsercodeURL = up.URL + "/usercode"
		o.DeviceTokURL = up.URL + "/devtok"
		o.TokenURL = up.URL + "/tok"
		return o
	}

	rec := httptest.NewRecorder()
	serveWeb(w, rec, httptest.NewRequest(http.MethodPost, "/api/login/codex/start", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("start status=%d body=%s", rec.Code, rec.Body.String())
	}
	var start struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &start); err != nil {
		t.Fatalf("parse start response: %v", err)
	}
	state := waitForLoginDone(t, w, start.SessionID)
	if state.Result != "acct-keychain" {
		t.Fatalf("poll result=%q want acct-keychain", state.Result)
	}

	path := cliframework.AuthFilePath("codex", "oauth_auth")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("keychain-mode Web login created plaintext auth file: %v", err)
	}
	auth, err := provider.LoadCodexAuthFile(path)
	if err != nil {
		t.Fatalf("load keychain auth after Web login: %v", err)
	}
	if auth.Tokens.AccountID != "acct-keychain" {
		t.Fatalf("keychain auth account id = %q, want acct-keychain", auth.Tokens.AccountID)
	}

	deleted := httptest.NewRecorder()
	serveWeb(w, deleted, httptest.NewRequest(http.MethodDelete, "/api/accounts/codex/acct-keychain", nil))
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	if _, err := provider.LoadCodexAuthFile(path); !errors.Is(err, credstore.ErrNotFound) {
		t.Fatalf("keychain credential survived Web delete: %v", err)
	}
}

type loginPollState struct {
	State   string `json:"state"`
	Detail  string `json:"detail"`
	Result  string `json:"result"`
	Warning string `json:"warning"`
}

// waitForLoginTerminal polls until the login session reaches ANY terminal
// state ("done" or "error") and returns it — for tests that assert the error
// resolution itself.
func waitForLoginTerminal(t *testing.T, server *WebServer, sessionID string) loginPollState {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		recorder := httptest.NewRecorder()
		serveWeb(server,
			recorder,
			httptest.NewRequest(http.MethodGet, "/api/login/"+sessionID+"/poll", nil),
		)
		if recorder.Code != http.StatusOK {
			t.Fatalf(
				"login poll status=%d want 200 body=%s",
				recorder.Code,
				recorder.Body.String(),
			)
		}
		var state loginPollState
		if err := json.Unmarshal(recorder.Body.Bytes(), &state); err != nil {
			t.Fatalf("decode login poll response: %v: %s", err, recorder.Body.String())
		}
		switch state.State {
		case "done", "error":
			return state
		case "pending":
		default:
			t.Fatalf("login poll returned invalid state %q: %s", state.State, recorder.Body.String())
		}
		select {
		case <-ctx.Done():
			t.Fatalf("login session %s never resolved (silent hang): %v", sessionID, ctx.Err())
		default:
			time.Sleep(2 * time.Millisecond)
		}
	}
}

func waitForLoginDone(t *testing.T, server *WebServer, sessionID string) loginPollState {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		recorder := httptest.NewRecorder()
		serveWeb(server,
			recorder,
			httptest.NewRequest(http.MethodGet, "/api/login/"+sessionID+"/poll", nil),
		)
		if recorder.Code != http.StatusOK {
			t.Fatalf(
				"login poll status=%d want 200 body=%s",
				recorder.Code,
				recorder.Body.String(),
			)
		}
		var state loginPollState
		if err := json.Unmarshal(recorder.Body.Bytes(), &state); err != nil {
			t.Fatalf("decode login poll response: %v: %s", err, recorder.Body.String())
		}
		switch state.State {
		case "done":
			return state
		case "error":
			t.Fatalf("login poll errored: %s", recorder.Body.String())
		case "pending":
		default:
			t.Fatalf("login poll returned invalid state %q: %s", state.State, recorder.Body.String())
		}
		select {
		case <-ctx.Done():
			t.Fatalf("login session %s did not complete: %v", sessionID, ctx.Err())
		default:
			time.Sleep(2 * time.Millisecond)
		}
	}
}

// TestLoginStartByProviderID asserts handleLoginStart dispatches on the RESOLVED
// provider_id (prov.Provider), NOT the raw URL name. A config entry named
// "aqp-alt" with provider_id: aqp must reach the aqp flow (200 with a login URL),
// not 400 ("aqp-alt has no async login flow"). Before the fix the switch was on
// the URL name and "aqp-alt" fell through to default → 400.
//
// RED-before evidence: with the old switch-on-URL-name code, this test fails at
// the status check (got 400, want 200). After the fix (switch on prov.Provider),
// "aqp-alt" resolves to provider_id "aqp" and dispatches to startAqpLogin.
func TestLoginStartByProviderID(t *testing.T) {
	setPoolHome(t, t.TempDir())
	// Mock aqp backend: bootstrap returns a login URL (mirrors TestAqpLoginFlow).
	mux := http.NewServeMux()
	mux.HandleFunc("/compass-api/v1/auth/login", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		fmt.Fprint(w, `{"result":"https://soup.shopee.io/login"}`)
	})
	up := httptest.NewServer(mux)
	defer up.Close()

	w, p := newTestWeb(t)
	// Custom-named provider whose provider_id is aqp. Before the fix this name
	// was switched on directly and fell through to default (400).
	p.mu.Lock()
	p.cfg.Providers["aqp-alt"] = Provider{Provider: "aqp", OpenAIBaseURL: "https://x"}
	p.mu.Unlock()
	w.newAqpClientFn = func(store string) *clilogin.AqpClient {
		client := clilogin.NewAqpClient(store)
		client.Base = up.URL
		return client
	}

	rec := httptest.NewRecorder()
	serveWeb(w, rec, httptest.NewRequest("POST", "/api/login/aqp-alt/start", nil))
	if rec.Code != 200 {
		t.Fatalf("start status=%d want 200 (must dispatch by provider_id, not URL name): %s", rec.Code, rec.Body.String())
	}
	var start struct {
		SessionID string `json:"session_id"`
		LoginURL  string `json:"login_url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &start); err != nil {
		t.Fatalf("parse start response: %v: %s", err, rec.Body.String())
	}
	if start.LoginURL != "https://soup.shopee.io/login" {
		t.Errorf("login_url=%q want https://soup.shopee.io/login", start.LoginURL)
	}
	if start.SessionID == "" {
		t.Error("session_id empty")
	}
}
