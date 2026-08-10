package main

import (
	"fmt"
	cliframework "model-proxy/internal/cli/framework"
	clilogin "model-proxy/internal/cli/login"
	"net/http"
	"net/http/httptest"
	"os"
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
	if !strings.Contains(stdout, "usage:") || !strings.Contains(stdout, "aqp") {
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
	// The key should have been saved to the PLURAL pool file
	// (<name>_apikeys.json). The legacy singular <name>_apikey.json is no
	// longer written by runApiKeyLogin — it now routes through the pool-aware
	// path. Assert via loadPool so we also verify the file is parseable.
	pool, err := loadPool("zhipu", "zhipu")
	if err != nil {
		t.Fatalf("load pool: %v", err)
	}
	if len(pool.Accounts) != 1 {
		t.Fatalf("want 1 account in pool, got %d: %+v", len(pool.Accounts), pool.Accounts)
	}
	if pool.Accounts[0].APIKey != "test-api-key" {
		t.Errorf("saved key = %q, want test-api-key", pool.Accounts[0].APIKey)
	}
	// ID must match the sha256[:16] of the key (zhipu is non-volcengine).
	wantID := accountIDFor("zhipu", accountCred{APIKey: "test-api-key"})
	if pool.Accounts[0].ID != wantID {
		t.Errorf("saved id = %q, want %q", pool.Accounts[0].ID, wantID)
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

// --- pool-aware login: dedup by accountID, --label, --replace ---

// TestRunApiKeyLoginWithInput_DedupSameKey verifies that re-entering the SAME
// key with --replace keeps the pool at size 1 (idempotent replace, not append).
// The id is derived from the key (sha256[:16] for non-volcengine), so the same
// key always maps to the same id → the existing entry is overwritten in place.
func TestRunApiKeyLoginWithInput_DedupSameKey(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	cfg := &Config{Listen: "127.0.0.1:1", Providers: map[string]Provider{"zhipu": {Provider: "zhipu"}}}
	prov := cfg.Providers["zhipu"]
	writePoolFile(t, "zhipu", "zhipu", "DUP-KEY")
	runApiKeyLoginWithInput(cfg, "zhipu", prov, "DUP-KEY", "renamed", true /*replace*/)
	pool, err := loadPool("zhipu", "zhipu")
	if err != nil {
		t.Fatal(err)
	}
	if len(pool.Accounts) != 1 {
		t.Fatalf("dup key should keep size 1, got %d: %+v", len(pool.Accounts), pool.Accounts)
	}
	// Replace must also update the label.
	if pool.Accounts[0].Label != "renamed" {
		t.Fatalf("replace should update label: got %q want %q", pool.Accounts[0].Label, "renamed")
	}
	if pool.Accounts[0].APIKey != "DUP-KEY" {
		t.Fatalf("replace should keep key: got %q", pool.Accounts[0].APIKey)
	}
}

// TestRunApiKeyLoginWithInput_DedupSameKey_NoReplace_Aborts verifies that when
// the same id already exists and replace is false, the function aborts with
// "login cancelled" (stdin says "n"). The pool is left untouched.
func TestRunApiKeyLoginWithInput_DedupSameKey_NoReplace_Aborts(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	cfg := &Config{Listen: "127.0.0.1:1", Providers: map[string]Provider{"zhipu": {Provider: "zhipu"}}}
	prov := cfg.Providers["zhipu"]
	writePoolFile(t, "zhipu", "zhipu", "DUP-KEY")

	// Redirect stdin to answer "n" to the replace prompt.
	orig := os.Stdin
	r, w, _ := os.Pipe()
	os.Stdin = r
	defer func() { os.Stdin = orig }()
	w.Write([]byte("n\n"))
	w.Close()

	err := runApiKeyLoginWithInput(cfg, "zhipu", prov, "DUP-KEY", "", false /*replace*/)
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("expected 'cancelled' error, got %v", err)
	}
	// Pool unchanged.
	pool, _ := loadPool("zhipu", "zhipu")
	if len(pool.Accounts) != 1 || pool.Accounts[0].APIKey != "DUP-KEY" {
		t.Fatalf("abort should leave pool untouched: %+v", pool.Accounts)
	}
}

// TestRunApiKeyLoginWithInput_DifferentKeyAppends verifies that a NEW key
// appends a fresh entry to the pool, with the provided label applied.
func TestRunApiKeyLoginWithInput_DifferentKeyAppends(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	cfg := &Config{Listen: "127.0.0.1:1", Providers: map[string]Provider{"zhipu": {Provider: "zhipu"}}}
	writePoolFile(t, "zhipu", "zhipu", "KEY-1")
	runApiKeyLoginWithInput(cfg, "zhipu", cfg.Providers["zhipu"], "KEY-2", "team", false)
	pool, err := loadPool("zhipu", "zhipu")
	if err != nil {
		t.Fatal(err)
	}
	if len(pool.Accounts) != 2 {
		t.Fatalf("different key should append, got %d: %+v", len(pool.Accounts), pool.Accounts)
	}
	// Label applied to the new entry.
	var labeled *poolAccount
	for i := range pool.Accounts {
		if pool.Accounts[i].Label == "team" {
			labeled = &pool.Accounts[i]
		}
	}
	if labeled == nil || labeled.APIKey != "KEY-2" {
		t.Fatalf("labeled entry wrong: %+v", labeled)
	}
}

// TestRunApiKeyLoginWithInput_EmptyKey verifies that an empty prompted key
// errors without touching the pool. The direct `in` arg is empty, so the
// function prompts on stdin; we feed it an empty line.
func TestRunApiKeyLoginWithInput_EmptyKey(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	orig := os.Stdin
	r, w, _ := os.Pipe()
	os.Stdin = r
	defer func() { os.Stdin = orig }()
	w.Write([]byte("\n"))
	w.Close()

	cfg := &Config{Listen: "127.0.0.1:1", Providers: map[string]Provider{"zhipu": {Provider: "zhipu"}}}
	err := runApiKeyLoginWithInput(cfg, "zhipu", cfg.Providers["zhipu"], "", "", false)
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty key: err=%v want 'empty'", err)
	}
}

// TestRunApiKeyLoginWithInput_NoLabel_DefaultsToID verifies that when no label
// is provided, the entry's label defaults to the account id.
func TestRunApiKeyLoginWithInput_NoLabel_DefaultsToID(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	cfg := &Config{Listen: "127.0.0.1:1", Providers: map[string]Provider{"zhipu": {Provider: "zhipu"}}}
	prov := cfg.Providers["zhipu"]
	if err := runApiKeyLoginWithInput(cfg, "zhipu", prov, "FRESH-KEY", "", false); err != nil {
		t.Fatal(err)
	}
	pool, _ := loadPool("zhipu", "zhipu")
	if len(pool.Accounts) != 1 {
		t.Fatalf("want 1 account, got %d", len(pool.Accounts))
	}
	wantID := accountIDFor("zhipu", accountCred{APIKey: "FRESH-KEY"})
	if pool.Accounts[0].ID != wantID {
		t.Fatalf("id = %q want %q", pool.Accounts[0].ID, wantID)
	}
	if pool.Accounts[0].Label != wantID {
		t.Fatalf("label should default to id: got %q want %q", pool.Accounts[0].Label, wantID)
	}
}

// --- non-printing account cores (add/remove) ---

// TestAddApikeyAccountCore exercises the non-printing core extracted from
// runApiKeyLoginWithInput: validate + dedup + savePool under the lock, plus
// the symmetric removeApikeyAccount. No stdin, no printing — the web layer
// (Task 12) reuses this same path.
func TestAddApikeyAccountCore(t *testing.T) {
	setPoolHome(t, t.TempDir())
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer up.Close()
	cfg, _ := LoadConfigFromBytes("test", []byte("providers:\n  zhipu:\n    provider_id: zhipu\n    openai_base_url: https://x\n    usage_url: "+up.URL+"\n"))
	prov := cfg.Providers["zhipu"]
	id, err := clilogin.AddApikeyAccount(cfg, "zhipu", prov, accountCred{APIKey: "sk-test-1234567890"}, "my-label", false)
	if err != nil {
		t.Fatalf("addApikeyAccount: %v", err)
	}
	if id == "" {
		t.Fatal("empty id")
	}
	// ID must match the sha256[:16] of the key (non-volcengine).
	wantID := accountIDFor("zhipu", accountCred{APIKey: "sk-test-1234567890"})
	if id != wantID {
		t.Fatalf("id = %q, want %q", id, wantID)
	}
	pool, _ := loadPool("zhipu", "zhipu")
	if len(pool.Accounts) != 1 || pool.Accounts[0].Label != "my-label" {
		t.Fatalf("pool not written: %+v", pool.Accounts)
	}
	// Replace path: same id, new label, replace=true overwrites in place.
	if _, err := clilogin.AddApikeyAccount(cfg, "zhipu", prov, accountCred{APIKey: "sk-test-1234567890"}, "renamed", true); err != nil {
		t.Fatalf("replace addApikeyAccount: %v", err)
	}
	pool2, _ := loadPool("zhipu", "zhipu")
	if len(pool2.Accounts) != 1 || pool2.Accounts[0].Label != "renamed" {
		t.Fatalf("replace should keep size 1 + update label: %+v", pool2.Accounts)
	}
	// Replace path with replace=false on an existing id aborts without prompting
	// (no stdin in core) and leaves the pool untouched.
	if _, err := clilogin.AddApikeyAccount(cfg, "zhipu", prov, accountCred{APIKey: "sk-test-1234567890"}, "ignored", false); err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("dup-no-replace should error 'cancelled', got %v", err)
	}
	// Remove: pool empties.
	if err := clilogin.RemoveApikeyAccount("zhipu", "zhipu", id); err != nil {
		t.Fatalf("remove: %v", err)
	}
	pool3, _ := loadPool("zhipu", "zhipu")
	if len(pool3.Accounts) != 0 {
		t.Fatalf("pool not emptied: %+v", pool3.Accounts)
	}
}

