package provider

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// auth_test.go covers the provider-owned auth injectors (codex OAuth, aqp key
// minting, SSO cookie read, JWT helpers), moved from main's auth_codex_test.go
// + auth_gateway_test.go in Phase 4.

// --- codex OAuth ---

// writeCodexAuth builds a fake codex auth.json with a JWT access_token expiring at exp.
func writeCodexAuth(t *testing.T, path, refresh string, exp time.Time) {
	t.Helper()
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp.Unix())))
	jwt := "head." + payload + ".sig"
	af := CodexAuthFile{}
	af.AuthMode = "chatgpt"
	af.Tokens.AccessToken = jwt
	af.Tokens.RefreshToken = refresh
	af.Tokens.AccountID = "acct-1"
	b, _ := json.MarshalIndent(af, "", "  ")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCodexOAuth_InjectValidToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	// One shared timestamp: computing time.Now() twice can straddle a second
	// boundary and flake the exact-match assertion below.
	exp := time.Now().Add(time.Hour)
	writeCodexAuth(t, path, "rt", exp)
	p := NewCodexOAuthProvider(path)
	req, _ := http.NewRequest("POST", "http://x", nil)
	if err := p.Inject(req); err != nil {
		t.Fatal(err)
	}
	jwt := "head." + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp.Unix()))) + ".sig"
	// P0-1: assert exact values, not just "non-empty" - originator + Account-Id + exact Bearer
	if got := req.Header.Get("Authorization"); got != "Bearer "+jwt {
		t.Errorf("Authorization=%q, want %q", got, "Bearer "+jwt)
	}
	if got := req.Header.Get("originator"); got != "codex_cli_rs" {
		t.Errorf("originator=%q, want codex_cli_rs", got)
	}
	if got := req.Header.Get("ChatGPT-Account-Id"); got != "acct-1" {
		t.Errorf("ChatGPT-Account-Id=%q, want acct-1", got)
	}
	if req.Header.Get("x-api-key") != "" {
		t.Error("x-api-key not cleared")
	}
}

func TestCodexOAuth_RefreshRotatesToken(t *testing.T) {
	// mock oauth/token returns a new access_token + rotates refresh_token
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody = r.FormValue("grant_type") + "|" + r.FormValue("client_id") + "|" + r.FormValue("refresh_token")
		payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, time.Now().Add(72*time.Hour).Unix())))
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "head." + payload + ".sig",
			"refresh_token": "rt-new",
			"expires_in":    3600,
		})
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "auth.json")
	writeCodexAuth(t, path, "rt-old", time.Now().Add(-time.Minute)) // expired
	p := NewCodexOAuthProvider(path)
	p.tokenURL = srv.URL // point at the mock

	if err := p.Refresh(); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	want := "refresh_token|" + CodexOAuthClientID + "|rt-old"
	if gotBody != want {
		t.Errorf("oauth request body=%q want %q", gotBody, want)
	}
	// auth.json rotated: new refresh_token written back.
	af, _ := p.load()
	if af.Tokens.RefreshToken != "rt-new" {
		t.Errorf("refresh_token not rotated: %q", af.Tokens.RefreshToken)
	}
	// Cached token is now valid and injectable.
	req, _ := http.NewRequest("POST", "http://x", nil)
	if err := p.Inject(req); err != nil {
		t.Errorf("inject after refresh: %v", err)
	}
}

func TestCodexOAuth_MissingRefreshToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	// no refresh token, expired access token
	writeCodexAuth(t, path, "", time.Now().Add(-time.Minute))
	p := NewCodexOAuthProvider(path)
	req, _ := http.NewRequest("POST", "http://x", nil)
	err := p.Inject(req)
	if err == nil {
		t.Error("expected error when access expired and no refresh_token")
	}
}

