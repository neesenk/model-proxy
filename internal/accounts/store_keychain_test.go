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

// Keychain-mode tests run entirely against go-keyring's in-memory mock
// (keyring.MockInit / MockInitWithError) plus t.TempDir files — they never
// touch the real OS keychain. Not parallel: the mock is process-global.

func keychainStore(dir string) Store {
	return NewStoreWithBackend(dir, BackendKeychain)
}

func keychainEntry(t *testing.T, providerName, accountID, field string) string {
	t.Helper()
	value, err := keyring.Get("model-proxy", keychainKey(providerName, accountID, field))
	if err != nil {
		t.Fatalf("keyring.Get(%s/%s/%s): %v", providerName, accountID, field, err)
	}
	return value
}

func poolFileBytes(t *testing.T, s Store, name string) string {
	t.Helper()
	data, err := os.ReadFile(s.PoolPath(name))
	if err != nil {
		t.Fatalf("read pool file: %v", err)
	}
	return string(data)
}

// seedLegacyKeychainPool constructs a pre-fix metadata/keychain state that
// current Save correctly refuses (for example a noncanonical namespace). It
// uses the storage primitives directly so tests can prove load/restore stays
// fail-closed for data written by an older release.
func seedLegacyKeychainPool(t *testing.T, dir, name string, pool Pool) Store {
	t.Helper()
	s := keychainStore(dir)
	if err := s.ensureDirectory(); err != nil {
		t.Fatal(err)
	}
	for _, a := range pool.Accounts {
		for _, field := range []struct {
			name  string
			value string
		}{
			{keychainFieldAPIKey, a.APIKey},
			{keychainFieldAccessKey, a.AccessKey},
			{keychainFieldSecretKey, a.SecretKey},
		} {
			if field.value == "" {
				continue
			}
			if err := credstore.KeychainSet(keychainKey(name, a.ID, field.name), field.value); err != nil {
				t.Fatalf("seed legacy keychain field %s: %v", field.name, err)
			}
		}
	}
	if err := s.writeMetadataFile(name, pool); err != nil {
		t.Fatalf("seed legacy metadata: %v", err)
	}
	return s
}

func TestKeychainRoundTrip(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	s := keychainStore(dir)
	credA := Credentials{APIKey: "sk-alpha-secret"}
	credB := Credentials{APIKey: "ark-beta-secret", AccessKey: "AK-BETA", SecretKey: "SK-BETA"}
	// One providerID throughout: normalizePool re-derives ids from credentials,
	// so mixing providerIDs between Save and Load would rewrite identities.
	pool := Pool{Version: 1, Accounts: []Account{
		{ID: AccountID("volcengine", credA), Label: "a", APIKey: credA.APIKey, AddedAt: "2026-08-26"},
		{ID: AccountID("volcengine", credB), Label: "b", APIKey: credB.APIKey, AccessKey: credB.AccessKey, SecretKey: credB.SecretKey, AddedAt: "2026-08-26"},
	}}
	// Capture ids BEFORE Save: Save sorts the shared accounts slice in place.
	idA, idB := pool.Accounts[0].ID, pool.Accounts[1].ID
	if err := s.Save("prov", "volcengine", pool); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// The pool file carries metadata only — no secret material on disk.
	content := poolFileBytes(t, s, "prov")
	for _, secret := range []string{"sk-alpha-secret", "ark-beta-secret", "AK-BETA", "SK-BETA"} {
		if strings.Contains(content, secret) {
			t.Fatalf("pool file contains plaintext secret %q", secret)
		}
	}
	for _, id := range []string{idA, idB} {
		if !strings.Contains(content, id) {
			t.Fatalf("pool file lost account id %s", id)
		}
	}
	// The keychain holds every secret field under its derived key.
	if got := keychainEntry(t, "prov", idA, keychainFieldAPIKey); got != credA.APIKey {
		t.Fatalf("keychain api_key = %q", got)
	}
	if got := keychainEntry(t, "prov", idB, keychainFieldAccessKey); got != credB.AccessKey {
		t.Fatalf("keychain access_key = %q", got)
	}
	if got := keychainEntry(t, "prov", idB, keychainFieldSecretKey); got != credB.SecretKey {
		t.Fatalf("keychain secret_key = %q", got)
	}

	// Read round-trip hydrates the full credential tuples.
	loaded, err := s.Load("prov", "volcengine")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Accounts) != 2 {
		t.Fatalf("loaded %d accounts, want 2", len(loaded.Accounts))
	}
	byID := map[string]Account{}
	for _, a := range loaded.Accounts {
		byID[a.ID] = a
	}
	if got := byID[idA].APIKey; got != credA.APIKey {
		t.Fatalf("hydrated api_key = %q", got)
	}
	b := byID[idB]
	if b.APIKey != credB.APIKey || b.AccessKey != credB.AccessKey || b.SecretKey != credB.SecretKey {
		t.Fatalf("hydrated volcengine tuple = %+v", b.Credentials())
	}
}

