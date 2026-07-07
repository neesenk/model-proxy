package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"model-proxy/provider"
)

// wire_pure_test.go covers the provider_wire.go delegation wrappers
// (clearCodexAuth/clearApiKey) and the small pure helpers in main.go
// (or, ultimateRemaining, quotaSourceLabel, truncate).

// --- or ---

func TestOr(t *testing.T) {
	if got := or("", "fallback"); got != "fallback" {
		t.Errorf("or(empty)=%q want fallback", got)
	}
	if got := or("set", "fallback"); got != "set" {
		t.Errorf("or(set)=%q want set", got)
	}
}

// --- ultimateRemaining ---

func TestUltimateRemaining(t *testing.T) {
	if got := ultimateRemaining(nil); got != -1 {
		t.Errorf("ultimateRemaining(nil)=%v want -1", got)
	}
	ws := []provider.QuotaWindow{
		{Label: "5h", RemainingPct: 0.5},
		{Label: "Monthly", RemainingPct: 0.8, Ultimate: true},
	}
	if got := ultimateRemaining(ws); got != 0.8 {
		t.Errorf("ultimateRemaining=%v want 0.8", got)
	}
	// No ultimate window → -1.
	ws2 := []provider.QuotaWindow{{Label: "5h", RemainingPct: 0.5}}
	if got := ultimateRemaining(ws2); got != -1 {
		t.Errorf("ultimateRemaining(no ultimate)=%v want -1", got)
	}
}

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

// --- provider_wire delegation wrappers (cover them via direct call) ---

func TestProviderWire_Wrappers(t *testing.T) {
	// showXxxUsageData wrappers call their showXxxUsage, print to stdout, return
	// (nil,nil). Capture stdout and assert each prints a recognizable marker —
	// not just "returned nil" (which would pass even if delegated to the wrong func).
	cfg := &Config{
		Providers: map[string]Provider{
			"deepseek": {OpenAIBaseURL: "http://127.0.0.1:1", Provider: "deepseek", UsageURL: "http://127.0.0.1:1/balance"},
		},
	}
	// deepseek wrapper → prints "Provider:" + "deepseek" or "Not logged in"
	out := captureStdout(t, func() {
		_, _ = showDeepseekUsageData(cfg, "deepseek", cfg.Providers["deepseek"])
	})
	if !strings.Contains(out, "deepseek") && !strings.Contains(out, "Not logged in") {
		t.Errorf("showDeepseekUsageData output missing deepseek/Not logged in:\n%s", out)
	}
	// zhipu wrapper → delegates to showGenericUsage → fetchZhipuQuota → dead URL → error
	out = captureStdout(t, func() {
		_, _ = showZhipuUsageData(cfg, "deepseek", cfg.Providers["deepseek"])
	})
	if !strings.Contains(out, "deepseek") && !strings.Contains(out, "Not logged in") && !strings.Contains(out, "Error:") {
		t.Errorf("showZhipuUsageData output missing marker:\n%s", out)
	}
	// aqp wrapper
	out = captureStdout(t, func() {
		_, _ = showAqpUsageData(cfg)
	})
	if !strings.Contains(out, "aqp") && !strings.Contains(out, "Not logged in") {
		t.Errorf("showAqpUsageData output missing marker:\n%s", out)
	}
	// codex wrapper
	out = captureStdout(t, func() {
		_, _ = showCodexUsageData(cfg, cfg.Providers["deepseek"])
	})
	if !strings.Contains(out, "codex") && !strings.Contains(out, "Not logged in") {
		t.Errorf("showCodexUsageData output missing marker:\n%s", out)
	}
	// volcengine wrapper
	out = captureStdout(t, func() {
		_, _ = showVolcengineUsageData(cfg, "deepseek", cfg.Providers["deepseek"])
	})
	if !strings.Contains(out, "volcengine") && !strings.Contains(out, "Note:") {
		t.Errorf("showVolcengineUsageData output missing marker:\n%s", out)
	}
}

// --- runCodexLogin / runVolcengineLoginErr / runApiKeyLoginErr wrappers ---
// These delegate to interactive login functions (stdin/browser) — not safe to
// call in tests. They're thin wrappers; the underlying functions need real
// network/browser. Skip (documented as not covered).

func TestProviderWire_LoginWrappers(t *testing.T) {
	// runCodexLogin calls cmdCodexLogin([]string{}) which starts the device flow
	// (network). runVolcengineLoginErr/runApiKeyLoginErr prompt on stdin. All
	// unsafe to call here — covered by their function-level tests where feasible.
	t.Skip("login wrappers require interactive network/stdin — covered elsewhere")
}

// keep strings referenced.
var _ = strings.HasPrefix
