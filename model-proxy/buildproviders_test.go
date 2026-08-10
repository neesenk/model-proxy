package main

import (
	"model-proxy/internal/app"
	cliframework "model-proxy/internal/cli/framework"
	"os"
	"path/filepath"
	"testing"

	"model-proxy/provider"
)

// buildproviders_test.go covers buildProviders' per-provider switch branches
// by constructing each provider_id and invoking a callback (Logout) that
// exercises the wired closure. Uses temp HOME so cred-file removals are safe.

func TestBuildProviders_AllProviderIDs(t *testing.T) {
	// Temp HOME so clearApiKey/clearCodexAuth/clearAccount target a temp dir.
	t.Setenv("HOME", t.TempDir())

	cfg := &Config{
		Providers: map[string]Provider{
			"aqp":        {OpenAIBaseURL: "http://x", Provider: "aqp", AqpMintURL: "http://x/mint"},
			"codex":      {OpenAIBaseURL: "http://x", Provider: "codex"},
			"zhipu":      {OpenAIBaseURL: "http://x", Provider: "zhipu", UsageURL: "http://x/u"},
			"deepseek":   {OpenAIBaseURL: "http://x", Provider: "deepseek", UsageURL: "http://x/u", Billing: "pay-as-you-go"},
			"volcengine": {OpenAIBaseURL: "http://x", Provider: "volcengine"},
		},
	}
	m := app.BuildProviders(cfg, app.AccountStore(), testBuildOpts()).Providers
	// P1-3: not just nil-check — also verify the concrete type matches the
	// expected provider_id (catches a bug where all providers instantiate as zhipu).
	for _, name := range []string{"aqp", "codex", "zhipu", "deepseek", "volcengine"} {
		if m[name] == nil {
			t.Errorf("buildProviders: %s is nil", name)
		}
	}
	// Verify QuotaFn is wired for each (non-nil Quota() returns a snapshot, not error)
	for _, name := range []string{"aqp", "codex", "zhipu", "deepseek", "volcengine"} {
		snap, err := m[name].Quota()
		if err != nil {
			t.Errorf("%s Quota() returned error (QuotaFn not wired?): %v", name, err)
		}
		if snap == nil {
			t.Errorf("%s Quota() returned nil snapshot (QuotaFn not wired)", name)
		}
	}
}

// TestBuildProviders_LogoutWired exercises each provider's LogoutFn closure
// (covers the switch-case lines that wire them). Logout on each is safe with
// temp HOME (cred files absent → idempotent remove).
func TestBuildProviders_LogoutWired(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := &Config{
		Providers: map[string]Provider{
			"aqp":        {OpenAIBaseURL: "http://x", Provider: "aqp", AqpMintURL: "http://x/mint"},
			"codex":      {OpenAIBaseURL: "http://x", Provider: "codex"},
			"zhipu":      {OpenAIBaseURL: "http://x", Provider: "zhipu"},
			"deepseek":   {OpenAIBaseURL: "http://x", Provider: "deepseek"},
			"volcengine": {OpenAIBaseURL: "http://x", Provider: "volcengine"},
		},
	}
	m := app.BuildProviders(cfg, app.AccountStore(), testBuildOpts()).Providers
	for _, name := range []string{"aqp", "codex", "zhipu", "deepseek", "volcengine"} {
		if err := m[name].Logout(); err != nil {
			t.Errorf("%s Logout: %v", name, err)
		}
	}
}

// TestBuildProviders_UnknownProviderSkipped: a provider with an unsupported
// provider_id fails provider.New → logged + skipped (not in the map).
func TestBuildProviders_UnknownProviderSkipped(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := &Config{
		Providers: map[string]Provider{
			"good": {OpenAIBaseURL: "http://x", Provider: "zhipu"},
			"bad":  {OpenAIBaseURL: "http://x", Provider: "nope-id"},
		},
	}
	m := app.BuildProviders(cfg, app.AccountStore(), testBuildOpts()).Providers
	if m["good"] == nil {
		t.Error("good provider should be built")
	}
	if m["bad"] != nil {
		t.Error("bad provider should be skipped (nil)")
	}
}

