package accounts

import (
	"encoding/json"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"
)

// oauthPoolFixture writes a two-account OAuth pool through the store (file or
// keychain backend, whichever the process default is at call time).
func oauthPoolFixture(store Store) Pool {
	return Pool{Version: 1, Accounts: []Account{
		{
			ID: OAuthAccountID("one@x.com"), Kind: AccountKindOAuth, Label: "one", Email: "one@x.com",
			OAuth: []byte(`{"account_id":"one@x.com","sso_session_cookie":"SSO_C=one"}`), AddedAt: "2026-07-15T00:00:00Z",
		},
		{
			ID: OAuthAccountID("two@x.com"), Kind: AccountKindOAuth, Label: "two", Email: "two@x.com",
			OAuth: []byte(`{"account_id":"two@x.com","sso_session_cookie":"SSO_C=two"}`), AddedAt: "2026-07-16T00:00:00Z",
		},
	}}
}

// TestOAuthPoolKeychainRoundTrip covers the OAuth blob's keychain lifecycle
// end to end: a keychain-mode save strips blobs into per-account entries and
// writes a metadata-only pool; loading hydrates them back; removing one
// account deletes exactly its keychain namespace.
func TestOAuthPoolKeychainRoundTrip(t *testing.T) {
	keyring.MockInit()
	t.Cleanup(keyring.MockInit)
	store := NewStoreWithBackend(t.TempDir(), BackendKeychain)
	pool := oauthPoolFixture(store)
	if err := store.Save("aqp", "aqp", pool); err != nil {
		t.Fatal(err)
	}

	// On disk: metadata only — kind/email/label present, blob stripped.
	data := readPoolFile(t, store, "aqp")
	if strings.Contains(string(data), "SSO_C=") {
		t.Fatalf("keychain-mode save leaked blobs into the pool file: %s", data)
	}
	if !strings.Contains(string(data), "kind") || !strings.Contains(string(data), "oauth") {
		t.Fatalf("metadata must keep the kind discriminator: %s", data)
	}

	// Load: hydrate every blob from its keychain entry.
	snapshot, err := store.LoadSnapshot("aqp", "aqp")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Source != SourcePlural || len(snapshot.Pool.Accounts) != 2 {
		t.Fatalf("hydrated snapshot = %+v", snapshot.Pool)
	}
	byID := map[string]Account{}
	for _, a := range snapshot.Pool.Accounts {
		byID[a.ID] = a
	}
	if string(byID[OAuthAccountID("one@x.com")].OAuth) == "" || string(byID[OAuthAccountID("two@x.com")].OAuth) == "" {
		t.Fatalf("hydration left blobs empty: %+v", snapshot.Pool.Accounts)
	}

	// Remove one account: its keychain entry goes, the sibling survives.
	if err := store.RemoveAccount("aqp", "aqp", OAuthAccountID("one@x.com")); err != nil {
		t.Fatal(err)
	}
	snapshot, err = store.LoadSnapshot("aqp", "aqp")
	if err != nil || len(snapshot.Pool.Accounts) != 1 || snapshot.Pool.Accounts[0].ID != OAuthAccountID("two@x.com") {
		t.Fatalf("post-remove snapshot = %+v, %v", snapshot.Pool, err)
	}
}

// TestOAuthPoolKeychainOversizeFallsBackInline pins the explicit-fallback
// contract: a blob exceeding the keychain entry ceiling stays INLINE in the
// 0600 pool file (the codex-archive precedent) instead of failing the pool.
func TestOAuthPoolKeychainOversizeFallsBackInline(t *testing.T) {
	// External precondition: the fallback branch is only reachable on backends
	// with an entry ceiling (darwin ~3000B command-line budget, windows 2560B).
	// Linux/BSD secret service has no practical limit, so ErrEntryTooLarge
	// cannot occur there — the normal keychain roundtrip is that platform's
	// behavior (covered by TestOAuthPoolKeychainRoundTrip).
	if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		t.Skip("platform keychain backend has no entry ceiling")
	}
	keyring.MockInit()
	t.Cleanup(keyring.MockInit)
	store := NewStoreWithBackend(t.TempDir(), BackendKeychain)
	big := make([]byte, 64*1024) // far above any platform ceiling
	for i := range big {
		big[i] = 'x'
	}
	blob, _ := json.Marshal(map[string]string{"blob": string(big)})
	pool := Pool{Version: 1, Accounts: []Account{{
		ID: OAuthAccountID("big@x.com"), Kind: AccountKindOAuth, Label: "big",
		OAuth: blob, AddedAt: "2026-07-15T00:00:00Z",
	}}}
	if err := store.Save("codex", "codex", pool); err != nil {
		t.Fatalf("oversize blob must fall back inline, got save error: %v", err)
	}
	// The metadata file KEEPS the blob (its only copy); the load still works.
	data := readPoolFile(t, store, "codex")
	if !strings.Contains(string(data), "kind") || !strings.Contains(string(data), "blob") {
		t.Fatalf("oversize fallback must keep the blob inline: %s...", string(data)[:120])
	}
	snapshot, err := store.LoadSnapshot("codex", "codex")
	if err != nil || len(snapshot.Pool.Accounts) != 1 {
		t.Fatalf("post-fallback snapshot = %+v, %v", snapshot.Pool, err)
	}
}

// TestOAuthPoolFileBackendPlainRoundTrip: the default file backend keeps
// OAuth blobs inline end to end (no keychain interaction), and normalizePool
// does NOT re-derive OAuth ids on load (the anchored-id contract).
func TestOAuthPoolFileBackendPlainRoundTrip(t *testing.T) {
	store := NewStoreWithBackend(t.TempDir(), BackendFile)
	pool := oauthPoolFixture(store)
	if err := store.Save("aqp", "aqp", pool); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.LoadSnapshot("aqp", "aqp")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Pool.Accounts) != 2 {
		t.Fatalf("snapshot = %+v", snapshot.Pool)
	}
	for _, a := range snapshot.Pool.Accounts {
		if a.ID != OAuthAccountID(a.Email) {
			t.Fatalf("anchored id drifted on load: %+v", a)
		}
		if len(a.OAuth) == 0 {
			t.Fatalf("inline blob lost: %+v", a)
		}
	}
}

func readPoolFile(t *testing.T, store Store, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(store.PoolPath(name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}