func TestJwtExpiry(t *testing.T) {
	// valid JWT with exp
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1700000000}`))
	exp := JwtExpiry("h." + payload + ".s")
	if exp.Unix() != 1700000000 {
		t.Errorf("got %v", exp)
	}
	// malformed
	if !JwtExpiry("not-a-jwt").IsZero() {
		t.Error("expected zero for malformed")
	}
	if !JwtExpiry("").IsZero() {
		t.Error("expected zero for empty")
	}
}

// idTokenWithEmail builds a fake id_token JWT carrying the `email` claim and the
// chatgpt_account_id claim under https://api.openai.com/auth (the two claims
// LoadCodexAccount + emailFromIDToken read).
func idTokenWithEmail(t *testing.T, email, accountID string) string {
	t.Helper()
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(
		`{"email":%q,"https://api.openai.com/auth":{"chatgpt_account_id":%q}}`,
		email, accountID)))
	return "head." + payload + ".sig"
}

// TestLoadCodexAccount verifies the Web-UI account projection reads the codex
// auth file (CodexAuthFile shape) and surfaces account_id + email, NOT any
// token. account_id prefers the stored tokens.account_id, else parses the
// id_token JWT; email is parsed from the id_token's `email` claim. This is the
// regression guard for the bug where the account list used LoadAqpAccount (the
// wrong shape) and so always showed codex as "No account configured".
func TestLoadCodexAccount(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "codex_oauth_auth.json")

	// Case 1: stored account_id wins; email parsed from id_token.
	af := CodexAuthFile{AuthMode: "chatgpt"}
	af.Tokens.AccessToken = "atk-secret"
	af.Tokens.RefreshToken = "rtk-secret"
	af.Tokens.IDToken = idTokenWithEmail(t, "u@x.com", "acct-from-jwt")
	af.Tokens.AccountID = "acct-stored"
	b, _ := json.MarshalIndent(af, "", "  ")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadCodexAccount(path)
	if err != nil || got == nil {
		t.Fatalf("LoadCodexAccount=%v,%v", got, err)
	}
	if got.AccountID != "acct-stored" {
		t.Errorf("AccountID=%q want acct-stored (stored must win)", got.AccountID)
	}
	if got.Email != "u@x.com" {
		t.Errorf("Email=%q want u@x.com", got.Email)
	}

	// Case 2: no stored account_id -> fall back to parsing the id_token JWT.
	af.Tokens.AccountID = ""
	b, _ = json.MarshalIndent(af, "", "  ")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = LoadCodexAccount(path)
	if err != nil || got == nil {
		t.Fatalf("LoadCodexAccount=%v,%v", got, err)
	}
	if got.AccountID != "acct-from-jwt" {
		t.Errorf("AccountID=%q want acct-from-jwt (parsed from id_token)", got.AccountID)
	}
	if got.Email != "u@x.com" {
		t.Errorf("Email=%q want u@x.com", got.Email)
	}

	// Case 3: absent file -> nil, nil (not an error).
	got, err = LoadCodexAccount(filepath.Join(dir, "missing.json"))
	if err != nil || got != nil {
		t.Errorf("absent file: got=%v,err=%v want nil,nil", got, err)
	}

	// Case 4: no id_token AND no stored account_id -> empty AccountID (the
	// caller's guard skips emitting a bogus empty entry).
	af.Tokens.IDToken = ""
	af.Tokens.AccountID = ""
	b, _ = json.MarshalIndent(af, "", "  ")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = LoadCodexAccount(path)
	if err != nil || got == nil {
		t.Fatalf("LoadCodexAccount=%v,%v", got, err)
	}
	if got.AccountID != "" || got.Email != "" {
		t.Errorf("degenerate file: AccountID=%q Email=%q want empty", got.AccountID, got.Email)
	}
}

// TestEmailFromIDToken covers the email-claim parser directly: present claim,
// absent claim, and malformed JWT.
func TestEmailFromIDToken(t *testing.T) {
	if got := emailFromIDToken(idTokenWithEmail(t, "a@b.com", "x")); got != "a@b.com" {
		t.Errorf("got %q want a@b.com", got)
	}
	noEmail := "head." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"s"}`)) + ".sig"
	if got := emailFromIDToken(noEmail); got != "" {
		t.Errorf("absent email: got %q want empty", got)
	}
	if got := emailFromIDToken("not-a-jwt"); got != "" {
		t.Errorf("malformed: got %q want empty", got)
	}
	if got := emailFromIDToken(""); got != "" {
		t.Errorf("empty: got %q want empty", got)
	}
}

