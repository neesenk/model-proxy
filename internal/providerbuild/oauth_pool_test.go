package providerbuild

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"model-proxy/internal/accounts"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/provider"
)

// writeOAuthPool writes a plural OAuth pool for an aqp/codex provider: each
// seed becomes one OAuth-kind account whose blob is the marshaled AqpAccountData.
func writeOAuthPool(t *testing.T, name, providerID string, seeds ...provider.AqpAccountData) {
	t.Helper()
	p := accounts.Pool{Version: 1}
	for _, seed := range seeds {
		blob, err := json.Marshal(seed)
		if err != nil {
			t.Fatal(err)
		}
		id := seed.AccountID
		if id == "" {
			t.Fatal("seed needs AccountID (the id anchor source)")
		}
		p.Accounts = append(p.Accounts, accounts.Account{
			ID:      accounts.OAuthAccountID(id),
			Kind:    accounts.AccountKindOAuth,
			Label:   seed.Email,
			Email:   seed.Email,
			OAuth:   blob,
			AddedAt: "2026-07-15T00:00:00Z",
		})
	}
	if err := poolStore(t).Save(name, providerID, p); err != nil {
		t.Fatal(err)
	}
}

// TestBuildProviders_UnrollsOAuthPool pins the OAuth pool contract: an aqp
// pool with N accounts unrolls to N virtuals keyed "name#<accountID>", the
// parent is not runnable, and every account's secret joins the OAuth subset
// under a per-account label.
func TestBuildProviders_UnrollsOAuthPool(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeOAuthPool(t, "aqp", "aqp",
		provider.AqpAccountData{AccountID: "one@x.com", Email: "one@x.com", SSOSessionCookie: "SSO_C=one"},
		provider.AqpAccountData{AccountID: "two@x.com", Email: "two@x.com", SSOSessionCookie: "SSO_C=two"},
	)

	cfg := poolConfig("aqp")
	built := BuildProviders(cfg, poolStore(t), testBuildOpts())
	if p := built.Providers["aqp"]; p != nil {
		t.Fatal("parent aqp must not be a runnable key when pooled")
	}
	vidA := "aqp#" + accounts.OAuthAccountID("one@x.com")
	vidB := "aqp#" + accounts.OAuthAccountID("two@x.com")
	if built.Providers[vidA] == nil || built.Providers[vidB] == nil {
		t.Fatalf("virtuals missing: %v", built.Providers)
	}
	if vids := built.PoolIndex["aqp"]; len(vids) != 2 || built.ParentOf[vidA] != "aqp" || built.ParentOf[vidB] != "aqp" {
		t.Fatalf("poolIndex/parentOf wrong: %v / %v", built.PoolIndex, built.ParentOf)
	}
	if accts := built.PoolAccounts["aqp"]; len(accts) != 2 {
		t.Fatalf("poolAccounts = %+v, want 2 entries", accts)
	}
	// Guard secrets: one SSO cookie per account, labeled by virtual id.
	cookies := map[string]string{}
	for _, sec := range built.OAuthSecrets {
		cookies[sec.Label] = sec.Value
	}
	if cookies["oauth:"+vidA] != "SSO_C=one" || cookies["oauth:"+vidB] != "SSO_C=two" {
		t.Fatalf("oauth secrets = %v", cookies)
	}
}

