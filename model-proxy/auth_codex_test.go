package main

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

// helper: build a fake codex auth.json with a JWT access_token expiring at exp.
func writeCodexAuth(t *testing.T, path, refresh string, exp time.Time) {
	t.Helper()
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp.Unix())))
	jwt := "head." + payload + ".sig"
	af := codexAuthFile{}
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
	p := newCodexOAuthProvider(path)
	req, _ := http.NewRequest("POST", "http://x", nil)
	if err := p.Inject(req); err != nil {
		t.Fatal(err)
	}
	if v := req.Header.Get("Authorization"); v == "" || v == "Bearer " {
		t.Errorf("missing/empty Authorization: %q", v)
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
			"expires_in":     3600,
		})
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "auth.json")
	writeCodexAuth(t, path, "rt-old", time.Now().Add(-time.Minute)) // expired
	p := newCodexOAuthProvider(path)
	p.tokenURL = srv.URL // point at the mock

	if err := p.Refresh(); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	want := "refresh_token|" + codexOAuthClientID + "|rt-old"
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
	p := newCodexOAuthProvider(path)
	req, _ := http.NewRequest("POST", "http://x", nil)
	err := p.Inject(req)
	if err == nil {
		t.Error("expected error when access expired and no refresh_token")
	}
}

func TestJwtExpiry(t *testing.T) {
	// valid JWT with exp
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1700000000}`))
	exp := jwtExpiry("h." + payload + ".s")
	if exp.Unix() != 1700000000 {
		t.Errorf("got %v", exp)
	}
	// malformed
	if !jwtExpiry("not-a-jwt").IsZero() {
		t.Error("expected zero for malformed")
	}
	if !jwtExpiry("").IsZero() {
		t.Error("expected zero for empty")
	}
}

func TestNewAuthProvider_CodexOAuth(t *testing.T) {
	cfg := &Config{}
	p := newAuthProvider("codex", "codex", cfg)
	if _, ok := p.(*CodexOAuthProvider); !ok {
		t.Errorf("expected *CodexOAuthProvider, got %T", p)
	}
}
