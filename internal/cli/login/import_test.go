package login

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"model-proxy/internal/accounts"
	configdomain "model-proxy/internal/config"
	logincore "model-proxy/internal/login"
	"model-proxy/internal/provider"
)

// import_test.go covers the credential-import paths: `login codex
// --from-codex` (official codex CLI auth.json) and `login <provider>
// --from-env*` (environment variables). All fixtures are synthetic — no real
// credential file is ever read, and every assertion on error/output text
// verifies that secret VALUES never leak into messages.

const (
	fixtureAccessToken  = "FIXTURE-ACCESS-TOKEN-VALUE"
	fixtureRefreshToken = "FIXTURE-REFRESH-TOKEN-VALUE"
	fixtureIDToken      = "FIXTURE-ID-TOKEN-VALUE"
	fixtureAccountID    = "acct-fixture-1234567890"
)

// writeCodexCLIAuthFile plants a synthetic ~/.codex/auth.json under home.
func writeCodexCLIAuthFile(t *testing.T, home, body string) string {
	t.Helper()
	path := filepath.Join(home, ".codex", "auth.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func validCodexCLIAuthJSON() string {
	return `{
  "OPENAI_API_KEY": null,
  "tokens": {
    "id_token": "` + fixtureIDToken + `",
    "access_token": "` + fixtureAccessToken + `",
    "refresh_token": "` + fixtureRefreshToken + `",
    "account_id": "` + fixtureAccountID + `"
  },
  "last_refresh": "2026-08-01T02:03:04Z"
}`
}

// readWrittenCodexAuth loads the proxy-side oauth auth file written by the
// import (credstore resolves to plain files in test binaries).
func readWrittenCodexAuth(t *testing.T, path string) provider.CodexAuthFile {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read written auth file: %v", err)
	}
	var af provider.CodexAuthFile
	if err := json.Unmarshal(data, &af); err != nil {
		t.Fatalf("parse written auth file: %v", err)
	}
	return af
}

// --- --from-codex ---

func TestRunCodexImport_OK(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeCodexCLIAuthFile(t, home, validCodexCLIAuthJSON())

	out := grabStdout(t, func() {
		if err := RunCodexImport("codex"); err != nil {
			t.Fatalf("RunCodexImport: %v", err)
		}
	})

	af := readWrittenCodexAuth(t, oauthAuthFilePath(home, "codex"))
	if af.AuthMode != "chatgpt" {
		t.Errorf("auth_mode = %q, want chatgpt", af.AuthMode)
	}
	if af.Tokens.AccessToken != fixtureAccessToken ||
		af.Tokens.RefreshToken != fixtureRefreshToken ||
		af.Tokens.IDToken != fixtureIDToken ||
		af.Tokens.AccountID != fixtureAccountID {
		t.Errorf("written tokens mismatch: %+v", af.Tokens)
	}
	if af.LastRefresh != "2026-08-01T02:03:04Z" {
		t.Errorf("last_refresh = %q, want preserved from source", af.LastRefresh)
	}
	// UX: expiry note + masked account id; no raw token material in output.
	if !strings.Contains(out, "refresh") {
		t.Errorf("stdout missing refresh-on-demand note:\n%s", out)
	}
	if strings.Contains(out, fixtureAccountID) {
		t.Errorf("stdout leaks full account_id:\n%s", out)
	}
	for _, secret := range []string{fixtureAccessToken, fixtureRefreshToken, fixtureIDToken} {
		if strings.Contains(out, secret) {
			t.Errorf("stdout leaks token material:\n%s", out)
		}
	}
}

func TestRunCodexImport_RenamedProviderWritesConfigNameFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeCodexCLIAuthFile(t, home, validCodexCLIAuthJSON())

	if err := RunCodexImport("codex-work"); err != nil {
		t.Fatalf("RunCodexImport: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".model-proxy", "codex-work_oauth_auth.json")); err != nil {
		t.Fatalf("renamed instance must write codex-work_oauth_auth.json: %v", err)
	}
}