// --- aqp key minting ---

// writeSSOCookie writes a store file with an sso_session_cookie.
func writeSSOCookie(t *testing.T, path, cookie string) {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"sso_session_cookie": cookie})
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReadSSOCookie(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "store.json")
	writeSSOCookie(t, good, "SSO_C=x")
	if got, err := ReadSSOCookie(good); err != nil || got != "SSO_C=x" {
		t.Errorf("ReadSSOCookie(good)=%q err=%v", got, err)
	}
	if _, err := ReadSSOCookie(filepath.Join(dir, "nope")); err == nil {
		t.Error("ReadSSOCookie(missing): want error, got nil")
	}
	empty := filepath.Join(dir, "empty.json")
	writeSSOCookie(t, empty, "")
	if _, err := ReadSSOCookie(empty); err == nil {
		t.Error("ReadSSOCookie(empty cookie): want error, got nil")
	}
	if _, err := ReadSSOCookie(""); err == nil {
		t.Error("ReadSSOCookie(empty path): want error, got nil")
	}
}

func TestAqpKeyProvider_MintAndCache(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "SSO_C=x" {
			t.Errorf("mint Cookie=%q want SSO_C=x", r.Header.Get("Cookie"))
		}
		json.NewEncoder(w).Encode(map[string]any{
			"retcode": 0,
			"data":    map[string]any{"api_key": "minted-key", "project_id": "p1"},
		})
	}))
	defer srv.Close()
	dir := t.TempDir()
	store := filepath.Join(dir, "store.json")
	writeSSOCookie(t, store, "SSO_C=x")
	p := NewAqpKeyProvider(srv.URL, store)
	req, _ := http.NewRequest("POST", "http://x", nil)
	if err := p.Inject(req); err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer minted-key" {
		t.Errorf("Authorization=%q want Bearer minted-key", got)
	}
	if req.Header.Get("x-api-key") != "" {
		t.Error("x-api-key should be cleared")
	}
	// Second Inject is served from cache (server would 2nd-call; assert same key).
	req2, _ := http.NewRequest("POST", "http://x", nil)
	if err := p.Inject(req2); err != nil {
		t.Fatal(err)
	}
	if got := req2.Header.Get("Authorization"); got != "Bearer minted-key" {
		t.Errorf("cached Authorization=%q want Bearer minted-key", got)
	}
}

func TestAqpKeyProvider_Non200(t *testing.T) {
	// Regression: the full error body used to be embedded untruncated. The
	// message is now capped at 200 bytes + "..." like the codex refresh error.
	longBody := strings.Repeat("x", 5000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(longBody))
	}))
	defer srv.Close()
	dir := t.TempDir()
	store := filepath.Join(dir, "store.json")
	writeSSOCookie(t, store, "SSO_C=x")
	p := NewAqpKeyProvider(srv.URL, store)
	req, _ := http.NewRequest("POST", "http://x", nil)
	err := p.Inject(req)
	if err == nil {
		t.Fatal("expected error on mint HTTP 500")
	}
	if !strings.Contains(err.Error(), "mint aqp key: HTTP 500:") {
		t.Errorf("error %q missing mint prefix", err.Error())
	}
	if got := len(err.Error()); got > len("mint aqp key: HTTP 500: ")+200+3 {
		t.Errorf("error length %d exceeds 200-byte body cap", got)
	}
}

