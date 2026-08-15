package app

import (
	"model-proxy/internal/accounts"
	cliframework "model-proxy/internal/cli/framework"
	clilogin "model-proxy/internal/cli/login"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// volcengine_creds_test.go covers loadVolcengineCreds and the pool login
// (runVolcengineLoginWithInput: writes the {api_key, access_key, secret_key}
// triple, dedup by AccessKey). Usage display and quota behavior belong to the
// provider package.

// stubVolcengineValidator replaces clilogin.VolcengineAKSKValidator with a no-op success
// for the runVolcengineLoginWithInput tests below, which exercise pool dedup/
// save logic (not key validation). The validation decision itself is tested in
// login_cmd_test.go (TestAddVolcengineAccountCore_AKSK*). Restored on test end.
func stubVolcengineValidator(t *testing.T) {
	t.Helper()
	orig := clilogin.VolcengineAKSKValidator
	clilogin.VolcengineAKSKValidator = func(string, string) error { return nil }
	t.Cleanup(func() { clilogin.VolcengineAKSKValidator = orig })
}

// --- loadVolcengineCreds: reads {api_key, access_key, secret_key} ---

func TestLoadVolcengineCreds(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	credDir := filepath.Join(home, ".model-proxy")
	os.MkdirAll(credDir, 0o700)
	os.WriteFile(filepath.Join(credDir, "volcengine_apikey.json"),
		[]byte(`{"api_key":"ark-key","access_key":"ak","secret_key":"sk"}`), 0o600)

	c, err := LoadVolcengineCreds(cliframework.HomeDir(), "volcengine")
	if err != nil {
		t.Fatal(err)
	}
	if c.APIKey != "ark-key" || c.AccessKey != "ak" || c.SecretKey != "sk" {
		t.Errorf("loadVolcengineCreds=%+v", c)
	}
}

func TestLoadVolcengineCreds_Missing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, err := LoadVolcengineCreds(cliframework.HomeDir(), "volcengine"); err == nil {
		t.Error("loadVolcengineCreds missing file: want error, got nil")
	}
}

func TestLoadVolcengineCreds_BadJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	credDir := filepath.Join(home, ".model-proxy")
	os.MkdirAll(credDir, 0o700)
	os.WriteFile(filepath.Join(credDir, "volcengine_apikey.json"), []byte(`not-json`), 0o600)
	if _, err := LoadVolcengineCreds(cliframework.HomeDir(), "volcengine"); err == nil {
		t.Error("loadVolcengineCreds bad JSON: want error, got nil")
	}
}

// --- Task 10: per-account AK/SK in the credential pool ---

// writeVolcenginePool writes a volcengine credential pool (plural file) where
// each account carries the full {api_key, access_key, secret_key} triple. The
// generic writePoolFile helper only writes APIKey, so volcengine needs its own.
func writeVolcenginePool(t *testing.T, name string, accts ...PoolAccount) {
	t.Helper()
	p := CredentialPool{Version: 1, Accounts: accts}
	if err := SavePool(name, "volcengine", p); err != nil {
		t.Fatal(err)
	}
}

// TestVolcenginePoolPerAccountAK verifies a 2-account volcengine pool (DISTINCT
// AccessKeys) is unrolled into 2 virtuals keyed by access_key. The virtual id
// suffix MUST be the AccessKey (account-level), not the api_key hash.
func TestVolcenginePoolPerAccountAK(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	writeVolcenginePool(t, "volcengine",
		PoolAccount{ID: "AK1", Label: "a", APIKey: "k1", AccessKey: "AK1", SecretKey: "SK1", AddedAt: "x"},
		PoolAccount{ID: "AK2", Label: "b", APIKey: "k2", AccessKey: "AK2", SecretKey: "SK2", AddedAt: "x"},
	)
	cfg := &Config{
		Listen: "127.0.0.1:1",
		Providers: map[string]Provider{
			"volcengine": {OpenAIBaseURL: "https://v", Provider: "volcengine"},
		},
	}
	px := newTestProxy(t, cfg)
	if got := len(px.poolIndex["volcengine"]); got != 2 {
		t.Fatalf("poolIndex[volcengine] len = %d, want 2 (%v)", got, px.poolIndex["volcengine"])
	}
	want := map[string]bool{
		"volcengine#" + accounts.AccountID("volcengine", accounts.Credentials{AccessKey: "AK1"}): true,
		"volcengine#" + accounts.AccountID("volcengine", accounts.Credentials{AccessKey: "AK2"}): true,
	}
	for _, vid := range px.poolIndex["volcengine"] {
		if !want[vid] {
			t.Fatalf("unexpected virtual %q (want hashed volcengine ids)", vid)
		}
	}
}