// TestAddApikeyAccountCore_ValidationFail verifies the core rejects a 401 from
// the usage endpoint (no save).
func TestAddApikeyAccountCore_ValidationFail(t *testing.T) {
	setPoolHome(t, t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`unauthorized`))
	}))
	defer srv.Close()
	cfg, _ := LoadConfigFromBytes("test", []byte("providers:\n  zhipu:\n    provider_id: zhipu\n    openai_base_url: https://x\n    usage_url: "+srv.URL+"\n"))
	prov := cfg.Providers["zhipu"]
	_, err := clilogin.AddApikeyAccount(cfg, "zhipu", prov, accountCred{APIKey: "bad"}, "", false)
	if err == nil || !strings.Contains(err.Error(), "validation failed") {
		t.Fatalf("expected 'validation failed', got %v", err)
	}
	pool, _ := loadPool("zhipu", "zhipu")
	if len(pool.Accounts) != 0 {
		t.Fatalf("401 should not save: %+v", pool.Accounts)
	}
}

// TestAddVolcengineAccountCore verifies the AK/SK core: dedup by AccessKey,
// triple save, label, remove.
func TestAddVolcengineAccountCore(t *testing.T) {
	setPoolHome(t, t.TempDir())
	stubVolcengineValidator(t) // pool dedup/save logic; AK/SK validation tested elsewhere
	cfg, _ := LoadConfigFromBytes("test", []byte("providers:\n  vol:\n    provider_id: volcengine\n    openai_base_url: https://x\n"))
	prov := cfg.Providers["vol"]
	cred := accountCred{APIKey: "ark-key", AccessKey: "AK9XYZ", SecretKey: "SK9"}
	id, err := clilogin.AddVolcengineAccount(cfg, "vol", prov, cred, "volc-label", false)
	if err != nil {
		t.Fatalf("addVolcengineAccount: %v", err)
	}
	if id != "AK9XYZ" {
		t.Fatalf("volcengine id = %q, want AK9XYZ", id)
	}
	pool, _ := loadPool("vol", "volcengine")
	if len(pool.Accounts) != 1 || pool.Accounts[0].Label != "volc-label" {
		t.Fatalf("pool not written: %+v", pool.Accounts)
	}
	if pool.Accounts[0].AccessKey != "AK9XYZ" || pool.Accounts[0].SecretKey != "SK9" || pool.Accounts[0].APIKey != "ark-key" {
		t.Fatalf("triple not saved: %+v", pool.Accounts[0])
	}
	// Same AccessKey with replace=false aborts; with replace=true overwrites the triple.
	if _, err := clilogin.AddVolcengineAccount(cfg, "vol", prov, accountCred{APIKey: "ark2", AccessKey: "AK9XYZ", SecretKey: "SK-new"}, "", false); err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("dup-no-replace should error 'cancelled', got %v", err)
	}
	if _, err := clilogin.AddVolcengineAccount(cfg, "vol", prov, accountCred{APIKey: "ark2", AccessKey: "AK9XYZ", SecretKey: "SK-new"}, "renamed", true); err != nil {
		t.Fatalf("replace addVolcengineAccount: %v", err)
	}
	pool2, _ := loadPool("vol", "volcengine")
	if len(pool2.Accounts) != 1 {
		t.Fatalf("replace should keep size 1: %+v", pool2.Accounts)
	}
	if pool2.Accounts[0].APIKey != "ark2" || pool2.Accounts[0].SecretKey != "SK-new" || pool2.Accounts[0].Label != "renamed" {
		t.Fatalf("replace did not overwrite triple/label: %+v", pool2.Accounts[0])
	}
	if err := clilogin.RemoveApikeyAccount("vol", "volcengine", id); err != nil {
		t.Fatalf("remove: %v", err)
	}
	pool3, _ := loadPool("vol", "volcengine")
	if len(pool3.Accounts) != 0 {
		t.Fatalf("pool not emptied: %+v", pool3.Accounts)
	}
}

