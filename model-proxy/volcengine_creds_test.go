package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"model-proxy/provider"
)

// volcengine_creds_test.go covers loadVolcengineCreds + showVolcengineUsage's
// no-AK/SK fallback path (prints configured models + a note), PLUS the Task-10
// per-account AK/SK cred threading (pool login, resolveVolcengineAKSK,
// fetchVolcengineQuota/showVolcengineUsage bound to the virtual's own cred).

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

// --- resolveVolcengineAKSK: cred wins, file fallback, neither → error ---

// TestResolveVolcengineAKSK_CredWins pins the load-bearing isolation rule: when
// a cred with AK/SK is passed, it is used EXCLUSIVELY — the on-disk file (which
// may belong to a DIFFERENT account) is never consulted. Deleting the cred-read
// branch or swapping it to read the file first makes this test red.
func TestResolveVolcengineAKSK_CredWins(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	// On-disk file holds a DIFFERENT account's AK/SK — must be ignored.
	os.WriteFile(filepath.Join(dir, ".model-proxy", "volcengine_apikey.json"),
		[]byte(`{"api_key":"file-key","access_key":"FILE-AK","secret_key":"FILE-SK"}`), 0o600)
	cred := &accountCred{APIKey: "k1", AccessKey: "CRED-AK", SecretKey: "CRED-SK"}
	ak, sk, err := resolveVolcengineAKSK("volcengine", cred)
	if err != nil {
		t.Fatalf("cred non-empty: want no error, got %v", err)
	}
	if ak != "CRED-AK" || sk != "CRED-SK" {
		t.Errorf("resolveVolcengineAKSK cred = (%q,%q), want (CRED-AK,CRED-SK) — cred must win over file", ak, sk)
	}
}

// TestResolveVolcengineAKSK_FileFallback: cred is nil → read the legacy file.
// (single-account / pre-pool path; backward compatible).
func TestResolveVolcengineAKSK_FileFallback(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	os.WriteFile(filepath.Join(dir, ".model-proxy", "volcengine_apikey.json"),
		[]byte(`{"api_key":"ark","access_key":"FILE-AK","secret_key":"FILE-SK"}`), 0o600)
	ak, sk, err := resolveVolcengineAKSK("volcengine", nil)
	if err != nil {
		t.Fatalf("nil cred with file: want no error, got %v", err)
	}
	if ak != "FILE-AK" || sk != "FILE-SK" {
		t.Errorf("resolveVolcengineAKSK file = (%q,%q), want (FILE-AK,FILE-SK)", ak, sk)
	}
}

// TestResolveVolcengineAKSK_CredIncomplete_NoFileFallback: a non-nil cred with
// empty AK/SK must NOT fall back to the file (that would break per-account
// isolation by reading a sibling account's key). It returns an error instead.
func TestResolveVolcengineAKSK_CredIncomplete_NoFileFallback(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	os.WriteFile(filepath.Join(dir, ".model-proxy", "volcengine_apikey.json"),
		[]byte(`{"api_key":"file-key","access_key":"FILE-AK","secret_key":"FILE-SK"}`), 0o600)
	cred := &accountCred{APIKey: "k1"} // AK/SK empty
	_, _, err := resolveVolcengineAKSK("volcengine", cred)
	if err == nil {
		t.Fatal("incomplete cred should error, not fall back to file")
	}
}

// TestResolveVolcengineAKSK_NeitherMissing: nil cred + no file → error.
func TestResolveVolcengineAKSK_NeitherMissing(t *testing.T) {
	setPoolHome(t, t.TempDir())
	_, _, err := resolveVolcengineAKSK("volcengine", nil)
	if err == nil {
		t.Fatal("nil cred + no file: want error, got nil")
	}
}

// TestFetchVolcengineQuota_BoundCredUsesCredAK: when a cred with AK/SK is
// passed, fetchVolcengineQuota uses it (not the file). We assert the cred path
// is taken by writing a file with EMPTY AK/SK — if the function consulted the
// file it would return "AK/SK not configured"; with the cred it instead
// attempts a real GetAFPUsage (which fails on a dead proxy → non-empty Err that
// is NOT "AK/SK not configured"). This proves cred-first resolution.
func TestFetchVolcengineQuota_BoundCredUsesCredAK(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	t.Setenv("HTTPS_PROXY", deadProxyURL(t))
	t.Setenv("HTTP_PROXY", deadProxyURL(t))
	// File has NO AK/SK — would yield "AK/SK not configured" if consulted.
	os.WriteFile(filepath.Join(dir, ".model-proxy", "volcengine_apikey.json"),
		[]byte(`{"api_key":"ark"}`), 0o600)
	cred := &accountCred{APIKey: "k1", AccessKey: "CRED-AK", SecretKey: "CRED-SK"}
	s, err := fetchVolcengineQuota("volcengine", cred)
	if err != nil {
		t.Fatal(err)
	}
	if s.Billing != provider.BillingUnknown {
		t.Fatalf("Billing = %v, want BillingUnknown (call failed on dead proxy)", s.Billing)
	}
	if s.Err == "" || s.Err == "AK/SK not configured" {
		t.Fatalf("Err = %q — cred path NOT taken (would be 'AK/SK not configured' only if file were consulted); want a GetAFPUsage transport error", s.Err)
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
