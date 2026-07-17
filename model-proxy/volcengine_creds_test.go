package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// volcengine_creds_test.go covers loadVolcengineCreds + showVolcengineUsage's
// no-AK/SK fallback path (prints configured models + a note), PLUS the pool
// login (runVolcengineLoginWithInput: writes the {api_key, access_key,
// secret_key} triple, dedup by AccessKey). The AK/SK resolution + GetAFPUsage
// fetch are tested directly in the provider package (provider/quota_fetch_test.go).

// stubVolcengineValidator replaces volcengineAKSKValidator with a no-op success
// for the runVolcengineLoginWithInput tests below, which exercise pool dedup/
// save logic (not key validation). The validation decision itself is tested in
// login_cmd_test.go (TestAddVolcengineAccountCore_AKSK*). Restored on test end.
func stubVolcengineValidator(t *testing.T) {
	t.Helper()
	orig := volcengineAKSKValidator
	volcengineAKSKValidator = func(string, string) error { return nil }
	t.Cleanup(func() { volcengineAKSKValidator = orig })
}

// --- loadVolcengineCreds: reads {api_key, access_key, secret_key} ---

func TestLoadVolcengineCreds(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	credDir := filepath.Join(home, ".model-proxy")
	os.MkdirAll(credDir, 0o700)
	os.WriteFile(filepath.Join(credDir, "volcengine_apikey.json"),
		[]byte(`{"api_key":"ark-key","access_key":"ak","secret_key":"sk"}`), 0o600)

	c, err := loadVolcengineCreds("volcengine")
	if err != nil {
		t.Fatal(err)
	}
	if c.APIKey != "ark-key" || c.AccessKey != "ak" || c.SecretKey != "sk" {
		t.Errorf("loadVolcengineCreds=%+v", c)
	}
}

func TestLoadVolcengineCreds_Missing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, err := loadVolcengineCreds("volcengine"); err == nil {
		t.Error("loadVolcengineCreds missing file: want error, got nil")
	}
}

func TestLoadVolcengineCreds_BadJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	credDir := filepath.Join(home, ".model-proxy")
	os.MkdirAll(credDir, 0o700)
	os.WriteFile(filepath.Join(credDir, "volcengine_apikey.json"), []byte(`not-json`), 0o600)
	if _, err := loadVolcengineCreds("volcengine"); err == nil {
		t.Error("loadVolcengineCreds bad JSON: want error, got nil")
	}
}

// --- showVolcengineUsage: no AK/SK → prints configured models + note ---

func TestShowVolcengineUsage_NoAKSK(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no cred file
	cfg := &Config{
		Providers: map[string]Provider{
			"volcengine": {
				OpenAIBaseURL: "http://x", Provider: "volcengine",
				Models: []string{"doubao-seed-2-0-code"},
			},
		},
	}
	out := grabStdout(t, func() {
		showVolcengineUsage(cfg, "volcengine", cfg.Providers["volcengine"], nil)
	})
	if !contains(out, "doubao-seed-2-0-code") {
		t.Errorf("showVolcengineUsage missing configured model:\n%s", out)
	}
	if !contains(out, "AK/SK") && !contains(out, "Agent Plan") {
		t.Errorf("showVolcengineUsage missing AK/SK note:\n%s", out)
	}
}

// --- Task 10: per-account AK/SK in the credential pool ---

// writeVolcenginePool writes a volcengine credential pool (plural file) where
// each account carries the full {api_key, access_key, secret_key} triple. The
// generic writePoolFile helper only writes APIKey, so volcengine needs its own.
func writeVolcenginePool(t *testing.T, name string, accts ...poolAccount) {
	t.Helper()
	p := credentialPool{Version: 1, Accounts: accts}
	if err := savePool(name, p); err != nil {
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
		poolAccount{ID: "AK1", Label: "a", APIKey: "k1", AccessKey: "AK1", SecretKey: "SK1", AddedAt: "x"},
		poolAccount{ID: "AK2", Label: "b", APIKey: "k2", AccessKey: "AK2", SecretKey: "SK2", AddedAt: "x"},
	)
	cfg := &Config{
		Listen: "127.0.0.1:1",
		Providers: map[string]Provider{
			"volcengine": {OpenAIBaseURL: "https://v", Provider: "volcengine"},
		},
	}
	px := NewProxy(cfg)
	if got := len(px.poolIndex["volcengine"]); got != 2 {
		t.Fatalf("poolIndex[volcengine] len = %d, want 2 (%v)", got, px.poolIndex["volcengine"])
	}
	want := map[string]bool{"volcengine#AK1": true, "volcengine#AK2": true}
	for _, vid := range px.poolIndex["volcengine"] {
		if !want[vid] {
			t.Fatalf("unexpected virtual %q (want one of volcengine#AK1/AK2)", vid)
		}
	}
}

// --- runVolcengineLoginWithInput: writes the pool (triple), dedup by AccessKey ---

// TestRunVolcengineLoginWithInput_WritesPoolTriple verifies the pool-aware
// volcengine login writes a pool entry carrying the FULL triple
// {api_key, access_key, secret_key} with id = AccessKey (not the api_key hash).
// Deleting any of the three fields from the saved entry, or keying by api_key,
// turns this red.
func TestRunVolcengineLoginWithInput_WritesPoolTriple(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	stubVolcengineValidator(t)
	cfg := &Config{Listen: "127.0.0.1:1", Providers: map[string]Provider{"volcengine": {Provider: "volcengine"}}}
	prov := cfg.Providers["volcengine"]
	if err := runVolcengineLoginWithInput(cfg, "volcengine", prov,
		"ark-key-1", "AK-ONE", "SK-ONE", "acct-one", false); err != nil {
		t.Fatalf("runVolcengineLoginWithInput: %v", err)
	}
	pool, err := loadPool("volcengine", "volcengine")
	if err != nil {
		t.Fatal(err)
	}
	if len(pool.Accounts) != 1 {
		t.Fatalf("want 1 account, got %d: %+v", len(pool.Accounts), pool.Accounts)
	}
	a := pool.Accounts[0]
	// id keyed by AccessKey (account-level), NOT the api_key hash.
	if a.ID != "AK-ONE" {
		t.Errorf("id = %q, want AK-ONE (access_key)", a.ID)
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
	if err := runVolcengineLoginWithInput(cfg, "volcengine", prov,
		"old-ark", "AK9", "old-sk", "first", false); err != nil {
		t.Fatal(err)
	}
	// Replace SAME AccessKey (AK9) with a new api_key + secret_key + label.
	if err := runVolcengineLoginWithInput(cfg, "volcengine", prov,
		"new-ark", "AK9", "new-sk", "renamed", true /*replace*/); err != nil {
		t.Fatal(err)
	}
	pool, _ := loadPool("volcengine", "volcengine")
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
	if err := runVolcengineLoginWithInput(cfg, "volcengine", prov,
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

	err := runVolcengineLoginWithInput(cfg, "volcengine", prov,
		"new-ark", "AK9", "new-sk", "", false /*replace*/)
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("expected 'cancelled' error, got %v", err)
	}
	// Pool unchanged: old key intact.
	pool, _ := loadPool("volcengine", "volcengine")
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
	if err := runVolcengineLoginWithInput(cfg, "volcengine", prov,
		"ark-1", "AK1", "sk-1", "a", false); err != nil {
		t.Fatal(err)
	}
	if err := runVolcengineLoginWithInput(cfg, "volcengine", prov,
		"ark-2", "AK2", "sk-2", "b", false); err != nil {
		t.Fatal(err)
	}
	pool, _ := loadPool("volcengine", "volcengine")
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