func TestAqpKeyProvider_NoSSOCookie(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "store.json") // not written
	p := NewAqpKeyProvider("https://x", store)
	req, _ := http.NewRequest("POST", "http://x", nil)
	if err := p.Inject(req); err == nil {
		t.Error("expected error when no SSO cookie")
	}
}

// TestAqpKeyProvider_MintFailureNegativeCache (A14 regression): the mint runs
// under p.mu with a 30s HTTP timeout, so a down mint endpoint used to make
// every request queue on the lock for a fresh network round trip. Within
// refreshFailureTTL the cached error must be returned with NO network attempt;
// past the TTL a retry must happen; a successful mint must clear the cache.
// Time advances through the injected clock — no Sleep.
func TestAqpKeyProvider_MintFailureNegativeCache(t *testing.T) {
	var up int32 // 0 = endpoint down (503), 1 = up (mint succeeds)
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if atomic.LoadInt32(&up) == 0 {
			w.WriteHeader(503)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"retcode": 0,
			"data":    map[string]any{"api_key": "minted-key", "project_id": "p1"},
		})
	}))
	defer srv.Close()

	store := filepath.Join(t.TempDir(), "store.json")
	writeSSOCookie(t, store, "SSO_C=x")
	p := NewAqpKeyProvider(srv.URL, store)
	now := time.Now()
	p.now = func() time.Time { return now }

	req, _ := http.NewRequest("POST", "http://x", nil)
	err1 := p.Inject(req)
	if err1 == nil {
		t.Fatal("first inject against a down mint endpoint: want error")
	}
	if err2 := p.Inject(req); err2 == nil || err2.Error() != err1.Error() {
		t.Fatalf("second inject within TTL: err=%v, want the SAME cached error %q", err2, err1)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("network attempts = %d, want 1 (second inject served from the negative cache)", got)
	}

	// Advance past the TTL: the next inject retries the network.
	now = now.Add(refreshFailureTTL + time.Second)
	if err3 := p.Inject(req); err3 == nil {
		t.Fatal("post-TTL inject: want retry error (endpoint still down)")
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("network attempts after TTL = %d, want 2 (retry happened)", got)
	}

	// Advance past the TTL again, with the endpoint up: the mint succeeds and
	// clears the negative cache.
	now = now.Add(refreshFailureTTL + time.Second)
	atomic.StoreInt32(&up, 1)
	if err := p.Refresh(); err != nil {
		t.Fatalf("recovered mint: %v", err)
	}
	// ...then goes down again: a fresh network attempt (not the stale cached
	// error) proves the success cleared the cache.
	atomic.StoreInt32(&up, 0)
	if err := p.Refresh(); err == nil || err.Error() != err1.Error() {
		t.Fatalf("post-recovery mint against down endpoint: err=%v, want a fresh %q", err, err1)
	}
	if got := atomic.LoadInt32(&calls); got != 4 {
		t.Fatalf("network attempts = %d, want 4 (cache cleared by the success)", got)
	}
}

