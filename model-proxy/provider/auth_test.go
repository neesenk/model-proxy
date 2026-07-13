package provider

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	writeCodexAuth(t, path, "rt", time.Now().Add(time.Hour))
	p := NewCodexOAuthProvider(path)
	req, _ := http.NewRequest("POST", "http://x", nil)
	if err := p.Inject(req); err != nil {
		t.Fatal(err)
	}
	jwt := "head." + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, time.Now().Add(time.Hour).Unix()))) + ".sig"
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	dir := t.TempDir()
	store := filepath.Join(dir, "store.json")
	writeSSOCookie(t, store, "SSO_C=x")
	p := NewAqpKeyProvider(srv.URL, store)
	req, _ := http.NewRequest("POST", "http://x", nil)
	if err := p.Inject(req); err == nil {
		t.Error("expected error on mint HTTP 500")
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
