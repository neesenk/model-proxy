package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// login_cmd_test.go covers cmdLogin's error/help paths (subprocess) and
// runApiKeyLogin's stdin-driven validation (in-process with a redirected stdin).

// --- cmdLogin: no provider → usage + available providers ---

func TestCLI_LoginNoProvider(t *testing.T) {
	cfg := writeTempConfig(t, minimalConfig)
	stdout, _, code := runCLI(t, "login", cfg)
	if code != 0 {
		t.Errorf("login (no provider): exit=%d want 0", code)
	}
	if !strings.Contains(stdout, "usage:") || !strings.Contains(stdout, "compass") {
		t.Errorf("login no provider missing usage/providers:\n%s", stdout)
	}
}

// --- cmdLogin: unknown provider → non-zero ---

func TestCLI_LoginUnknownProvider(t *testing.T) {
	cfg := writeTempConfig(t, minimalConfig)
	_, stderr, code := runCLI(t, "login", cfg, "nope")
	if code == 0 {
		t.Error("login nope: exit=0 want non-zero")
	}
	if !strings.Contains(stderr, "unknown provider") {
		t.Errorf("login nope stderr missing 'unknown provider':\n%s", stderr)
	}
}

// --- runApiKeyLogin: empty key → error (stdin redirected) ---

func TestRunApiKeyLogin_EmptyKey(t *testing.T) {
	// Redirect stdin to a pipe that yields an empty line.
	orig := os.Stdin
	r, w, _ := os.Pipe()
	os.Stdin = r
	defer func() { os.Stdin = orig }()
	w.Write([]byte("\n"))
	w.Close()

	cfg := &Config{Providers: map[string]Provider{"zhipu": {Provider: "zhipu"}}}
	err := runApiKeyLogin(cfg, "zhipu", cfg.Providers["zhipu"])
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("empty key: err=%v want 'empty' error", err)
	}
}

// --- runApiKeyLogin: valid key + mock validation → saved ---

func TestRunApiKeyLogin_ValidKeyMockValidation(t *testing.T) {
	// Stand up a mock usage endpoint that returns 200 (key accepted).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	home := t.TempDir()
	t.Setenv("HOME", home)
	// Redirect stdin to provide a key.
	orig := os.Stdin
	r, w, _ := os.Pipe()
	os.Stdin = r
	defer func() { os.Stdin = orig }()
	w.Write([]byte("test-api-key\n"))
	w.Close()

	cfg := &Config{
		Providers: map[string]Provider{
			"zhipu": {Provider: "zhipu", UsageURL: srv.URL},
		},
	}
	if err := runApiKeyLogin(cfg, "zhipu", cfg.Providers["zhipu"]); err != nil {
		t.Fatalf("runApiKeyLogin: %v", err)
	}
	// The key should have been saved.
	data, err := os.ReadFile(filepath.Join(home, ".model-proxy", "zhipu_apikey.json"))
	if err != nil {
		t.Fatalf("apikey file not saved: %v", err)
	}
	if !strings.Contains(string(data), "test-api-key") {
		t.Errorf("saved key wrong: %s", data)
	}
}

// --- runApiKeyLogin: 401 from validation → error ---

func TestRunApiKeyLogin_Validation401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`unauthorized`))
	}))
	defer srv.Close()

	t.Setenv("HOME", t.TempDir())
	orig := os.Stdin
	r, w, _ := os.Pipe()
	os.Stdin = r
	defer func() { os.Stdin = orig }()
	w.Write([]byte("bad-key\n"))
	w.Close()

	cfg := &Config{Providers: map[string]Provider{"zhipu": {Provider: "zhipu", UsageURL: srv.URL}}}
	err := runApiKeyLogin(cfg, "zhipu", cfg.Providers["zhipu"])
	if err == nil || !strings.Contains(err.Error(), "validation failed") {
		t.Errorf("401 validation: err=%v want 'validation failed'", err)
	}
}