func TestKeychainMigratesPlaintextPool(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	cred := Credentials{APIKey: "sk-legacy-plaintext"}
	pool := Pool{Version: 1, Accounts: []Account{
		{ID: AccountID("zhipu", cred), Label: "old", APIKey: cred.APIKey, AddedAt: "2026-01-01"},
	}}
	// Pre-seed a plaintext pool via the file backend (the pre-keychain layout).
	if err := NewStoreWithBackend(dir, BackendFile).Save("prov", "zhipu", pool); err != nil {
		t.Fatalf("seed file pool: %v", err)
	}

	s := keychainStore(dir)
	loaded, err := s.Load("prov", "zhipu")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Accounts) != 1 || loaded.Accounts[0].APIKey != cred.APIKey {
		t.Fatalf("loaded pool = %+v", loaded.Accounts)
	}
	// Migration rewrote the file metadata-only and copied the secret over.
	if content := poolFileBytes(t, s, "prov"); strings.Contains(content, cred.APIKey) {
		t.Fatalf("pool file still contains plaintext after migration")
	}
	if got := keychainEntry(t, "prov", loaded.Accounts[0].ID, keychainFieldAPIKey); got != cred.APIKey {
		t.Fatalf("keychain api_key = %q", got)
	}
	// A second read is stable (hydration path, no re-migration).
	if _, err := s.Load("prov", "zhipu"); err != nil {
		t.Fatalf("second Load: %v", err)
	}
}