// TestBuildProviders_OAuthLegacyFileStaysBareName: without a pool, the legacy
// single-account <name>_oauth_auth.json keeps serving under the bare name (the
// historical pre-pool behavior — the migration to a pool happens lazily on the
// first login).
func TestBuildProviders_OAuthLegacyFileStaysBareName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	credDir := filepath.Join(home, ".model-proxy")
	os.MkdirAll(credDir, 0o700)
	os.WriteFile(filepath.Join(credDir, "codex_oauth_auth.json"),
		[]byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"at","refresh_token":"rt"}}`), 0o600)

	built := BuildProviders(poolConfig("codex"), poolStore(t), testBuildOpts())
	if built.Providers["codex"] == nil {
		t.Fatal("legacy codex file must keep the bare-name instance")
	}
	if len(built.PoolIndex) != 0 {
		t.Fatalf("no pool file → no pool index, got %v", built.PoolIndex)
	}
	if len(built.OAuthSecrets) == 0 {
		t.Fatal("legacy oauth secrets must still join the guard set")
	}
}

// TestPoolOAuthStore_SaveReplacesOnlyOwnAccount is the rotating-refresh_token
// regression: two accounts share one pool; concurrent Save traffic on slot A
// must never drop or clobber slot B's blob (the write runs inside the pool's
// cross-process lock).
func TestPoolOAuthStore_SaveReplacesOnlyOwnAccount(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := poolStore(t)
	writeOAuthPool(t, "codex", "codex",
		provider.AqpAccountData{AccountID: "a@x.com", SSOSessionCookie: "SSO_C=a"},
	)
	// (aqp-shaped blobs are fine as opaque payloads here; the store is
	// shape-agnostic.)
	idA := accounts.OAuthAccountID("a@x.com")
	idB := "bbbbbbbbbbbbbbbb"
	// Add the second account directly.
	err := store.WithLock("codex", func() error {
		pool, err := store.Load("codex", "codex")
		if err != nil {
			return err
		}
		pool.Accounts = append(pool.Accounts, accounts.Account{
			ID: idB, Kind: accounts.AccountKindOAuth, Label: "b", OAuth: []byte(`{"b":1}`), AddedAt: "2026-07-15T00:00:00Z",
		})
		return store.Save("codex", "codex", pool)
	})
	if err != nil {
		t.Fatal(err)
	}

	slotA := poolOAuthStore{store: store, name: "codex", providerID: "codex", id: idA}
	// A rotation-shaped write: brand-new blob for A.
	if err := slotA.Save([]byte(`{"rotated":"refresh-v2"}`)); err != nil {
		t.Fatal(err)
	}
	pool, err := store.Load("codex", "codex")
	if err != nil || len(pool.Accounts) != 2 {
		t.Fatalf("pool after save = %+v, %v", pool, err)
	}
	// The pool save re-indents RawMessage blobs; compare semantic JSON.
	blobJSON := func(raw []byte) string {
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatalf("blob not JSON: %v (%s)", err, raw)
		}
		out, _ := json.Marshal(v)
		return string(out)
	}
	for _, a := range pool.Accounts {
		switch a.ID {
		case idA:
			if got := blobJSON(a.OAuth); got != `{"rotated":"refresh-v2"}` {
				t.Fatalf("slot A blob = %s", got)
			}
		case idB:
			if got := blobJSON(a.OAuth); got != `{"b":1}` {
				t.Fatalf("slot B blob must be untouched, got %s", got)
			}
		}
	}
	// Load reads the account's own slot; a removed account reads as absent.
	blob, err := slotA.Load()
	if err != nil || blobJSON(blob) != `{"rotated":"refresh-v2"}` {
		t.Fatalf("slot A load = %s, %v", blob, err)
	}
	if err := slotA.Delete(); err != nil {
		t.Fatal(err)
	}
	pool, err = store.Load("codex", "codex")
	if err != nil || len(pool.Accounts) != 1 || pool.Accounts[0].ID != idB {
		t.Fatalf("pool after delete = %+v, %v", pool, err)
	}
	if blob, err := slotA.Load(); err != nil || blob != nil {
		t.Fatalf("removed account must read as absent, got %s, %v", blob, err)
	}
}

// TestCollectOAuthSecrets_PoolEnumeratesPerAccount: the guard refresh loop's
// collector reads POOLED OAuth credentials per account (labeled by virtual
// id) and falls back to the legacy file only for pool-less providers.
func TestCollectOAuthSecrets_PoolEnumeratesPerAccount(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := poolStore(t)
	// Pooled codex: two accounts, codex-shaped blobs (three tokens each).
	writeCodexOAuthBlob := func(accountID, access string) accounts.Account {
		blob, err := json.Marshal(map[string]any{
			"auth_mode": "chatgpt",
			"tokens": map[string]string{
				"access_token": access, "refresh_token": "rt-" + accountID, "id_token": "id-" + accountID,
				"account_id": accountID,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return accounts.Account{
			ID: accounts.OAuthAccountID(accountID), Kind: accounts.AccountKindOAuth,
			Label: accountID, OAuth: blob, AddedAt: "2026-07-15T00:00:00Z",
		}
	}
	pool := accounts.Pool{Version: 1, Accounts: []accounts.Account{
		writeCodexOAuthBlob("acct-1", "at-1"),
		writeCodexOAuthBlob("acct-2", "at-2"),
	}}
	if err := store.Save("codex", "codex", pool); err != nil {
		t.Fatal(err)
	}
	// Pool-less aqp: legacy single-account file.
	credDir := accounts.HomeDir() + "/.model-proxy"
	os.MkdirAll(credDir, 0o700)
	os.WriteFile(credDir+"/aqp_oauth_auth.json",
		[]byte(`{"account_id":"legacy@x.com","sso_session_cookie":"SSO_C=legacy"}`), 0o600)

	cfg := poolConfig("codex", "aqp")
	cfg.Providers["codex"] = configdomain.Provider{OpenAIBaseURL: "http://x", Provider: "codex"}
	// codex emits THREE tokens per account under one label — collect sets.
	got := map[string]map[string]bool{}
	for _, sec := range CollectOAuthSecrets(cfg, testBuildOpts()) {
		if got[sec.Label] == nil {
			got[sec.Label] = map[string]bool{}
		}
		got[sec.Label][sec.Value] = true
	}
	vid1 := "codex#" + accounts.OAuthAccountID("acct-1")
	vid2 := "codex#" + accounts.OAuthAccountID("acct-2")
	for label, want := range map[string][]string{
		"oauth:" + vid1: {"at-1", "rt-acct-1", "id-acct-1"},
		"oauth:" + vid2: {"at-2", "rt-acct-2", "id-acct-2"},
		"oauth:aqp":     {"SSO_C=legacy"},
	} {
		set := got[label]
		if set == nil || len(set) != len(want) {
			t.Fatalf("secret[%s] = %v, want %v", label, set, want)
		}
		for _, v := range want {
			if !set[v] {
				t.Errorf("secret[%s] missing %q (has %v)", label, v, set)
			}
		}
	}
	if len(got) != 3 {
		t.Fatalf("labels = %v, want exactly the three per-account/oauth labels", got)
	}
}
