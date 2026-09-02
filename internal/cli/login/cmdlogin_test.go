package login

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"model-proxy/internal/accounts"
	configdomain "model-proxy/internal/config"
)

// cmdlogin_test.go covers the CmdLogin process entry in-process: the usage
// path, a full apikey login through the flag/dispatch wiring, the --from-env
// branch, and RunProviderLogin's volcengine dispatch. The log.Fatal error
// paths stay subprocess-covered in internal/cli/login_cmd_test.go.

// writeLoginConfig writes a minimal one-provider config and returns its path.
// baseURL is wired as the provider's openai_base_url (required by config
// validation) so the login-time key validation probes the mock, never a real
// endpoint.
func writeLoginConfig(t *testing.T, baseURL string) string {
	t.Helper()
	yaml := `listen: 127.0.0.1:1
providers:
  zhipu:
    provider_id: zhipu
    openai_base_url: ` + baseURL + `
    models: [glm-x]
routes:
  glm-x:
    - {provider: zhipu, model: glm-x, priority: 1}
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// redirectStdin feeds lines to the next stdin readers (key prompts, Scanln).
func redirectStdin(t *testing.T, input string) {
	t.Helper()
	orig := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = orig })
	if _, err := w.Write([]byte(input)); err != nil {
		t.Fatal(err)
	}
	w.Close()
}

// TestCmdLogin_NoProviderPrintsUsage covers the no-positional path: usage +
// provider list on stdout, no login attempted, no exit.
func TestCmdLogin_NoProviderPrintsUsage(t *testing.T) {
	setPoolHome(t, t.TempDir())
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer up.Close()
	cfgPath := writeLoginConfig(t, up.URL)
	out := grabStdout(t, func() {
		CmdLogin([]string{"--config", cfgPath})
	})
	if !strings.Contains(out, "usage: model-proxy login") || !strings.Contains(out, "zhipu (provider=zhipu)") {
		t.Errorf("usage output missing usage line or provider list:\n%s", out)
	}
	// No login attempted → no pool file.
	if _, err := os.Stat(filepath.Join(accounts.HomeDir(), ".model-proxy", "zhipu_apikeys.json")); !os.IsNotExist(err) {
		t.Errorf("no-provider path must not write a pool (err=%v)", err)
	}
}

// TestCmdLogin_ApiKeyLoginSavesPool drives the full process entry: config load
// → credentials mode → apikey dispatch → stdin key prompt → pool save → daemon
// nudge (no-op without a pid file).
func TestCmdLogin_ApiKeyLoginSavesPool(t *testing.T) {
	setPoolHome(t, t.TempDir())
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer up.Close()
	cfgPath := writeLoginConfig(t, up.URL)
	redirectStdin(t, "sk-cmd-login-key\n")

	CmdLogin([]string{"--config", cfgPath, "zhipu", "--label", "team"})

	pool, err := accounts.NewStore(accounts.HomeDir()).Load("zhipu", "zhipu")
	if err != nil {
		t.Fatalf("load pool: %v", err)
	}
	if len(pool.Accounts) != 1 || pool.Accounts[0].APIKey != "sk-cmd-login-key" || pool.Accounts[0].Label != "team" {
		t.Fatalf("pool after CmdLogin = %+v, want one account {key:sk-cmd-login-key label:team}", pool.Accounts)
	}
}

// TestCmdLogin_FromEnvLogin covers the --from-env branch of the CmdLogin
// dispatch (no stdin key prompt; key comes from the environment).
func TestCmdLogin_FromEnvLogin(t *testing.T) {
	setPoolHome(t, t.TempDir())
	t.Setenv("MP_CMD_LOGIN_KEY", "sk-from-env-key")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer up.Close()
	cfgPath := writeLoginConfig(t, up.URL)

	CmdLogin([]string{"--config", cfgPath, "zhipu", "--from-env", "MP_CMD_LOGIN_KEY", "--label", "env-team"})

	pool, err := accounts.NewStore(accounts.HomeDir()).Load("zhipu", "zhipu")
	if err != nil {
		t.Fatalf("load pool: %v", err)
	}
	if len(pool.Accounts) != 1 || pool.Accounts[0].APIKey != "sk-from-env-key" || pool.Accounts[0].Label != "env-team" {
		t.Fatalf("pool after --from-env CmdLogin = %+v", pool.Accounts)
	}
}

// TestRunProviderLogin_VolcengineDispatch covers the default→volcengine branch
// of the provider dispatch: all three triple values are prompted on stdin when
// not passed, then the pool-aware save runs.
func TestRunProviderLogin_VolcengineDispatch(t *testing.T) {
	setPoolHome(t, t.TempDir())
	stubVolcengineValidator(t)
	redirectStdin(t, "ark-key\nAK9\nSK9\n")
	cfg := &configdomain.Config{Providers: map[string]configdomain.Provider{"vol": {Provider: "volcengine"}}}

	if err := RunProviderLogin(cfg, "vol", "", "lbl", false); err != nil {
		t.Fatalf("RunProviderLogin volcengine: %v", err)
	}
	pool, err := accounts.NewStore(accounts.HomeDir()).Load("vol", "volcengine")
	if err != nil {
		t.Fatalf("load pool: %v", err)
	}
	if len(pool.Accounts) != 1 {
		t.Fatalf("want 1 account, got %+v", pool.Accounts)
	}
	a := pool.Accounts[0]
	if a.APIKey != "ark-key" || a.AccessKey != "AK9" || a.SecretKey != "SK9" || a.Label != "lbl" {
		t.Fatalf("saved triple = %+v", a)
	}
}

// TestRunProviderLogin_ApiKeyDispatch covers the default apikey branch of the
// provider dispatch with the key passed directly (no prompt).
func TestRunProviderLogin_ApiKeyDispatch(t *testing.T) {
	setPoolHome(t, t.TempDir())
	cfg := &configdomain.Config{Providers: map[string]configdomain.Provider{"zhipu": {Provider: "zhipu"}}}

	if err := RunProviderLogin(cfg, "zhipu", "sk-direct", "", false); err != nil {
		t.Fatalf("RunProviderLogin apikey: %v", err)
	}
	pool, err := accounts.NewStore(accounts.HomeDir()).Load("zhipu", "zhipu")
	if err != nil {
		t.Fatalf("load pool: %v", err)
	}
	if len(pool.Accounts) != 1 || pool.Accounts[0].APIKey != "sk-direct" {
		t.Fatalf("pool after apikey dispatch = %+v", pool.Accounts)
	}
}
