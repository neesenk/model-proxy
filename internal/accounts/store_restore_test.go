package accounts

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"model-proxy/internal/credstore"
)

// Keychain→file restore tests (config `credentials:` switched back to file
// after a keychain-mode save). Same hermetic setup as the keychain tests:
// keyring.MockInit + t.TempDir, never the real OS keychain. Not parallel: the
// mock is process-global.

// seedKeychainPool writes a metadata-only pool file plus keychain entries via
// the keychain backend — the exact on-disk shape a keychain→file switch
// starts from.
func seedKeychainPool(t *testing.T, dir, name, providerID string, pool Pool) Store {
	t.Helper()
	s := NewStoreWithBackend(dir, BackendKeychain)
	if err := s.Save(name, providerID, pool); err != nil {
		t.Fatalf("seed keychain pool: %v", err)
	}
	return s
}

func TestRestoreFromKeychainFull(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	credA := Credentials{APIKey: "sk-restore-alpha"}
	credB := Credentials{APIKey: "ark-restore-beta", AccessKey: "AK-RESTORE", SecretKey: "SK-RESTORE"}
	pool := Pool{Version: 1, Accounts: []Account{
		{ID: AccountID("volcengine", credA), Label: "alpha", APIKey: credA.APIKey, AddedAt: "2026-08-26"},
		{ID: AccountID("volcengine", credB), Label: "beta", APIKey: credB.APIKey, AccessKey: credB.AccessKey, SecretKey: credB.SecretKey, AddedAt: "2026-08-26"},
	}}
	idA, idB := pool.Accounts[0].ID, pool.Accounts[1].ID
	seedKeychainPool(t, dir, "prov", "volcengine", pool)

	// File backend: restore hydrates every entry and rewrites plaintext.
	s := NewStoreWithBackend(dir, BackendFile)
	snapshot, err := s.LoadSnapshot("prov", "volcengine")
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if snapshot.Source != SourcePlural || len(snapshot.Pool.Accounts) != 2 {
		t.Fatalf("snapshot = %+v, want 2-account SourcePlural", snapshot)
	}
	if len(snapshot.ReloginNeeded) != 0 {
		t.Fatalf("ReloginNeeded = %+v, want none on full restore", snapshot.ReloginNeeded)
	}
	byID := map[string]Account{}
	for _, a := range snapshot.Pool.Accounts {
		byID[a.ID] = a
	}
	if byID[idA].APIKey != credA.APIKey {
		t.Fatalf("restored api_key = %q", byID[idA].APIKey)
	}
	if b := byID[idB]; b.APIKey != credB.APIKey || b.AccessKey != credB.AccessKey || b.SecretKey != credB.SecretKey {
		t.Fatalf("restored volcengine tuple = %+v", b.Credentials())
	}

	// The pool file is plaintext again at 0600 — a re-Load no longer touches
	// the keychain (it is a normal file-mode pool now).
	content := poolFileBytes(t, s, "prov")
	for _, secret := range []string{credA.APIKey, credB.APIKey, credB.AccessKey, credB.SecretKey} {
		if !strings.Contains(content, secret) {
			t.Fatalf("restored pool file missing secret %q", secret)
		}
	}
	info, err := os.Stat(s.PoolPath("prov"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("restored pool perms = %#o, want 600", perm)
	}
	reloaded, err := s.Load("prov", "volcengine")
	if err != nil || len(reloaded.Accounts) != 2 {
		t.Fatalf("re-Load after restore: (%+v, %v)", reloaded, err)
	}

	// Keychain entries are KEPT (logout is the normal deletion path).
	if got := keychainEntry(t, "prov", idA, keychainFieldAPIKey); got != credA.APIKey {
		t.Fatalf("keychain entry deleted on restore: api_key = %q", got)
	}
	if got := keychainEntry(t, "prov", idB, keychainFieldSecretKey); got != credB.SecretKey {
		t.Fatalf("keychain entry deleted on restore: secret_key = %q", got)
	}

	// File mode works normally after restore: add and remove accounts.
	kept := reloaded.Accounts[0]
	if err := s.Save("prov", "volcengine", Pool{Version: 1, Accounts: []Account{kept}}); err != nil {
		t.Fatalf("file-mode Save after restore: %v", err)
	}
	after, err := s.Load("prov", "volcengine")
	if err != nil || len(after.Accounts) != 1 || after.Accounts[0].ID != kept.ID {
		t.Fatalf("file-mode delete-after-restore: (%+v, %v)", after, err)
	}
}

func TestRestoreFromKeychainPartialKeepsMetadataAndReports(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	credA := Credentials{APIKey: "sk-partial-alpha"}
	credB := Credentials{APIKey: "sk-partial-gone"}
	pool := Pool{Version: 1, Accounts: []Account{
		{ID: AccountID("zhipu", credA), Label: "alpha", APIKey: credA.APIKey, AddedAt: "2026-08-26"},
		{ID: AccountID("zhipu", credB), Label: "gone", APIKey: credB.APIKey, AddedAt: "2026-08-26"},
	}}
	idA, idB := pool.Accounts[0].ID, pool.Accounts[1].ID
	seedKeychainPool(t, dir, "prov", "zhipu", pool)
	// One account lost its keychain entry (deleted out of band).
	if err := credstore.KeychainDelete(keychainKey("prov", idB, keychainFieldAPIKey)); err != nil {
		t.Fatalf("delete keychain entry: %v", err)
	}

	s := NewStoreWithBackend(dir, BackendFile)
	snapshot, err := s.LoadSnapshot("prov", "zhipu")
	if err != nil {
		t.Fatalf("partial restore must not fail the pool: %v", err)
	}
	if len(snapshot.Pool.Accounts) != 1 || snapshot.Pool.Accounts[0].ID != idA {
		t.Fatalf("restored pool = %+v, want only the restorable account", snapshot.Pool)
	}
	if snapshot.Pool.Accounts[0].APIKey != credA.APIKey {
		t.Fatalf("restored api_key = %q", snapshot.Pool.Accounts[0].APIKey)
	}
	// The missing account is reported with its metadata for re-login.
	if len(snapshot.ReloginNeeded) != 1 {
		t.Fatalf("ReloginNeeded = %+v, want the missing account", snapshot.ReloginNeeded)
	}
	if got := snapshot.ReloginNeeded[0]; got.ID != idB || got.Label != "gone" || got.AddedAt != "2026-08-26" {
		t.Fatalf("ReloginNeeded metadata = %+v, want id/label/added_at of the missing account", got)
	}

	// Partial restore keeps the metadata file untouched: no plaintext secret
	// on disk, BOTH account ids still recorded for the next report round.
	content := poolFileBytes(t, s, "prov")
	if strings.Contains(content, credA.APIKey) {
		t.Fatal("partial restore must not rewrite the pool file with plaintext")
	}
	if !strings.Contains(content, idA) || !strings.Contains(content, idB) {
		t.Fatalf("metadata file lost account ids after partial restore: %s", content)
	}

	// Re-login of the missing account converges the pool to plaintext.
	rel := snapshot.ReloginNeeded[0]
	rel.APIKey = "sk-partial-relogin"
	rel.ID = AccountID("zhipu", rel.Credentials())
	newPool := Pool{Version: 1, Accounts: append(snapshot.Pool.Accounts, rel)}
	if err := s.Save("prov", "zhipu", newPool); err != nil {
		t.Fatalf("file-mode Save after partial restore: %v", err)
	}
	after, err := s.Load("prov", "zhipu")
	if err != nil || len(after.Accounts) != 2 {
		t.Fatalf("Load after re-login: (%+v, %v)", after, err)
	}
}

func TestRestoreFromKeychainNothingRestorable(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	cred := Credentials{APIKey: "sk-unreachable"}
	pool := Pool{Version: 1, Accounts: []Account{
		{ID: AccountID("zhipu", cred), Label: "only", APIKey: cred.APIKey, AddedAt: "2026-08-26"},
	}}
	seedKeychainPool(t, dir, "prov", "zhipu", pool)
	// Backend dies before the file-mode read (entries exist but unreachable).
	keyring.MockInitWithError(errors.New("secret service down"))
	t.Cleanup(keyring.MockInit)

	s := NewStoreWithBackend(dir, BackendFile)
	snapshot, err := s.LoadSnapshot("prov", "zhipu")
	if err != nil {
		t.Fatalf("unreachable keychain must not fail file-mode load: %v", err)
	}
	if snapshot.Source != SourcePlural || len(snapshot.Pool.Accounts) != 0 {
		t.Fatalf("snapshot = %+v, want empty SourcePlural pool", snapshot)
	}
	if len(snapshot.ReloginNeeded) != 1 || snapshot.ReloginNeeded[0].Label != "only" {
		t.Fatalf("ReloginNeeded = %+v, want the unrestorable account", snapshot.ReloginNeeded)
	}
	// Metadata file intact: the account is still recorded for re-login.
	if content := poolFileBytes(t, s, "prov"); !strings.Contains(content, "only") {
		t.Fatalf("metadata file lost the unrestorable account: %s", content)
	}
}

func TestRestoreFromKeychainMixedPoolStillRejected(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	s := NewStoreWithBackend(dir, BackendFile)
	// One entry with a secret, one without: not a mode-switch artifact, so no
	// restore is attempted and validatePool rejects it as before.
	body := `{"version":1,"accounts":[{"id":"one","api_key":"sk-x"},{"id":"two","api_key":""}]}`
	if err := os.MkdirAll(filepath.Join(dir, ".model-proxy"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.PoolPath("prov"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadSnapshot("prov", "zhipu"); err == nil || !strings.Contains(err.Error(), "api_key is empty") {
		t.Fatalf("mixed pool LoadSnapshot = %v, want validate rejection", err)
	}
	if content := poolFileBytes(t, s, "prov"); content != body {
		t.Fatal("rejected mixed pool must not be rewritten")
	}
}

func TestSetProcessCredentialsModeAppliesPoolBackend(t *testing.T) {
	SetProcessCredentialsMode("keychain")
	t.Cleanup(func() { SetProcessCredentialsMode("file") })
	if got := NewStore(t.TempDir()).Backend(); got != BackendKeychain {
		t.Fatalf("NewStore backend after SetProcessCredentialsMode(keychain) = %v, want keychain", got)
	}
	SetProcessCredentialsMode("file")
	if got := NewStore(t.TempDir()).Backend(); got != BackendFile {
		t.Fatalf("NewStore backend after SetProcessCredentialsMode(file) = %v, want file", got)
	}
}