// --- flag parsing helpers ---

func TestFlagStringValue(t *testing.T) {
	cases := []struct {
		args []string
		flag string
		want string
	}{
		{[]string{"--label", "team"}, "--label", "team"},
		{[]string{"--label=team"}, "--label", "team"},
		{[]string{"login", "zhipu", "--label", "team"}, "--label", "team"},
		{[]string{"zhipu", "--label=team"}, "--label", "team"},
		{[]string{"zhipu"}, "--label", ""},
		{[]string{"zhipu", "--label"}, "--label", ""}, // no value after flag
		{[]string{}, "--label", ""},
	}
	for i, c := range cases {
		got := cliframework.FlagStringValue(c.args, c.flag)
		if got != c.want {
			t.Errorf("case %d: cliframework.FlagStringValue(%v, %q) = %q, want %q", i, c.args, c.flag, got, c.want)
		}
	}
}

func TestHasFlagValue(t *testing.T) {
	if !cliframework.HasFlagValue([]string{"--replace", "zhipu"}, "--replace") {
		t.Error("--replace present should be true")
	}
	if cliframework.HasFlagValue([]string{"zhipu"}, "--replace") {
		t.Error("missing --replace should be false")
	}
	// --replace=anything still counts as present
	if !cliframework.HasFlagValue([]string{"--replace=true"}, "--replace") {
		t.Error("--replace=true should be true")
	}
}