func TestKeychainMigratesLegacySingular(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	s := keychainStore(dir)
	if err := os.MkdirAll(filepath.Join(dir, ".model-proxy"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.LegacyPath("prov"), []byte(`{"api_key":"sk-ancient"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	snapshot, err := s.LoadSnapshot("prov", "zhipu")
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if snapshot.Source != SourceLegacy {
		t.Fatalf("source = %v, want SourceLegacy", snapshot.Source)
	}
	if len(snapshot.Pool.Accounts) != 1 || snapshot.Pool.Accounts[0].APIKey != "sk-ancient" {
		t.Fatalf("snapshot pool = %+v", snapshot.Pool.Accounts)
	}
	id := snapshot.Pool.Accounts[0].ID
	if got := keychainEntry(t, "prov", id, keychainFieldAPIKey); got != "sk-ancient" {
		t.Fatalf("keychain api_key = %q", got)
	}
	// Plural metadata pool created; legacy file moved aside with one rollback
	// generation kept.
	if content := poolFileBytes(t, s, "prov"); strings.Contains(content, "sk-ancient") {
		t.Fatalf("plural pool file contains plaintext")
	}
	if _, err := os.Stat(s.LegacyPath("prov")); !os.IsNotExist(err) {
		t.Fatalf("legacy file still present: %v", err)
	}
	if _, err := os.Stat(s.LegacyPath("prov") + ".migrated.bak"); err != nil {
		t.Fatalf("legacy rollback backup missing: %v", err)
	}
	// Next read is served by the plural pool.
	second, err := s.LoadSnapshot("prov", "zhipu")
	if err != nil {
		t.Fatalf("second LoadSnapshot: %v", err)
	}
	if second.Source != SourcePlural || len(second.Pool.Accounts) != 1 {
		t.Fatalf("second snapshot = %+v", second)
	}
}

func TestKeychainDeleteOnAccountRemoval(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	s := keychainStore(dir)
	credA := Credentials{APIKey: "sk-keep"}
	credB := Credentials{APIKey: "sk-remove", AccessKey: "AK-RM", SecretKey: "SK-RM"}
	idA, idB := AccountID("volcengine", credA), AccountID("volcengine", credB)
	full := Pool{Version: 1, Accounts: []Account{
		{ID: idA, Label: "a", APIKey: credA.APIKey, AccessKey: credA.AccessKey, SecretKey: credA.SecretKey, AddedAt: "2026-08-26"},
		{ID: idB, Label: "b", APIKey: credB.APIKey, AccessKey: credB.AccessKey, SecretKey: credB.SecretKey, AddedAt: "2026-08-26"},
	}}
	if err := s.Save("prov", "volcengine", full); err != nil {
		t.Fatalf("Save full: %v", err)
	}
	// Logout shape: load → drop the account → save.
	loaded, err := s.Load("prov", "volcengine")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	kept := loaded.Accounts[:0]
	for _, a := range loaded.Accounts {
		if a.ID != idB {
			kept = append(kept, a)
		}
	}
	loaded.Accounts = kept
	if err := s.Save("prov", "volcengine", loaded); err != nil {
		t.Fatalf("Save reduced: %v", err)
	}
	// Every keychain entry of the removed account is gone; the kept one stays.
	for _, field := range []string{keychainFieldAPIKey, keychainFieldAccessKey, keychainFieldSecretKey} {
		if _, err := keyring.Get("model-proxy", keychainKey("prov", idB, field)); !errors.Is(err, keyring.ErrNotFound) {
			t.Fatalf("removed account field %s: err = %v, want ErrNotFound", field, err)
		}
	}
	if got := keychainEntry(t, "prov", idA, keychainFieldAPIKey); got != credA.APIKey {
		t.Fatalf("kept account api_key = %q", got)
	}
}

func TestKeychainUnavailableFailsClosed(t *testing.T) {
	dir := t.TempDir()
	cred := Credentials{APIKey: "sk-plaintext-stays"}
	pool := Pool{Version: 1, Accounts: []Account{
		{ID: AccountID("zhipu", cred), Label: "a", APIKey: cred.APIKey, AddedAt: "2026-08-26"},
	}}
	// Seed a plaintext pool while the mock still works, then break the backend.
	keyring.MockInit()
	if err := NewStoreWithBackend(dir, BackendFile).Save("prov", "zhipu", pool); err != nil {
		t.Fatalf("seed: %v", err)
	}
	keyring.MockInitWithError(errors.New("secret service boom"))

	s := keychainStore(dir)
	// Save fails closed: no metadata file replaces the plaintext pool.
	if err := s.Save("prov", "zhipu", pool); err == nil {
		t.Fatal("Save succeeded with unavailable keychain")
	} else if !errors.Is(err, credstore.ErrUnavailable) {
		t.Fatalf("Save err = %v, want ErrUnavailable", err)
	}
	// Load fails closed instead of silently serving the plaintext file, and
	// the file is left untouched for the next attempt.
	if _, err := s.Load("prov", "zhipu"); err == nil {
		t.Fatal("Load succeeded with unavailable keychain")
	} else if !errors.Is(err, credstore.ErrUnavailable) {
		t.Fatalf("Load err = %v, want ErrUnavailable", err)
	}
	if content := poolFileBytes(t, s, "prov"); !strings.Contains(content, cred.APIKey) {
		t.Fatalf("plaintext pool file was modified despite failed migration")
	}
}

func TestKeychainMissingEntryFailsClosed(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	s := keychainStore(dir)
	cred := Credentials{APIKey: "sk-once"}
	pool := Pool{Version: 1, Accounts: []Account{
		{ID: AccountID("zhipu", cred), Label: "a", APIKey: cred.APIKey, AddedAt: "2026-08-26"},
	}}
	if err := s.Save("prov", "zhipu", pool); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// Simulate a lost keychain entry behind a metadata-only pool file.
	if err := keyring.Delete("model-proxy", keychainKey("prov", pool.Accounts[0].ID, keychainFieldAPIKey)); err != nil {
		t.Fatalf("delete entry: %v", err)
	}
	if _, err := s.Load("prov", "zhipu"); err == nil {
		t.Fatal("Load succeeded with missing keychain entry")
	} else if !errors.Is(err, credstore.ErrNotFound) {
		t.Fatalf("Load err = %v, want ErrNotFound", err)
	}
}

func TestKeychainMetadataIdentityMismatchFailsClosed(t *testing.T) {
	t.Run("noncanonical api-key namespace", func(t *testing.T) {
		keyring.MockInit()
		dir := t.TempDir()
		s := keychainStore(dir)
		cred := Credentials{APIKey: "sk-noncanonical-keychain"}
		pool := Pool{Version: 1, Accounts: []Account{{
			ID: "legacy-id", Label: "legacy", APIKey: cred.APIKey, AddedAt: "2026-08-26",
		}}}
		if err := s.Save("prov", "zhipu", pool); err == nil || !strings.Contains(err.Error(), "credentials do not match metadata identity") {
			t.Fatalf("noncanonical Save error = %v, want identity mismatch", err)
		}
		if _, err := os.Stat(s.PoolPath("prov")); !os.IsNotExist(err) {
			t.Fatalf("rejected noncanonical Save wrote metadata: %v", err)
		}
		s = seedLegacyKeychainPool(t, dir, "prov", pool)
		if _, err := s.LoadSnapshot("prov", "zhipu"); err == nil || !strings.Contains(err.Error(), "credentials do not match metadata identity") {
			t.Fatalf("LoadSnapshot error = %v, want identity mismatch", err)
		}
	})

	t.Run("volcengine AK fields both missing", func(t *testing.T) {
		keyring.MockInit()
		dir := t.TempDir()
		s := keychainStore(dir)
		cred := Credentials{APIKey: "ark-keychain-bound", AccessKey: "AK-KEYCHAIN-BOUND", SecretKey: "SK-KEYCHAIN-BOUND"}
		id := AccountID("volcengine", cred)
		pool := Pool{Version: 1, Accounts: []Account{{
			ID: id, Label: "bound", APIKey: cred.APIKey,
			AccessKey: cred.AccessKey, SecretKey: cred.SecretKey, AddedAt: "2026-08-26",
		}}}
		if err := s.Save("vol", "volcengine", pool); err != nil {
			t.Fatalf("seed Volcengine metadata: %v", err)
		}
		for _, field := range []string{keychainFieldAccessKey, keychainFieldSecretKey} {
			if err := credstore.KeychainDelete(keychainKey("vol", id, field)); err != nil {
				t.Fatalf("delete %s fixture: %v", field, err)
			}
		}
		if _, err := s.LoadSnapshot("vol", "volcengine"); err == nil || !strings.Contains(err.Error(), "credentials do not match metadata identity") {
			t.Fatalf("LoadSnapshot error = %v, want fail-closed identity mismatch", err)
		}
		content := poolFileBytes(t, s, "vol")
		if !strings.Contains(content, id) || strings.Contains(content, cred.APIKey) {
			t.Fatalf("failed keychain load changed metadata file: %s", content)
		}
	})
}

func TestFileBackendIgnoresKeychainMode(t *testing.T) {
	// Zero-regression: the file backend never consults the keychain, even with
	// a broken backend injected.
	keyring.MockInitWithError(errors.New("boom"))
	dir := t.TempDir()
	s := NewStoreWithBackend(dir, BackendFile)
	cred := Credentials{APIKey: "sk-file-mode"}
	pool := Pool{Version: 1, Accounts: []Account{
		{ID: AccountID("zhipu", cred), Label: "a", APIKey: cred.APIKey, AddedAt: "2026-08-26"},
	}}
	if err := s.Save("prov", "zhipu", pool); err != nil {
		t.Fatalf("file Save: %v", err)
	}
	loaded, err := s.Load("prov", "zhipu")
	if err != nil {
		t.Fatalf("file Load: %v", err)
	}
	if len(loaded.Accounts) != 1 || loaded.Accounts[0].APIKey != cred.APIKey {
		t.Fatalf("file pool = %+v", loaded.Accounts)
	}
	if content := poolFileBytes(t, s, "prov"); !strings.Contains(content, cred.APIKey) {
		t.Fatalf("file backend stripped the plaintext secret")
	}
	// A pure file-mode history has no restore provenance, so explicit removal
	// must remain independent of an unavailable keychain backend.
	if err := s.RemoveAccount("prov", "zhipu", pool.Accounts[0].ID); err != nil {
		t.Fatalf("pure file RemoveAccount consulted keychain: %v", err)
	}
	after, err := s.Load("prov", "zhipu")
	if err != nil || len(after.Accounts) != 0 {
		t.Fatalf("pure file pool after RemoveAccount = (%+v, %v)", after, err)
	}
}
