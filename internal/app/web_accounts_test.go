package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/zalando/go-keyring"
	"model-proxy/internal/accounts"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/credstore"
	"model-proxy/internal/login"
	"model-proxy/internal/provider"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// ---- web_accounts_test.go ----

func TestAccountsListMasked(t *testing.T) {
	setPoolHome(t, t.TempDir())
	if err := accounts.NewStore(accounts.HomeDir()).Save("zhipu", "zhipu", accounts.Pool{
		Version: 1,
		Accounts: []accounts.Account{{
			// ID is derived from the credential (AccountID), like login does.
			ID:        accounts.AccountID("zhipu", accounts.Credentials{APIKey: "sk-secret-key-1234567890"}),
			Label:     "work",
			APIKey:    "sk-secret-key-1234567890",
			AccessKey: "AK-LEAK-12345",
			SecretKey: "SK-LEAK-67890",
			AddedAt:   "2026-01-01T00:00:00Z",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	w, _ := newTestWeb(t)
	rec := httptest.NewRecorder()
	serveWeb(w, rec, httptest.NewRequest("GET", "/api/accounts", nil))
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	var decoded any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode accounts response: %v: %s", err, body)
	}
	assertJSONKeysAbsent(t, decoded, map[string]bool{
		"api_key":            true,
		"access_key":         true,
		"secret_key":         true,
		"sso_session_cookie": true,
	})
	// No raw secret may appear anywhere in the response.
	for _, secret := range []string{"sk-secret-key-1234567890", "AK-LEAK-12345", "SK-LEAK-67890"} {
		if strings.Contains(body, secret) {
			t.Errorf("raw secret leaked (%s):\n%s", secret, body)
		}
	}
	// The real account id MUST be present (unmasked) — the UI sends it back on
	// remove, so masking it would break deletion.
	if want := accounts.AccountID("zhipu", accounts.Credentials{APIKey: "sk-secret-key-1234567890"}); !strings.Contains(body, `"id":"`+want+`"`) {
		t.Errorf("real id (for removal) missing:\n%s", body)
	}
	if !strings.Contains(body, `"label":"work"`) {
		t.Errorf("label missing:\n%s", body)
	}
}

// TestAccountsListCodex is the regression test for the bug where the account
// tab showed codex as "No account configured" despite being logged in.
// handleAccountsList used LoadAqpAccount (the wrong on-disk shape) for codex,
// so every field mapped to empty and the AccountID guard dropped the entry.
// Now codex uses LoadCodexAccount (CodexAuthFile shape) and surfaces account_id
// + email parsed from the id_token. Asserts the real account_id + email are
// present AND no token (access/refresh/id) leaks.
func TestAccountsListCodex(t *testing.T) {
	setPoolHome(t, t.TempDir())
	// Build a codex auth file: a stored account_id, an id_token JWT carrying
	// email + chatgpt_account_id, and secret access/refresh tokens that must
	// never appear in the response.
	idPayload := base64.RawURLEncoding.EncodeToString([]byte(
		`{"email":"coder@openai.com","https://api.openai.com/auth":{"chatgpt_account_id":"acct-codex-42"}}`))
	af := provider.CodexAuthFile{AuthMode: "chatgpt"}
	af.Tokens.AccessToken = "atk-TOPSECRET-codex"
	af.Tokens.RefreshToken = "rtk-TOPSECRET-codex"
	af.Tokens.IDToken = "head." + idPayload + ".sig"
	af.Tokens.AccountID = "acct-codex-42"
	ab, _ := json.MarshalIndent(af, "", "  ")
	if err := os.WriteFile(accounts.AuthFilePath("codex", "oauth_auth"), ab, 0o600); err != nil {
		t.Fatal(err)
	}

	w, p := newTestWeb(t)
	p.mu.Lock()
	p.cfg.Providers["codex"] = configdomain.Provider{Provider: "codex", OpenAIBaseURL: "https://x"}
	p.mu.Unlock()

	rec := httptest.NewRecorder()
	serveWeb(w, rec, httptest.NewRequest("GET", "/api/accounts", nil))
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	var decoded any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode codex accounts response: %v: %s", err, body)
	}
	assertJSONKeysAbsent(t, decoded, map[string]bool{
		"access_token":  true,
		"refresh_token": true,
		"id_token":      true,
	})
	// The account_id (used by the UI for removal) + email label must appear.
	if !strings.Contains(body, `"id":"acct-codex-42"`) {
		t.Errorf("account_id missing:\n%s", body)
	}
	if !strings.Contains(body, `"email":"coder@openai.com"`) {
		t.Errorf("email (label) missing:\n%s", body)
	}
	// No token may leak - the acct struct has no field for any of them.
	for _, secret := range []string{"atk-TOPSECRET-codex", "rtk-TOPSECRET-codex", idPayload} {
		if strings.Contains(body, secret) {
			t.Errorf("token leaked (%s):\n%s", secret, body)
		}
	}
	// The codex provider card must carry one account (not the empty state).
	var resp struct {
		Providers []struct {
			Name     string `json:"name"`
			Accounts []struct {
				ID string `json:"id"`
			} `json:"accounts"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v: %s", err, body)
	}
	var n int
	for _, pr := range resp.Providers {
		if pr.Name == "codex" {
			n = len(pr.Accounts)
		}
	}
	if n != 1 {
		t.Errorf("codex accounts=%d want 1 (was 0 before the fix)", n)
	}
}

func assertJSONKeysAbsent(t *testing.T, value any, forbidden map[string]bool) {
	t.Helper()
	var visit func(any)
	visit = func(current any) {
		switch current := current.(type) {
		case map[string]any:
			for key, child := range current {
				if forbidden[key] {
					t.Errorf("forbidden credential field %q present in accounts response", key)
				}
				visit(child)
			}
		case []any:
			for _, child := range current {
				visit(child)
			}
		}
	}
	visit(value)
}

func TestAccountsAddRemove(t *testing.T) {
	setPoolHome(t, t.TempDir())
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer up.Close()
	w, p := newTestWeb(t)
	w.configFile = "test" // reload will fail (no such file); add must still succeed
	p.mu.Lock()
	p.cfg.Providers["zhipu"] = configdomain.Provider{Provider: "zhipu", OpenAIBaseURL: "https://x", UsageURL: up.URL}
	p.cfg.Providers["aqp"] = configdomain.Provider{Provider: "aqp", OpenAIBaseURL: "https://x"}
	p.cfg.Providers["codex"] = configdomain.Provider{Provider: "codex", OpenAIBaseURL: "https://x"}
	p.mu.Unlock()

	// aqp/codex add must 400 — they use the async login flow (POST /api/login/<n>/start),
	// not the apikey core. Asserting the message points there keeps a future refactor
	// from accidentally swallowing these into the apikey path.
	for _, name := range []string{"aqp", "codex"} {
		rec := httptest.NewRecorder()
		serveWeb(w, rec, httptest.NewRequest("POST", "/api/accounts/"+name,
			strings.NewReader(`{"api_key":"x"}`)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s add status=%d want 400 (async flow): %s", name, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "/api/login/"+name+"/start") {
			t.Errorf("%s add must point at async flow: %s", name, rec.Body.String())
		}
	}

	// unknown provider add → 404 (route table + 404 contract intact).
	rec404 := httptest.NewRecorder()
	serveWeb(w, rec404, httptest.NewRequest("POST", "/api/accounts/nope",
		strings.NewReader(`{"api_key":"x"}`)))
	if rec404.Code != http.StatusNotFound {
		t.Errorf("unknown provider add status=%d want 404", rec404.Code)
	}

	// apikey add: validate (200 from usage mock) → save to pool → reload (best-effort).
	rec := httptest.NewRecorder()
	serveWeb(w, rec, httptest.NewRequest("POST", "/api/accounts/zhipu",
		strings.NewReader(`{"api_key":"sk-test-1234567890","label":"work"}`)))
	if rec.Code != 200 {
		t.Fatalf("add status=%d body=%s", rec.Code, rec.Body.String())
	}
	pool, _ := accounts.NewStore(accounts.HomeDir()).Load("zhipu", "zhipu")
	if len(pool.Accounts) != 1 {
		t.Fatalf("account not added: %+v", pool.Accounts)
	}
	if pool.Accounts[0].Label != "work" {
		t.Errorf("label not saved: %q", pool.Accounts[0].Label)
	}
	id := pool.Accounts[0].ID

	// remove → pool emptied.
	rec2 := httptest.NewRecorder()
	serveWeb(w, rec2, httptest.NewRequest("DELETE", "/api/accounts/zhipu/"+id, nil))
	if rec2.Code != 200 {
		t.Fatalf("remove status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	pool2, _ := accounts.NewStore(accounts.HomeDir()).Load("zhipu", "zhipu")
	if len(pool2.Accounts) != 0 {
		t.Fatalf("account not removed: %+v", pool2.Accounts)
	}

	// remove with a missing id segment → 400 (not a panic / 500).
	rec3 := httptest.NewRecorder()
	serveWeb(w, rec3, httptest.NewRequest("DELETE", "/api/accounts/zhipu", nil))
	if rec3.Code != http.StatusBadRequest {
		t.Errorf("malformed remove status=%d want 400: %s", rec3.Code, rec3.Body.String())
	}

	// remove on unknown provider → 404 (route table intact).
	rec404b := httptest.NewRecorder()
	serveWeb(w, rec404b, httptest.NewRequest("DELETE", "/api/accounts/nope/x", nil))
	if rec404b.Code != http.StatusNotFound {
		t.Errorf("remove unknown provider status=%d want 404", rec404b.Code)
	}

	// add JSON decode failure → 400.
	recBad := httptest.NewRecorder()
	serveWeb(w, recBad, httptest.NewRequest("POST", "/api/accounts/zhipu",
		strings.NewReader(`{not-json`)))
	if recBad.Code != http.StatusBadRequest {
		t.Errorf("bad-json add status=%d want 400: %s", recBad.Code, recBad.Body.String())
	}

	// add validation failure (usage_url 401) → 400 from the add core. Asserting
	// the message carries "validation failed" proves we surface the core's error
	// verbatim rather than masking it as a generic 400.
	badUp := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.WriteHeader(401)
		rw.Write([]byte(`{"error":"bad key"}`))
	}))
	defer badUp.Close()
	p.mu.Lock()
	p.cfg.Providers["zhipu"] = configdomain.Provider{Provider: "zhipu", OpenAIBaseURL: "https://x", UsageURL: badUp.URL}
	p.mu.Unlock()
	recVal := httptest.NewRecorder()
	serveWeb(w, recVal, httptest.NewRequest("POST", "/api/accounts/zhipu",
		strings.NewReader(`{"api_key":"sk-bad"}`)))
	if recVal.Code != http.StatusBadRequest {
		t.Errorf("validation-failed add status=%d want 400: %s", recVal.Code, recVal.Body.String())
	}
	if !strings.Contains(recVal.Body.String(), "validation failed") {
		t.Errorf("validation error not surfaced: %s", recVal.Body.String())
	}
}

// TestAccountsRouting verifies the POST/DELETE cases are wired into serveAPI and
// stay distinct from GET /api/accounts (exact path) — guards against a future
// router change collapsing them or breaking path precedence.
func TestAccountsRouting(t *testing.T) {
	setPoolHome(t, t.TempDir())
	w, p := newTestWeb(t)
	w.configFile = "test"
	p.mu.Lock()
	p.cfg.Providers["aqp"] = configdomain.Provider{Provider: "aqp", OpenAIBaseURL: "https://x"}
	p.mu.Unlock()
	mux := http.NewServeMux()
	w.Register(mux)

	// GET /api/accounts → 200 (list, pre-existing).
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/accounts", nil))
	if rec.Code != 200 {
		t.Fatalf("GET /api/accounts status=%d want 200", rec.Code)
	}

	// POST /api/accounts/aqp → 400 (async flow), proving the POST prefix route is wired.
	recPost := httptest.NewRecorder()
	mux.ServeHTTP(recPost, httptest.NewRequest("POST", "/api/accounts/aqp", strings.NewReader(`{}`)))
	if recPost.Code != http.StatusBadRequest {
		t.Errorf("POST /api/accounts/aqp status=%d want 400: %s", recPost.Code, recPost.Body.String())
	}

	// DELETE /api/accounts/aqp/<id> → 200 (clearAccount on a nonexistent file is a no-op),
	// proving the DELETE prefix route is wired and distinct from POST.
	recDel := httptest.NewRecorder()
	mux.ServeHTTP(recDel, httptest.NewRequest("DELETE", "/api/accounts/aqp/some-id", nil))
	if recDel.Code != 200 {
		t.Errorf("DELETE /api/accounts/aqp/<id> status=%d want 200: %s", recDel.Code, recDel.Body.String())
	}

	// Unknown /api path still 404s.
	rec404 := httptest.NewRecorder()
	mux.ServeHTTP(rec404, httptest.NewRequest("GET", "/api/no-such", nil))
	if rec404.Code != http.StatusNotFound {
		t.Errorf("unknown /api path status=%d want 404", rec404.Code)
	}
}

// ---- web_login_test.go ----

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
	p.cfg.Providers["aqp"] = configdomain.Provider{Provider: "aqp", OpenAIBaseURL: "https://x"}
	p.mu.Unlock()
	// Seam: point the AQP client at the mock base so BootstrapLoginURL /
	// PollSession / fetchAPIKey hit the httptest.Server instead of the real
	// compass backend.
	w.newAqpClientFn = func(store string) *login.AqpClient {
		client := login.NewAqpClient(store)
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
	account, err := provider.LoadAqpAccount(accounts.AuthFilePath("aqp", "oauth_auth"))
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
	p.cfg.Providers["aqp"] = configdomain.Provider{Provider: "aqp", OpenAIBaseURL: "https://x"}
	p.mu.Unlock()
	w.newAqpClientFn = func(store string) *login.AqpClient {
		client := login.NewAqpClient(store)
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
	p.cfg.Providers["aqp"] = configdomain.Provider{Provider: "aqp", OpenAIBaseURL: "https://x"}
	p.mu.Unlock()
	w.newAqpClientFn = func(store string) *login.AqpClient {
		client := login.NewAqpClient(store)
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
	p.cfg.Providers["aqp"] = configdomain.Provider{Provider: "aqp", OpenAIBaseURL: "https://x"}
	p.cfg.Providers["codex"] = configdomain.Provider{Provider: "codex", OpenAIBaseURL: "https://x"}
	p.mu.Unlock()
	mux := http.NewServeMux()
	w.Register(mux)

	// POST /api/login/codex/start → 502 (requestUserCode fails against a dead
	// server), proving the POST prefix route is wired AND dispatches to the
	// codex branch (aqp would likewise try a real bootstrap). Forcing a dead
	// server keeps it deterministic — no real network dependency.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead.Close()
	w.newCodexOptions = func() *login.CodexLoginServerOptions {
		o := &login.CodexLoginServerOptions{UsercodeURL: dead.URL + "/usercode"}
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
	p.cfg.Providers["codex"] = configdomain.Provider{Provider: "codex", OpenAIBaseURL: "https://x"}
	p.mu.Unlock()
	// Seam: point codex options at the mock so requestUserCode / pollForToken /
	// exchangeCodeForTokens hit the httptest.Server instead of the real OpenAI
	// deviceauth endpoints.
	w.newCodexOptions = func() *login.CodexLoginServerOptions {
		o := &login.CodexLoginServerOptions{}
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
	if start.VerifyURL != login.CodexOAuthVerifyURL {
		t.Errorf("verify_url=%q want %q", start.VerifyURL, login.CodexOAuthVerifyURL)
	}
	if start.SessionID == "" {
		t.Fatal("session_id empty")
	}

	state := waitForLoginDone(t, w, start.SessionID)
	if state.Result != "acct-1" {
		t.Errorf("poll result=%q want acct-1", state.Result)
	}
	// codex auth file must exist (written 0600 by the goroutine).
	path := accounts.AuthFilePath("codex", "oauth_auth")
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
	p.cfg.Providers["codex"] = configdomain.Provider{Provider: "codex", OpenAIBaseURL: "https://x"}
	p.mu.Unlock()
	w.newCodexOptions = func() *login.CodexLoginServerOptions {
		o := &login.CodexLoginServerOptions{}
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

	path := accounts.AuthFilePath("codex", "oauth_auth")
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
	p.cfg.Providers["aqp-alt"] = configdomain.Provider{Provider: "aqp", OpenAIBaseURL: "https://x"}
	p.mu.Unlock()
	w.newAqpClientFn = func(store string) *login.AqpClient {
		client := login.NewAqpClient(store)
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