// --- maybeReloadDaemon is a no-op when no daemon/pid file exists ---

// TestMaybeReloadDaemon_NoOpWithoutPidFile pins the contract that
// maybeReloadDaemon returns silently (no error, no fatal) when no pid file
// exists — the foreground/test case. We point HOME at an empty temp dir so
// resolveLogFile lands in a path with no pid file.
func TestMaybeReloadDaemon_NoOpWithoutPidFile(t *testing.T) {
	setPoolHome(t, t.TempDir())
	// No config file either — maybeReloadDaemon must tolerate LoadConfig failure
	// and still be a no-op (no panic, no fatal).
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("maybeReloadDaemon panicked: %v", r)
		}
	}()
	maybeReloadDaemon(nil)
}

// --- aqp/codex CLI login path-key regression ---
//
// Bug: cmdCodexLogin/runLogin previously hardcoded cliframework.AuthFilePath("aqp"/"codex",
// "oauth_auth"), so a renamed instance (provider_id=codex, name=codex-work)
// wrote codex_oauth_auth.json — a file the forward path (buildOne, which uses
// the config name) never read → 401/502 after login. The fix threads provName
// through. These flows are interactive (browser/device-flow) and not unit-run
// by convention, so this test guards the fix at the source level: the login
// functions MUST resolve the auth file from provName, not the provider_id.
func TestAqpCodexLogin_UsesConfigNameForAuthFile(t *testing.T) {
	src, err := os.ReadFile("internal/cli/login/login.go")
	if err != nil {
		t.Skip("source not readable:", err)
	}
	if !strings.Contains(string(src), `provName+"_oauth_auth.json"`) {
		t.Errorf("internal/cli/login/login.go runLogin must resolve the auth file from provName, not the hardcoded provider_id")
	}
	csrc, err := os.ReadFile("internal/cli/login/codex_login.go")
	if err != nil {
		t.Skip("source not readable:", err)
	}
	if !strings.Contains(string(csrc), `provName+"_oauth_auth.json"`) {
		t.Errorf("internal/cli/login/codex_login.go cmdCodexLogin must resolve the auth file from provName, not the hardcoded provider_id")
	}
	// And the hardcoded forms must be GONE (the bug).
	for _, bad := range []string{`"aqp_oauth_auth.json"`, `"codex_oauth_auth.json"`} {
		if strings.Contains(string(src), bad) || strings.Contains(string(csrc), bad) {
			t.Errorf("hardcoded provider_id auth path %q still present (the bug)", bad)
		}
	}
}

