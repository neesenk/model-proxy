package main

import (
	"os"
	"path/filepath"
	"testing"
)

// wire_pure_test.go covers the small pure helpers that remain in the
// composition root and the buildOne-integrated Logout paths. Provider display
// and formatting behavior belongs to provider/*_test.go.

// --- quotaSourceLabel ---

func TestQuotaSourceLabel(t *testing.T) {
	for _, tc := range []struct {
		id   string
		want string
	}{
		{"aqp", "monthly_usage"},
		{"codex", "wham/usage"},
		{"zhipu", "quota/limit"},
		{"volcengine", "GetAFPUsage (AK/SK)"},
		{"deepseek", "user/balance"},
		{"unknown", "(none → unknown at runtime)"},
	} {
		if got := quotaSourceLabel(tc.id); got != tc.want {
			t.Errorf("quotaSourceLabel(%q)=%q want %q", tc.id, got, tc.want)
		}
	}
}

// --- truncate (gateway.go) ---

func TestTruncate(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Errorf("truncate(short)=%q", got)
	}
	if got := truncate("abcdef", 3); got != "abc..." {
		t.Errorf("truncate(abcdef,3)=%q want abc...", got)
	}
	if got := truncate("exact", 5); got != "exact" {
		t.Errorf("truncate(exact,5)=%q want exact", got)
	}
}

// --- codex Logout removes the oauth file (provider-owned since Phase 5) ---

func TestCodexProvider_Logout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cred := authFilePath("codex", "oauth_auth")
	os.MkdirAll(filepath.Dir(cred), 0o700)
	os.WriteFile(cred, []byte(`{}`), 0o600)

	cfg := &Config{Providers: map[string]Provider{"codex": {Provider: "codex"}}}
	p := buildOne(cfg, "codex", cfg.Providers["codex"], accountCred{})
	if p == nil {
		t.Fatal("buildOne codex returned nil")
	}
	if err := p.Logout(); err != nil {
		t.Fatalf("codex Logout: %v", err)
	}
	if _, err := os.Stat(cred); !os.IsNotExist(err) {
		t.Error("codex Logout did not remove the oauth_auth file")
	}
	// Idempotent: missing file is not an error.
	if err := p.Logout(); err != nil {
		t.Errorf("codex Logout (missing): want nil, got %v", err)
	}
}

// --- apikey Logout removes <name>_apikey.json (provider-owned since Phase 5) ---

func TestApiKeyProvider_Logout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cred := filepath.Join(home, ".model-proxy", "zhipu-work_apikey.json")
	os.MkdirAll(filepath.Dir(cred), 0o700)
	os.WriteFile(cred, []byte(`{}`), 0o600)

	cfg := &Config{Providers: map[string]Provider{
		"zhipu-work": {Provider: "zhipu"},
	}}
	p := buildOne(cfg, "zhipu-work", cfg.Providers["zhipu-work"], accountCred{})
	if p == nil {
		t.Fatal("buildOne zhipu-work returned nil")
	}
	if err := p.Logout(); err != nil {
		t.Fatalf("apikey Logout: %v", err)
	}
	if _, err := os.Stat(cred); !os.IsNotExist(err) {
		t.Error("apikey Logout did not remove the file")
	}
	if err := p.Logout(); err != nil {
		t.Errorf("apikey Logout (missing): want nil, got %v", err)
	}
}

// --- aqp Logout removes the oauth_auth file (provider-owned since Phase 5) ---

func TestAqpProvider_Logout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cred := authFilePath("aqp", "oauth_auth")
	os.MkdirAll(filepath.Dir(cred), 0o700)
	os.WriteFile(cred, []byte(`{}`), 0o600)
	cfg := &Config{Providers: map[string]Provider{"aqp": {Provider: "aqp"}}}
	p := buildOne(cfg, "aqp", cfg.Providers["aqp"], accountCred{})
	if p == nil {
		t.Fatal("buildOne aqp returned nil")
	}
	if err := p.Logout(); err != nil {
		t.Fatalf("aqp Logout: %v", err)
	}
	if _, err := os.Stat(cred); !os.IsNotExist(err) {
		t.Error("aqp Logout did not remove the oauth_auth file")
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
		if got := dirOf(in); got != want {
			t.Errorf("dirOf(%q)=%q want %q", in, got, want)
		}
	}
}
