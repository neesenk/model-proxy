package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// wire_pure_test.go covers the provider_wire.go delegation wrappers
// (clearCodexAuth/clearApiKey) and the small pure helpers in main.go
// (or, quotaSourceLabel, truncate). ultimateRemaining moved to the provider
// package in Phase 1 (provider/quota_parse_test.go).

// --- or ---

func TestOr(t *testing.T) {
	if got := or("", "fallback"); got != "fallback" {
		t.Errorf("or(empty)=%q want fallback", got)
	}
	if got := or("set", "fallback"); got != "set" {
		t.Errorf("or(set)=%q want set", got)
	}
}

// --- ultimateRemaining: moved to provider/quota_parse_test.go (Phase 1) ---

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

// --- show*Usage dispatch shims (cover them via direct call) ---
// These are 1-line buildOne().Usage() dispatchers (the display logic lives in
// provider/usage_display.go since Phase 3). Capture stdout and assert each
// prints a recognizable marker - not just "returned nil".

func TestShowUsageShims(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{
			"deepseek":   {OpenAIBaseURL: "http://127.0.0.1:1", Provider: "deepseek", UsageURL: "http://127.0.0.1:1/balance"},
			"volcengine": {Provider: "volcengine", Models: []string{"doubao"}},
		},
	}
	// deepseek shim -> prints "Provider:" + "deepseek" or "Not logged in"
	out := captureStdout(t, func() {
		showDeepseekUsage(cfg, "deepseek", cfg.Providers["deepseek"], nil)
	})
	if !strings.Contains(out, "deepseek") && !strings.Contains(out, "Not logged in") {
		t.Errorf("showDeepseekUsage output missing deepseek/Not logged in:\n%s", out)
	}
	// zhipu shim (via showGenericUsage) -> dead URL -> error or Not logged in
	out = captureStdout(t, func() {
		showGenericUsage(cfg, "deepseek", cfg.Providers["deepseek"], nil)
	})
	if !strings.Contains(out, "deepseek") && !strings.Contains(out, "Not logged in") && !strings.Contains(out, "Error:") {
		t.Errorf("showGenericUsage output missing marker:\n%s", out)
	}
	// aqp shim
	out = captureStdout(t, func() { showAqpUsage(cfg) })
	if !strings.Contains(out, "aqp") && !strings.Contains(out, "Not logged in") {
		t.Errorf("showAqpUsage output missing marker:\n%s", out)
	}
	// codex shim
	out = captureStdout(t, func() { showCodexUsage(cfg, cfg.Providers["deepseek"]) })
	if !strings.Contains(out, "codex") && !strings.Contains(out, "Not logged in") {
		t.Errorf("showCodexUsage output missing marker:\n%s", out)
	}
	// volcengine shim -> no AK/SK -> Note + config models
	out = captureStdout(t, func() {
		showVolcengineUsage(cfg, "volcengine", cfg.Providers["volcengine"], nil)
	})
	if !strings.Contains(out, "volcengine") && !strings.Contains(out, "Note:") {
		t.Errorf("showVolcengineUsage output missing marker:\n%s", out)
	}
	// Unknown provider id -> buildOne returns nil -> shim must not panic.
	showDeepseekUsage(cfg, "x", Provider{Provider: "unknown-id"}, nil)
	// Non-nil cred path (bound key) through the shim.
	cred := accountCred{APIKey: "k"}
	showDeepseekUsage(cfg, "deepseek", cfg.Providers["deepseek"], &cred)
}

// --- runCodexLogin / runVolcengineLoginErr / runApiKeyLoginErr wrappers ---
// These delegate to interactive login functions (stdin/browser) - not safe to
// call in tests. They're thin wrappers; the underlying functions need real
// network/browser. Skip (documented as not covered).

func TestProviderWire_LoginWrappers(t *testing.T) {
	// runCodexLogin calls cmdCodexLogin([]string{}) which starts the device flow
	// (network). runVolcengineLoginErr/runApiKeyLoginErr prompt on stdin. All
	// unsafe to call here - covered by their function-level tests where feasible.
	t.Skip("login wrappers require interactive network/stdin - covered elsewhere")
}

// keep strings referenced.
var _ = strings.HasPrefix

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