// --- login validates API keys for kimi-code and volcengine ---

// TestValidateKeyBearerGET pins the shared key-validation gate's semantics: a
// no-op on empty url, nil on 200, an error on 401/403 (the two statuses that
// mean "key rejected"). Green-signal guard: getting the accept/reject boundary
// wrong would silently pass bad keys or reject valid ones.
func TestValidateKeyBearerGET(t *testing.T) {
	if err := clilogin.ValidateKeyBearerGET("", "k"); err != nil {
		t.Errorf("empty url: want nil, got %v", err)
	}
	statusCase := func(code int) {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		}))
		defer srv.Close()
		err := clilogin.ValidateKeyBearerGET(srv.URL, "k")
		switch code {
		case 200:
			if err != nil {
				t.Errorf("HTTP %d: want nil, got %v", code, err)
			}
		case 401, 403:
			if err == nil || !strings.Contains(err.Error(), "validation failed") {
				t.Errorf("HTTP %d: want 'validation failed', got %v", code, err)
			}
		}
	}
	statusCase(200)
	statusCase(401)
	statusCase(403)
}

// TestRunApiKeyLogin_KimiCode_Validation401 pins that kimi-code login now
// validates the key (it routes through the generic addApikeyAccount, which gates
// on usage_url). A 401 from the usage endpoint rejects before the pool is written.
func TestRunApiKeyLogin_KimiCode_Validation401(t *testing.T) {
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

	cfg := &Config{Providers: map[string]Provider{"kimi-code": {Provider: "kimi-code", UsageURL: srv.URL}}}
	err := runApiKeyLogin(cfg, "kimi-code", cfg.Providers["kimi-code"])
	if err == nil || !strings.Contains(err.Error(), "validation failed") {
		t.Errorf("kimi-code 401: err=%v want 'validation failed'", err)
	}
	pool, _ := loadPool("kimi-code", "kimi-code")
	if len(pool.Accounts) != 0 {
		t.Errorf("kimi-code 401 should not save: %+v", pool.Accounts)
	}
}

// TestAddVolcengineAccountCore_ArkKeyValidation401 pins that volcengine login
// validates the Ark API Key via usage_url (a Bearer GET to /models): a 401
// rejects before the triple is saved.
func TestAddVolcengineAccountCore_ArkKeyValidation401(t *testing.T) {
	setPoolHome(t, t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer bad-ark" {
			t.Errorf("validation sent Authorization=%q, want Bearer bad-ark", r.Header.Get("Authorization"))
		}
		w.WriteHeader(401)
		w.Write([]byte(`unauthorized`))
	}))
	defer srv.Close()
	cfg, _ := LoadConfigFromBytes("test", []byte("providers:\n  vol:\n    provider_id: volcengine\n    openai_base_url: https://x\n    usage_url: "+srv.URL+"\n"))
	prov := cfg.Providers["vol"]
	// No AK/SK — isolates the Ark-key path.
	_, err := clilogin.AddVolcengineAccount(cfg, "vol", prov, accountCred{APIKey: "bad-ark"}, "", false)
	if err == nil || !strings.Contains(err.Error(), "validation failed") {
		t.Fatalf("Ark 401: err=%v want 'validation failed'", err)
	}
	pool, _ := loadPool("vol", "volcengine")
	if len(pool.Accounts) != 0 {
		t.Fatalf("Ark 401 should not save: %+v", pool.Accounts)
	}
}

