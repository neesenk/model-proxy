package main

import (
	"os"
	"path/filepath"
	"testing"
)

// setPoolHome redirects HOME to a temp dir (auto-cleanup via t.Setenv) and
// pre-creates the .model-proxy subdir. Named distinctly to avoid colliding
// with deepseek_forward_test.go's setHome helper.
func setPoolHome(t *testing.T, dir string) {
	t.Helper()
	os.MkdirAll(filepath.Join(dir, ".model-proxy"), 0o700)
	t.Setenv("HOME", dir)
}

func TestAccountIDFor(t *testing.T) {
	z := accountIDFor("zhipu", accountCred{APIKey: "sk-abc"})
	z2 := accountIDFor("zhipu", accountCred{APIKey: "sk-abc"})
	z3 := accountIDFor("zhipu", accountCred{APIKey: "sk-other"})
	if z != z2 {
		t.Fatalf("same key must yield same id: %q vs %q", z, z2)
	}
	if z == z3 {
		t.Fatalf("different keys must yield different ids")
	}
	if len(z) != 16 {
		t.Fatalf("zhipu id len = %d, want 16", len(z))
	}
	// volcengine keys by access_key (account-level), not api_key
	a := accountIDFor("volcengine", accountCred{APIKey: "k1", AccessKey: "AK9"})
	b := accountIDFor("volcengine", accountCred{APIKey: "k2", AccessKey: "AK9"})
	if a != b {
		t.Fatalf("volcengine same access_key must yield same id: %q vs %q", a, b)
	}
	if a != "AK9" {
		t.Fatalf("volcengine id = %q, want AK9", a)
	}
	// access_key empty → fall back to key hash
	c := accountIDFor("volcengine", accountCred{APIKey: "k1"})
	if c == "" || len(c) != 16 {
		t.Fatalf("volcengine fallback id = %q", c)
	}
}

func TestSaveLoadPoolRoundTrip(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	in := credentialPool{Version: 1, Accounts: []poolAccount{
		{ID: "id1", Label: "home", APIKey: "k1", AddedAt: "2026-07-08T00:00:00Z"},
		{ID: "id2", Label: "team", APIKey: "k2", AddedAt: "2026-07-08T00:00:00Z"},
	}}
	if err := savePool("zhipu", in); err != nil {
		t.Fatal(err)
	}
	out, err := loadPool("zhipu", "zhipu")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Accounts) != 2 {
		t.Fatalf("len = %d, want 2", len(out.Accounts))
	}
	if out.Accounts[0].ID != "id1" || out.Accounts[0].APIKey != "k1" || out.Accounts[0].Label != "home" {
		t.Fatalf("account0 = %+v", out.Accounts[0])
	}
}

func TestLoadPoolSingularFallback(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	// Legacy single-key file, no plural pool.
	singular := filepath.Join(dir, ".model-proxy", "zhipu_apikey.json")
	os.WriteFile(singular, []byte(`{"api_key":"legacy-key"}`), 0o600)

	pool, err := loadPool("zhipu", "zhipu")
	if err != nil {
		t.Fatal(err)
	}
	if len(pool.Accounts) != 1 {
		t.Fatalf("fallback len = %d, want 1", len(pool.Accounts))
	}
	if pool.Accounts[0].APIKey != "legacy-key" {
		t.Fatalf("fallback key = %q", pool.Accounts[0].APIKey)
	}
	// id derived from the key.
	if pool.Accounts[0].ID != accountIDFor("zhipu", accountCred{APIKey: "legacy-key"}) {
		t.Fatalf("fallback id not derived from key")
	}
}

func TestLoadPoolEmpty(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	pool, err := loadPool("zhipu", "zhipu")
	if err != nil {
		t.Fatalf("missing pool should be empty, not error: %v", err)
	}
	if len(pool.Accounts) != 0 {
		t.Fatalf("want 0 accounts, got %d", len(pool.Accounts))
	}
}