func TestRunCodexImport_MissingFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	err := RunCodexImport("codex")
	if err == nil {
		t.Fatal("want error for missing auth.json")
	}
	if !strings.Contains(err.Error(), ".codex/auth.json") || !strings.Contains(err.Error(), "codex login") {
		t.Errorf("error should name the missing path and the remedy: %v", err)
	}
	// Nothing written.
	if _, statErr := os.Stat(oauthAuthFilePath(home, "codex")); !os.IsNotExist(statErr) {
		t.Errorf("auth file must not be created on failed import")
	}
}

func TestRunCodexImport_CorruptJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeCodexCLIAuthFile(t, home, "{ this is not json "+fixtureAccessToken)

	err := RunCodexImport("codex")
	if err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("err = %v, want 'not valid JSON'", err)
	}
	if strings.Contains(err.Error(), fixtureAccessToken) {
		t.Errorf("error must not quote file content: %v", err)
	}
}

func TestRunCodexImport_MissingTokenNamesField(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeCodexCLIAuthFile(t, home, `{
  "OPENAI_API_KEY": null,
  "tokens": {"id_token": "`+fixtureIDToken+`", "access_token": "`+fixtureAccessToken+`", "account_id": "`+fixtureAccountID+`"}
}`)

	err := RunCodexImport("codex")
	if err == nil || !strings.Contains(err.Error(), "tokens.refresh_token") {
		t.Fatalf("err = %v, want 'tokens.refresh_token' named", err)
	}
	for _, secret := range []string{fixtureAccessToken, fixtureIDToken, fixtureAccountID} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error leaks fixture value: %v", err)
		}
	}
}

func TestRunCodexImport_APIKeyModeRejected(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeCodexCLIAuthFile(t, home, `{"OPENAI_API_KEY": "sk-fixture-secret", "tokens": null}`)

	err := RunCodexImport("codex")
	if err == nil || !strings.Contains(err.Error(), "apikey-mode") {
		t.Fatalf("err = %v, want apikey-mode hint", err)
	}
	if strings.Contains(err.Error(), "sk-fixture-secret") {
		t.Errorf("error leaks OPENAI_API_KEY value: %v", err)
	}
}

// --- --from-env ---

func TestEnvCredentials(t *testing.T) {
	t.Setenv("MP_LOGIN_KEY", "  key-value  ")
	t.Setenv("MP_LOGIN_AK", "ak-value")
	t.Setenv("MP_LOGIN_SK", "sk-value")
	t.Setenv("MP_LOGIN_EMPTY", "   ")

	key, ak, sk, err := envCredentials("MP_LOGIN_KEY", "MP_LOGIN_AK", "MP_LOGIN_SK")
	if err != nil {
		t.Fatalf("envCredentials: %v", err)
	}
	if key != "key-value" || ak != "ak-value" || sk != "sk-value" {
		t.Fatalf("got (%q, %q, %q)", key, ak, sk)
	}

	// Optional ak/sk may be omitted entirely.
	if _, _, _, err := envCredentials("MP_LOGIN_KEY", "", ""); err != nil {
		t.Fatalf("key-only: %v", err)
	}

	// Missing/empty variables are errors naming the VARIABLE, never a value.
	for _, tc := range []struct{ keyVar, akVar, wantName string }{
		{"MP_LOGIN_UNSET", "", "MP_LOGIN_UNSET"},
		{"MP_LOGIN_EMPTY", "", "MP_LOGIN_EMPTY"},
		{"MP_LOGIN_KEY", "MP_LOGIN_UNSET", "MP_LOGIN_UNSET"},
	} {
		_, _, _, err := envCredentials(tc.keyVar, tc.akVar, "")
		if err == nil || !strings.Contains(err.Error(), tc.wantName) {
			t.Errorf("envCredentials(%q, %q): err = %v, want %q named", tc.keyVar, tc.akVar, err, tc.wantName)
		}
	}
}