// TestAddVolcengineAccountCore_ArkKeyValid pins that a 200 from /models accepts
// the Ark key and saves the triple (chat-only, no AK/SK).
func TestAddVolcengineAccountCore_ArkKeyValid(t *testing.T) {
	setPoolHome(t, t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	cfg, _ := LoadConfigFromBytes("test", []byte("providers:\n  vol:\n    provider_id: volcengine\n    openai_base_url: https://x\n    usage_url: "+srv.URL+"\n"))
	prov := cfg.Providers["vol"]
	id, err := clilogin.AddVolcengineAccount(cfg, "vol", prov, accountCred{APIKey: "good-ark"}, "lbl", false)
	if err != nil {
		t.Fatalf("Ark 200: %v", err)
	}
	if id == "" {
		t.Fatal("empty id")
	}
	pool, _ := loadPool("vol", "volcengine")
	if len(pool.Accounts) != 1 || pool.Accounts[0].APIKey != "good-ark" || pool.Accounts[0].Label != "lbl" {
		t.Fatalf("Ark 200 should save triple: %+v", pool.Accounts)
	}
}

// TestAddVolcengineAccountCore_AKSKValidationFail pins that the AK/SK pair is
// validated (via the clilogin.VolcengineAKSKValidator seam, stubbed here to fail) when
// both are present, and that a failure rejects before save. Asserts the stub WAS
// called — a count-only check would miss a "never validated" bug.
func TestAddVolcengineAccountCore_AKSKValidationFail(t *testing.T) {
	setPoolHome(t, t.TempDir())
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer up.Close()
	cfg, _ := LoadConfigFromBytes("test", []byte("providers:\n  vol:\n    provider_id: volcengine\n    openai_base_url: https://x\n    usage_url: "+up.URL+"\n"))
	prov := cfg.Providers["vol"]

	orig := clilogin.VolcengineAKSKValidator
	defer func() { clilogin.VolcengineAKSKValidator = orig }()
	called := false
	clilogin.VolcengineAKSKValidator = func(ak, sk string) error {
		called = true
		if ak != "AK9" || sk != "SK9" {
			t.Errorf("validator got ak=%q sk=%q, want AK9/SK9", ak, sk)
		}
		return fmt.Errorf("GetAFPUsage HTTP 401: signature mismatch")
	}

	_, err := clilogin.AddVolcengineAccount(cfg, "vol", prov, accountCred{APIKey: "good-ark", AccessKey: "AK9", SecretKey: "SK9"}, "", false)
	if err == nil || !strings.Contains(err.Error(), "validation failed") {
		t.Fatalf("AK/SK fail: err=%v want 'validation failed'", err)
	}
	if !called {
		t.Fatal("AK/SK validator was not called")
	}
	pool, _ := loadPool("vol", "volcengine")
	if len(pool.Accounts) != 0 {
		t.Fatalf("AK/SK fail should not save: %+v", pool.Accounts)
	}
}

// TestAddVolcengineAccountCore_AKSKEmptySkips pins the optional-AK/SK rule:
// when AK/SK are absent (chat-only), the validator is NOT called and a valid Ark
// key is saved. A sentinel stub fails the test if invoked.
func TestAddVolcengineAccountCore_AKSKEmptySkips(t *testing.T) {
	setPoolHome(t, t.TempDir())
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer up.Close()
	cfg, _ := LoadConfigFromBytes("test", []byte("providers:\n  vol:\n    provider_id: volcengine\n    openai_base_url: https://x\n    usage_url: "+up.URL+"\n"))
	prov := cfg.Providers["vol"]

	orig := clilogin.VolcengineAKSKValidator
	defer func() { clilogin.VolcengineAKSKValidator = orig }()
	clilogin.VolcengineAKSKValidator = func(ak, sk string) error {
		t.Fatal("validator must not be called when AK/SK absent")
		return nil
	}

	if _, err := clilogin.AddVolcengineAccount(cfg, "vol", prov, accountCred{APIKey: "good-ark"}, "", false); err != nil {
		t.Fatalf("chat-only login should succeed: %v", err)
	}
	pool, _ := loadPool("vol", "volcengine")
	if len(pool.Accounts) != 1 || pool.Accounts[0].APIKey != "good-ark" {
		t.Fatalf("chat-only should save: %+v", pool.Accounts)
	}
}

// TestAddVolcengineAccountCore_PartialAKSKRejected pins the both-or-neither rule:
// a lone AccessKey or lone SecretKey is rejected up front with "both be set"
// (a partial pair can't sign GetAFPUsage yet previously saved silently and
// degraded to BillingUnknown). Neither-set still succeeds (chat-only). A sentinel
// stub also proves the validator never runs for partial or empty pairs.
func TestAddVolcengineAccountCore_PartialAKSKRejected(t *testing.T) {
	setPoolHome(t, t.TempDir())
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer up.Close()
	cfg, _ := LoadConfigFromBytes("test", []byte("providers:\n  vol:\n    provider_id: volcengine\n    openai_base_url: https://x\n    usage_url: "+up.URL+"\n"))
	prov := cfg.Providers["vol"]

	orig := clilogin.VolcengineAKSKValidator
	defer func() { clilogin.VolcengineAKSKValidator = orig }()
	clilogin.VolcengineAKSKValidator = func(string, string) error {
		t.Error("validator must not run for a partial or empty AK/SK pair")
		return nil
	}

	for _, cred := range []accountCred{
		{APIKey: "good-ark", AccessKey: "AK9"}, // lone AK
		{APIKey: "good-ark", SecretKey: "SK9"}, // lone SK
	} {
		_, err := clilogin.AddVolcengineAccount(cfg, "vol", prov, cred, "", false)
		if err == nil || !strings.Contains(err.Error(), "both be set") {
			t.Errorf("partial %+v: err=%v want 'both be set'", cred, err)
		}
		pool, _ := loadPool("vol", "volcengine")
		if len(pool.Accounts) != 0 {
			t.Errorf("partial pair must not save: %+v", pool.Accounts)
		}
	}

	// Neither set → chat-only success; pool written.
	if _, err := clilogin.AddVolcengineAccount(cfg, "vol", prov, accountCred{APIKey: "good-ark"}, "", false); err != nil {
		t.Fatalf("chat-only (no AK/SK): %v", err)
	}
	pool, _ := loadPool("vol", "volcengine")
	if len(pool.Accounts) != 1 || pool.Accounts[0].APIKey != "good-ark" {
		t.Fatalf("chat-only should save: %+v", pool.Accounts)
	}
}

// TestDefaultsHaveValidationURLs is a source-level guard: kimi-code and
// volcengine MUST carry a usage_url in both the repo config.yaml and the
// `config init` template (defaults.go), or login silently skips validation for
// them (the gap this change closes). Pattern of TestAqpCodexLogin_UsesConfigNameForAuthFile.
func TestDefaultsHaveValidationURLs(t *testing.T) {
	for _, file := range []string{"config.yaml", "defaults.go"} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Skipf("%s not readable: %v", file, err)
		}
		s := string(src)
		if !strings.Contains(s, "usage_url: https://api.kimi.com/coding/v1/usages") {
			t.Errorf("%s: kimi-code usage_url missing (login would skip validation)", file)
		}
		if !strings.Contains(s, "usage_url: https://ark.cn-beijing.volces.com/api/plan/v3/models") {
			t.Errorf("%s: volcengine usage_url missing (login would skip validation)", file)
		}
	}
}

