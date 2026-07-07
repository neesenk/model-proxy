package main

import (
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
		got := flagStringValue(c.args, c.flag)
		if got != c.want {
			t.Errorf("case %d: flagStringValue(%v, %q) = %q, want %q", i, c.args, c.flag, got, c.want)
		}
	}
}

func TestHasFlagValue(t *testing.T) {
	if !hasFlagValue([]string{"--replace", "zhipu"}, "--replace") {
		t.Error("--replace present should be true")
	}
	if hasFlagValue([]string{"zhipu"}, "--replace") {
		t.Error("missing --replace should be false")
	}
	// --replace=anything still counts as present
	if !hasFlagValue([]string{"--replace=true"}, "--replace") {
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