func TestRunFromEnvLogin_ApiKeyProvider(t *testing.T) {
	setPoolHome(t, t.TempDir())
	t.Setenv("MP_LOGIN_ZHIPU", "env-key-123")
	cfg := &configdomain.Config{Providers: map[string]configdomain.Provider{"zhipu": {Provider: "zhipu"}}}

	if err := runFromEnvLogin(cfg, "zhipu", cfg.Providers["zhipu"], "MP_LOGIN_ZHIPU", "", "", "team", false); err != nil {
		t.Fatalf("runFromEnvLogin: %v", err)
	}
	pool, err := accounts.NewStore(accounts.HomeDir()).Load("zhipu", "zhipu")
	if err != nil {
		t.Fatal(err)
	}
	if len(pool.Accounts) != 1 || pool.Accounts[0].APIKey != "env-key-123" || pool.Accounts[0].Label != "team" {
		t.Fatalf("pool wrong: %+v", pool.Accounts)
	}
}

func TestRunFromEnvLogin_RejectedForOAuthProviders(t *testing.T) {
	cfg := &configdomain.Config{Providers: map[string]configdomain.Provider{
		"codex": {Provider: "codex"},
		"aqp":   {Provider: "aqp"},
	}}
	if err := runFromEnvLogin(cfg, "codex", cfg.Providers["codex"], "MP_LOGIN_ZHIPU", "", "", "", false); err == nil || !strings.Contains(err.Error(), "--from-codex") {
		t.Errorf("codex: err = %v, want --from-codex hint", err)
	}
	if err := runFromEnvLogin(cfg, "aqp", cfg.Providers["aqp"], "MP_LOGIN_ZHIPU", "", "", "", false); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Errorf("aqp: err = %v, want not supported", err)
	}
}

func TestRunFromEnvLogin_FlagValidation(t *testing.T) {
	cfg := &configdomain.Config{Providers: map[string]configdomain.Provider{
		"zhipu": {Provider: "zhipu"},
		"vol":   {Provider: "volcengine"},
	}}
	// ak/sk flags require --from-env.
	if err := runFromEnvLogin(cfg, "vol", cfg.Providers["vol"], "", "MP_A", "", "", false); err == nil || !strings.Contains(err.Error(), "--from-env is required") {
		t.Errorf("ak without key var: err = %v", err)
	}
	// ak/sk flags are volcengine-only.
	if err := runFromEnvLogin(cfg, "zhipu", cfg.Providers["zhipu"], "MP_K", "MP_A", "", "", false); err == nil || !strings.Contains(err.Error(), "only valid for volcengine") {
		t.Errorf("ak on zhipu: err = %v", err)
	}
}