func TestApiKeyValidationURL_Fallback(t *testing.T) {
	cases := []struct {
		name string
		prov Provider
		want string
	}{
		{"usage_url wins", Provider{UsageURL: "https://x/balance", OpenAIBaseURL: "https://x/v1"}, "https://x/balance"},
		{"openai_base_url/models when no usage_url", Provider{UsageURL: "", OpenAIBaseURL: "https://token-plan.cn-beijing.maas.aliyuncs.com/compatible-mode/v1"}, "https://token-plan.cn-beijing.maas.aliyuncs.com/compatible-mode/v1/models"},
		{"trailing slash trimmed", Provider{UsageURL: "", OpenAIBaseURL: "https://x/v1/"}, "https://x/v1/models"},
		{"empty when neither set", Provider{UsageURL: "", OpenAIBaseURL: ""}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := clilogin.ApiKeyValidationURL(c.prov); got != c.want {
				t.Errorf("apiKeyValidationURL = %q, want %q", got, c.want)
			}
		})
	}
}

// stubVolcengineValidator no-ops AK/SK validation for pool dedup/save tests.
func stubVolcengineValidator(t *testing.T) {
	t.Helper()
	orig := clilogin.VolcengineAKSKValidator
	clilogin.VolcengineAKSKValidator = func(string, string) error { return nil }
	t.Cleanup(func() { clilogin.VolcengineAKSKValidator = orig })
}