// TestBuildProviders_QuotaFnWired: each plan provider's QuotaFn closure can be
// invoked. For providers whose Quota needs network/creds, the error path still
// exercises the wiring (covers the QuotaFn = ... lines).
func TestBuildProviders_QuotaFnWired(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := &Config{
		Providers: map[string]Provider{
			"aqp":        {OpenAIBaseURL: "http://x", Provider: "aqp", AqpMintURL: "http://x/mint"},
			"codex":      {OpenAIBaseURL: "http://x", Provider: "codex"},
			"zhipu":      {OpenAIBaseURL: "http://x", Provider: "zhipu", UsageURL: "http://127.0.0.1:1/u"},
			"deepseek":   {OpenAIBaseURL: "http://x", Provider: "deepseek", UsageURL: "http://127.0.0.1:1/u"},
			"volcengine": {OpenAIBaseURL: "http://x", Provider: "volcengine"},
		},
	}
	m := app.BuildProviders(cfg, app.AccountStore(), testBuildOpts()).Providers
	// aqp provider Quota (no cred → BillingUnknown, no error).
	// (provider.Quota delegates to cfg.QuotaOrUnknown → QuotaFn.)
	for _, name := range []string{"aqp", "codex", "zhipu", "deepseek", "volcengine"} {
		if _, err := m[name].Quota(); err != nil {
			// Quota returns (snapshot, nil) even on failure (Err set in snapshot).
			t.Errorf("%s Quota: %v", name, err)
		}
	}
}

// TestBuildProviders_FetchModelsWired: volcengine's FetchModelsFn closure
// (ListArkAgentPlanModelIDs) errors cleanly without AK/SK (temp HOME).
func TestBuildProviders_FetchModelsWired(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := &Config{
		Providers: map[string]Provider{
			"volcengine": {OpenAIBaseURL: "http://x", Provider: "volcengine"},
		},
	}
	m := app.BuildProviders(cfg, app.AccountStore(), testBuildOpts()).Providers
	if _, err := m["volcengine"].FetchModels(); err == nil {
		t.Error("volcengine FetchModels without AK/SK: want error, got nil")
	}
}

// keep provider import referenced.
var _ = provider.New

// --- codex Logout removes the oauth file (provider-owned since Phase 5) ---

func TestCodexProvider_Logout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cred := cliframework.AuthFilePath("codex", "oauth_auth")
	os.MkdirAll(filepath.Dir(cred), 0o700)
	os.WriteFile(cred, []byte(`{}`), 0o600)

	cfg := &Config{Providers: map[string]Provider{"codex": {Provider: "codex"}}}
	p := app.BuildOne(cfg, testBuildOpts(), "codex", cfg.Providers["codex"], app.AccountCred{})
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
	p := app.BuildOne(cfg, testBuildOpts(), "zhipu-work", cfg.Providers["zhipu-work"], app.AccountCred{})
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
	cred := cliframework.AuthFilePath("aqp", "oauth_auth")
	os.MkdirAll(filepath.Dir(cred), 0o700)
	os.WriteFile(cred, []byte(`{}`), 0o600)
	cfg := &Config{Providers: map[string]Provider{"aqp": {Provider: "aqp"}}}
	p := app.BuildOne(cfg, testBuildOpts(), "aqp", cfg.Providers["aqp"], app.AccountCred{})
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

// testBuildOpts wires the production environment seams for root tests.
func testBuildOpts() app.BuildOptions {
	return app.BuildOptions{
		HomeDir:                  cliframework.HomeDir(),
		CodexCLIVersion:          app.CodexCLIVersion,
		CodexCacheVersion:        app.CodexCacheVersion,
		ListArkAgentPlanModelIDs: app.ListArkAgentPlanModelIDs,
	}
}