func TestRunVolcengineLoginFromEnv_Triple(t *testing.T) {
	setPoolHome(t, t.TempDir())
	stubVolcengineValidator(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer up.Close()
	cfg, _ := configdomain.LoadConfigFromBytes("test", []byte("providers:\n  vol:\n    provider_id: volcengine\n    openai_base_url: https://x\n    usage_url: "+up.URL+"\n"))
	prov := cfg.Providers["vol"]

	if err := runVolcengineLoginFromEnv(cfg, "vol", prov, "ark-key", "AK9", "SK9", "volc-label", false); err != nil {
		t.Fatalf("runVolcengineLoginFromEnv: %v", err)
	}
	pool, _ := accounts.NewStore(accounts.HomeDir()).Load("vol", "volcengine")
	if len(pool.Accounts) != 1 {
		t.Fatalf("want 1 account: %+v", pool.Accounts)
	}
	a := pool.Accounts[0]
	if a.APIKey != "ark-key" || a.AccessKey != "AK9" || a.SecretKey != "SK9" || a.Label != "volc-label" {
		t.Fatalf("triple/label not saved: %+v", a)
	}
}

// TestRunVolcengineLoginFromEnv_NoAKSKPins pins that the env path never
// prompts for AK/SK: a chat-only login (key only) must succeed with stdin
// closed, and the AK/SK validator must not run.
func TestRunVolcengineLoginFromEnv_NoAKSKPins(t *testing.T) {
	setPoolHome(t, t.TempDir())
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer up.Close()
	cfg, _ := configdomain.LoadConfigFromBytes("test", []byte("providers:\n  vol:\n    provider_id: volcengine\n    openai_base_url: https://x\n    usage_url: "+up.URL+"\n"))
	prov := cfg.Providers["vol"]

	orig := logincore.VolcengineAKSKValidator
	defer func() { logincore.VolcengineAKSKValidator = orig }()
	logincore.VolcengineAKSKValidator = func(string, string) error {
		t.Fatal("validator must not run when AK/SK absent")
		return nil
	}
	// stdin at EOF: any prompt would fail the save.
	origStdin := os.Stdin
	r, w, _ := os.Pipe()
	os.Stdin = r
	defer func() { os.Stdin = origStdin }()
	w.Close()

	if err := runVolcengineLoginFromEnv(cfg, "vol", prov, "ark-key", "", "", "", false); err != nil {
		t.Fatalf("chat-only env login: %v", err)
	}
	pool, _ := accounts.NewStore(accounts.HomeDir()).Load("vol", "volcengine")
	if len(pool.Accounts) != 1 || pool.Accounts[0].APIKey != "ark-key" || pool.Accounts[0].AccessKey != "" {
		t.Fatalf("chat-only account wrong: %+v", pool.Accounts)
	}
}

func TestRunVolcengineLoginFromEnv_PartialAKSKRejected(t *testing.T) {
	setPoolHome(t, t.TempDir())
	cfg := &configdomain.Config{Providers: map[string]configdomain.Provider{"vol": {Provider: "volcengine"}}}
	prov := cfg.Providers["vol"]
	err := runVolcengineLoginFromEnv(cfg, "vol", prov, "ark-key", "AK9", "", "", false)
	if err == nil || !strings.Contains(err.Error(), "both be set") {
		t.Fatalf("err = %v, want 'both be set'", err)
	}
}

// TestRunVolcengineLoginFromEnv_ReplaceConfirmPrompt covers the only stdin
// interaction of the env path: an existing id with replace=false prompts
// "[y/N]"; "y" confirms and the core overwrites the triple in place.
func TestRunVolcengineLoginFromEnv_ReplaceConfirmPrompt(t *testing.T) {
	setPoolHome(t, t.TempDir())
	stubVolcengineValidator(t)
	cfg := &configdomain.Config{Providers: map[string]configdomain.Provider{"vol": {Provider: "volcengine"}}}
	prov := cfg.Providers["vol"]
	if err := runVolcengineLoginFromEnv(cfg, "vol", prov, "old-ark", "AK9", "old-sk", "first", false); err != nil {
		t.Fatal(err)
	}

	orig := os.Stdin
	r, w, _ := os.Pipe()
	os.Stdin = r
	defer func() { os.Stdin = orig }()
	w.Write([]byte("y\n"))
	w.Close()

	if err := runVolcengineLoginFromEnv(cfg, "vol", prov, "new-ark", "AK9", "new-sk", "renamed", false); err != nil {
		t.Fatalf("confirmed replace: %v", err)
	}
	pool, _ := accounts.NewStore(accounts.HomeDir()).Load("vol", "volcengine")
	if len(pool.Accounts) != 1 {
		t.Fatalf("confirmed replace should keep size 1: %+v", pool.Accounts)
	}
	a := pool.Accounts[0]
	if a.APIKey != "new-ark" || a.SecretKey != "new-sk" || a.Label != "renamed" {
		t.Fatalf("confirmed replace did not overwrite: %+v", a)
	}
}