// --- runVolcengineLoginWithInput: writes the pool (triple), dedup by AccessKey ---

// TestRunVolcengineLoginWithInput_WritesPoolTriple verifies the pool-aware
// volcengine login writes a pool entry carrying the FULL triple
// {api_key, access_key, secret_key} with id keyed by the AccessKey HASH
// (account-level identity, no credential material — the id reaches logs and
// persisted state). Deleting any of the three fields from the saved entry, or
// keying by api_key, turns this red.
func TestRunVolcengineLoginWithInput_WritesPoolTriple(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	stubVolcengineValidator(t)
	cfg := &Config{Listen: "127.0.0.1:1", Providers: map[string]Provider{"volcengine": {Provider: "volcengine"}}}
	prov := cfg.Providers["volcengine"]
	if err := clilogin.RunVolcengineLoginWithInput(cfg, "volcengine", prov,
		"ark-key-1", "AK-ONE", "SK-ONE", "acct-one", false); err != nil {
		t.Fatalf("runVolcengineLoginWithInput: %v", err)
	}
	pool, err := LoadPool("volcengine", "volcengine")
	if err != nil {
		t.Fatal(err)
	}
	if len(pool.Accounts) != 1 {
		t.Fatalf("want 1 account, got %d: %+v", len(pool.Accounts), pool.Accounts)
	}
	a := pool.Accounts[0]
	// id keyed by the AccessKey hash (account-level identity), NOT the api_key
	// hash and never the raw access key.
	wantID := accounts.AccountID("volcengine", accounts.Credentials{APIKey: "ark-key-1", AccessKey: "AK-ONE", SecretKey: "SK-ONE"})
	if a.ID != wantID {
		t.Errorf("id = %q, want %q (access_key hash)", a.ID, wantID)
	}
	if a.ID == "AK-ONE" || strings.Contains(a.ID, "AK-ONE") {
		t.Errorf("id %q must not carry the raw access key", a.ID)
	}
	if a.APIKey != "ark-key-1" || a.AccessKey != "AK-ONE" || a.SecretKey != "SK-ONE" {
		t.Errorf("saved triple = %+v, want {api_key:ark-key-1 access_key:AK-ONE secret_key:SK-ONE}", a)
	}
	if a.Label != "acct-one" {
		t.Errorf("label = %q, want acct-one", a.Label)
	}
	// The legacy singular file must NOT be written by the pool-aware login.
	if _, err := os.Stat(filepath.Join(dir, ".model-proxy", "volcengine_apikey.json")); !os.IsNotExist(err) {
		t.Errorf("legacy singular file should not exist after pool login (err=%v)", err)
	}
}

