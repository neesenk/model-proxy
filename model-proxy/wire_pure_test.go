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

// --- clearCodexAuth: removes codex oauth file, idempotent ---

func TestClearCodexAuth(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	credDir := filepath.Join(home, ".model-proxy")
	os.MkdirAll(credDir, 0o700)
	cred := filepath.Join(credDir, "codex_oauth_auth.json")
	os.WriteFile(cred, []byte(`{}`), 0o600)

	if err := clearCodexAuth(&Config{}); err != nil {
		t.Fatalf("clearCodexAuth existing: %v", err)
	}
	if _, err := os.Stat(cred); !os.IsNotExist(err) {
		t.Error("clearCodexAuth did not remove the file")
	}
	// Idempotent: missing file is not an error.
	if err := clearCodexAuth(&Config{}); err != nil {
		t.Errorf("clearCodexAuth missing: want nil, got %v", err)
	}
}

// --- clearApiKey: removes <name>_apikey.json, idempotent ---

func TestClearApiKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	credDir := filepath.Join(home, ".model-proxy")
	os.MkdirAll(credDir, 0o700)
	cred := filepath.Join(credDir, "zhipu-work_apikey.json")
	os.WriteFile(cred, []byte(`{}`), 0o600)

	if err := clearApiKey("zhipu-work"); err != nil {
		t.Fatalf("clearApiKey existing: %v", err)
	}
	if _, err := os.Stat(cred); !os.IsNotExist(err) {
		t.Error("clearApiKey did not remove the file")
	}
	if err := clearApiKey("zhipu-work"); err != nil {
		t.Errorf("clearApiKey missing: want nil, got %v", err)
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