// TestCodexOAuth_RefreshFailureNegativeCache (A14 regression): same contract
// as the aqp case — the refresh runs under p.mu, so a down auth endpoint used
// to serialize every request on a fresh 30s round trip. Within
// refreshFailureTTL the cached error is returned with NO network attempt; past
// the TTL a retry happens; a successful refresh clears the cache. Clock via
// p.now, no Sleep.
func TestCodexOAuth_RefreshFailureNegativeCache(t *testing.T) {
	var up int32 // 0 = endpoint down (503), 1 = up (valid tokens)
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if atomic.LoadInt32(&up) == 0 {
			w.WriteHeader(503)
			return
		}
		payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, time.Now().Add(72*time.Hour).Unix())))
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "head." + payload + ".sig",
			"refresh_token": "rt-new",
		})
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "auth.json")
	writeCodexAuth(t, path, "rt-old", time.Now().Add(-time.Minute)) // expired -> refresh path
	p := NewCodexOAuthProvider(path)
	p.tokenURL = srv.URL
	now := time.Now()
	p.now = func() time.Time { return now }

	err1 := p.Refresh()
	if err1 == nil {
		t.Fatal("first refresh against a down endpoint: want error")
	}
	if err2 := p.Refresh(); err2 == nil || err2.Error() != err1.Error() {
		t.Fatalf("second refresh within TTL: err=%v, want the SAME cached error %q", err2, err1)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("network attempts = %d, want 1 (second refresh served from the negative cache)", got)
	}

	// Advance past the TTL: the next refresh retries the network.
	now = now.Add(refreshFailureTTL + time.Second)
	if err3 := p.Refresh(); err3 == nil {
		t.Fatal("post-TTL refresh: want retry error (endpoint still down)")
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("network attempts after TTL = %d, want 2 (retry happened)", got)
	}

	// Advance past the TTL again, with the endpoint up: the refresh succeeds
	// and clears the negative cache.
	now = now.Add(refreshFailureTTL + time.Second)
	atomic.StoreInt32(&up, 1)
	if err := p.Refresh(); err != nil {
		t.Fatalf("recovered refresh: %v", err)
	}
	// Endpoint goes down again: a fresh network attempt (not the stale cached
	// error) proves the success cleared the cache.
	atomic.StoreInt32(&up, 0)
	if err := p.Refresh(); err == nil || err.Error() != err1.Error() {
		t.Fatalf("post-recovery refresh against down endpoint: err=%v, want a fresh %q", err, err1)
	}
	if got := atomic.LoadInt32(&calls); got != 4 {
		t.Fatalf("network attempts = %d, want 4 (cache cleared by the success)", got)
	}
}

// --- SecretReporter (in-memory credentials for the guard known-secret set) ---

// The aqp minted key lives only in memory: ReportSecrets must return nothing
// before the first mint, the cached key after minting, and only the NEW key
// after a Refresh re-mint (the retired value must drop out).
func TestAqpKeyProvider_ReportSecrets(t *testing.T) {
	key := "minted-key-v1"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"retcode": 0,
			"data":    map[string]any{"api_key": key, "project_id": "p1"},
		})
	}))
	defer srv.Close()
	store := filepath.Join(t.TempDir(), "store.json")
	writeSSOCookie(t, store, "SSO_C=x")
	p := NewAqpKeyProvider(srv.URL, store)

	if got := p.ReportSecrets(); len(got) != 0 {
		t.Errorf("pre-mint ReportSecrets = %v, want empty", got)
	}

	req, _ := http.NewRequest("POST", "http://x", nil)
	if err := p.Inject(req); err != nil {
		t.Fatal(err)
	}
	got := p.ReportSecrets()
	if len(got) != 1 || got[0] != "minted-key-v1" {
		t.Fatalf("post-mint ReportSecrets = %v, want [minted-key-v1]", got)
	}

	// Refresh clears the cache and re-mints: only the rotated value is reported.
	key = "minted-key-v2"
	if err := p.Refresh(); err != nil {
		t.Fatal(err)
	}
	got = p.ReportSecrets()
	if len(got) != 1 || got[0] != "minted-key-v2" {
		t.Fatalf("post-Refresh ReportSecrets = %v, want [minted-key-v2]", got)
	}
}

// codex: nothing before any token load, the cached access_token afterwards.
func TestCodexOAuthProvider_ReportSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	exp := time.Now().Add(time.Hour)
	writeCodexAuth(t, path, "rt", exp)
	p := NewCodexOAuthProvider(path)

	if got := p.ReportSecrets(); len(got) != 0 {
		t.Errorf("pre-load ReportSecrets = %v, want empty", got)
	}
	req, _ := http.NewRequest("POST", "http://x", nil)
	if err := p.Inject(req); err != nil {
		t.Fatal(err)
	}
	jwt := "head." + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp.Unix()))) + ".sig"
	got := p.ReportSecrets()
	if len(got) != 1 || got[0] != jwt {
		t.Fatalf("post-load ReportSecrets = %v, want the cached access_token", got)
	}
}