// TestRunVolcengineLoginWithInput_DedupByAccessKey verifies that re-logging-in
// the SAME AccessKey with --replace keeps the pool at size 1 and overwrites the
// api_key + secret_key in place (idempotent replace, not append).
func TestRunVolcengineLoginWithInput_DedupByAccessKey(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	stubVolcengineValidator(t)
	cfg := &Config{Listen: "127.0.0.1:1", Providers: map[string]Provider{"volcengine": {Provider: "volcengine"}}}
	prov := cfg.Providers["volcengine"]
	// Seed an account with AccessKey=AK9.
	if err := clilogin.RunVolcengineLoginWithInput(cfg, "volcengine", prov,
		"old-ark", "AK9", "old-sk", "first", false); err != nil {
		t.Fatal(err)
	}
	// Replace SAME AccessKey (AK9) with a new api_key + secret_key + label.
	if err := clilogin.RunVolcengineLoginWithInput(cfg, "volcengine", prov,
		"new-ark", "AK9", "new-sk", "renamed", true /*replace*/); err != nil {
		t.Fatal(err)
	}
	pool, _ := LoadPool("volcengine", "volcengine")
	if len(pool.Accounts) != 1 {
		t.Fatalf("replace same AccessKey should keep size 1, got %d: %+v", len(pool.Accounts), pool.Accounts)
	}
	a := pool.Accounts[0]
	if a.APIKey != "new-ark" || a.SecretKey != "new-sk" || a.Label != "renamed" {
		t.Errorf("replace did not update fields: got %+v", a)
	}
	if a.AccessKey != "AK9" {
		t.Errorf("AccessKey changed on replace: got %q want AK9", a.AccessKey)
	}
}

// TestRunVolcengineLoginWithInput_DedupNoReplace_Aborts: same AccessKey, no
// --replace, stdin says "n" → "login cancelled", pool untouched.
func TestRunVolcengineLoginWithInput_DedupNoReplace_Aborts(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	stubVolcengineValidator(t)
	cfg := &Config{Listen: "127.0.0.1:1", Providers: map[string]Provider{"volcengine": {Provider: "volcengine"}}}
	prov := cfg.Providers["volcengine"]
	if err := clilogin.RunVolcengineLoginWithInput(cfg, "volcengine", prov,
		"old-ark", "AK9", "old-sk", "first", false); err != nil {
		t.Fatal(err)
	}
	// Redirect stdin to answer "n".
	orig := os.Stdin
	r, w, _ := os.Pipe()
	os.Stdin = r
	defer func() { os.Stdin = orig }()
	w.Write([]byte("n\n"))
	w.Close()

	err := clilogin.RunVolcengineLoginWithInput(cfg, "volcengine", prov,
		"new-ark", "AK9", "new-sk", "", false /*replace*/)
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("expected 'cancelled' error, got %v", err)
	}
	// Pool unchanged: old key intact.
	pool, _ := LoadPool("volcengine", "volcengine")
	if len(pool.Accounts) != 1 || pool.Accounts[0].APIKey != "old-ark" {
		t.Fatalf("abort should leave pool untouched: %+v", pool.Accounts)
	}
}

// TestRunVolcengineLoginWithInput_DifferentAccessKeyAppends: a NEW AccessKey
// appends a fresh entry to the pool.
func TestRunVolcengineLoginWithInput_DifferentAccessKeyAppends(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	stubVolcengineValidator(t)
	cfg := &Config{Listen: "127.0.0.1:1", Providers: map[string]Provider{"volcengine": {Provider: "volcengine"}}}
	prov := cfg.Providers["volcengine"]
	if err := clilogin.RunVolcengineLoginWithInput(cfg, "volcengine", prov,
		"ark-1", "AK1", "sk-1", "a", false); err != nil {
		t.Fatal(err)
	}
	if err := clilogin.RunVolcengineLoginWithInput(cfg, "volcengine", prov,
		"ark-2", "AK2", "sk-2", "b", false); err != nil {
		t.Fatal(err)
	}
	pool, _ := LoadPool("volcengine", "volcengine")
	if len(pool.Accounts) != 2 {
		t.Fatalf("different AccessKey should append, got %d: %+v", len(pool.Accounts), pool.Accounts)
	}
	// Both AccessKeys present.
	aks := map[string]bool{}
	for _, a := range pool.Accounts {
		aks[a.AccessKey] = true
	}
	if !aks["AK1"] || !aks["AK2"] {
		t.Fatalf("missing AccessKeys in pool: %+v", pool.Accounts)
	}
}
