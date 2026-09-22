package accounts

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	if err := s.RemoveAccount("prov", "volcengine", idB); err != nil {
		t.Fatalf("file-mode RemoveAccount after restore: %v", err)
	}
	after, err := s.Load("prov", "volcengine")
	if err != nil || len(after.Accounts) != 1 || after.Accounts[0].ID != idA {
		t.Fatalf("file-mode delete-after-restore: (%+v, %v)", after, err)
	}
	for _, field := range []string{keychainFieldAPIKey, keychainFieldAccessKey, keychainFieldSecretKey} {
		if _, err := keyring.Get("model-proxy", keychainKey("prov", idB, field)); !errors.Is(err, keyring.ErrNotFound) {
			t.Fatalf("removed restored account field %s: err = %v, want ErrNotFound", field, err)
		}
	}
	if got := keychainEntry(t, "prov", idA, keychainFieldAPIKey); got != credA.APIKey {
		t.Fatalf("kept restored account api_key = %q", got)
	}
	markerData, err := os.ReadFile(s.restoredKeychainMarkerPath("prov"))
	if err != nil {
		t.Fatalf("read restore marker after one removal: %v", err)
	}
	if !strings.Contains(string(markerData), idA) || strings.Contains(string(markerData), idB) {
		t.Fatalf("restore marker = %s, want only kept account id", markerData)
	}
	for _, secret := range []string{credA.APIKey, credB.APIKey, credB.AccessKey, credB.SecretKey} {
		if strings.Contains(string(markerData), secret) {
			t.Fatalf("restore marker contains credential value %q", secret)
		}
	}
	markerInfo, err := os.Stat(s.restoredKeychainMarkerPath("prov"))
	if err != nil {
		t.Fatalf("stat restore marker: %v", err)
	}
	if perm := markerInfo.Mode().Perm(); perm != 0o600 {
		t.Fatalf("restore marker perms = %#o, want 0600", perm)
	}
}

func TestRestoreFromKeychainVolcengineIncompleteAKSKNeedsRelogin(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	apiOnly := Credentials{APIKey: "ark-api-only"}
	missingAccess := Credentials{APIKey: "ark-missing-access", AccessKey: "AK-MISSING", SecretKey: "SK-PRESENT"}
	missingSecret := Credentials{APIKey: "ark-missing-secret", AccessKey: "AK-PRESENT", SecretKey: "SK-MISSING"}
	pool := Pool{Version: 1, Accounts: []Account{
		{ID: AccountID("volcengine", apiOnly), Label: "api-only", APIKey: apiOnly.APIKey, AddedAt: "2026-08-26"},
		{ID: AccountID("volcengine", missingAccess), Label: "missing-access", APIKey: missingAccess.APIKey, AccessKey: missingAccess.AccessKey, SecretKey: missingAccess.SecretKey, AddedAt: "2026-08-26"},
		{ID: AccountID("volcengine", missingSecret), Label: "missing-secret", APIKey: missingSecret.APIKey, AccessKey: missingSecret.AccessKey, SecretKey: missingSecret.SecretKey, AddedAt: "2026-08-26"},
	}}
	apiOnlyID := pool.Accounts[0].ID
	missingAccessID := pool.Accounts[1].ID
	missingSecretID := pool.Accounts[2].ID
	seedKeychainPool(t, dir, "vol", "volcengine", pool)
	if err := credstore.KeychainDelete(keychainKey("vol", missingAccessID, keychainFieldAccessKey)); err != nil {
		t.Fatalf("delete access_key fixture: %v", err)
	}
	if err := credstore.KeychainDelete(keychainKey("vol", missingSecretID, keychainFieldSecretKey)); err != nil {
		t.Fatalf("delete secret_key fixture: %v", err)
	}

	s := NewStoreWithBackend(dir, BackendFile)
	snapshot, err := s.LoadSnapshot("vol", "volcengine")
	if err != nil {
		t.Fatalf("partial Volcengine restore: %v", err)
	}
	if len(snapshot.Pool.Accounts) != 1 || snapshot.Pool.Accounts[0].ID != apiOnlyID {
		t.Fatalf("restored accounts = %+v, want only API-only account", snapshot.Pool.Accounts)
	}
	if got := snapshot.Pool.Accounts[0].Credentials(); got != apiOnly {
		t.Fatalf("API-only credentials = %+v, want API key without AK/SK", got)
	}
	wantRelogin := map[string]string{missingAccessID: "missing-access", missingSecretID: "missing-secret"}
	if len(snapshot.ReloginNeeded) != len(wantRelogin) {
		t.Fatalf("ReloginNeeded = %+v, want two incomplete AK/SK accounts", snapshot.ReloginNeeded)
	}
	for _, a := range snapshot.ReloginNeeded {
		if wantRelogin[a.ID] != a.Label {
			t.Fatalf("unexpected ReloginNeeded metadata: %+v", a)
		}
		if a.Credentials() != (Credentials{}) {
			t.Fatalf("ReloginNeeded leaked partial credentials: %+v", a.Credentials())
		}
	}
	content := poolFileBytes(t, s, "vol")
	for _, id := range []string{apiOnlyID, missingAccessID, missingSecretID} {
		if !strings.Contains(content, id) {
			t.Fatalf("partial restore rewrote away metadata id %s", id)
		}
	}
	for _, secret := range []string{apiOnly.APIKey, missingAccess.APIKey, missingSecret.APIKey} {
		if strings.Contains(content, secret) {
			t.Fatalf("partial restore wrote plaintext secret %q", secret)
		}
	}
	if _, err := os.Stat(s.restoredKeychainMarkerPath("vol")); !os.IsNotExist(err) {
		t.Fatalf("partial restore created full-restore provenance: %v", err)
	}

	// Explicit removal from a partial restore operates on the authoritative
	// metadata file, so the two relogin accounts are retained and no plaintext
	// credentials are introduced as a side effect.
	if err := s.RemoveAccount("vol", "volcengine", apiOnlyID); err != nil {
		t.Fatalf("RemoveAccount from partial metadata pool: %v", err)
	}
	var metadata Pool
	if err := json.Unmarshal([]byte(poolFileBytes(t, s, "vol")), &metadata); err != nil {
		t.Fatalf("parse metadata after RemoveAccount: %v", err)
	}
	if len(metadata.Accounts) != 2 {
		t.Fatalf("metadata accounts after removal = %+v, want two relogin accounts", metadata.Accounts)
	}
	for _, a := range metadata.Accounts {
		if _, ok := wantRelogin[a.ID]; !ok || a.Credentials() != (Credentials{}) {
			t.Fatalf("metadata after partial removal = %+v", metadata.Accounts)
		}
	}
	if _, err := keyring.Get("model-proxy", keychainKey("vol", apiOnlyID, keychainFieldAPIKey)); !errors.Is(err, keyring.ErrNotFound) {
		t.Fatalf("removed API-only keychain entry: err = %v, want ErrNotFound", err)
	}
}

func TestRestoreFromKeychainNonCanonicalIDNeedsRelogin(t *testing.T) {
	cases := []struct {
		name       string
		providerID string
		storedID   string
		cred       Credentials
	}{
		{name: "api key provider", providerID: "zhipu", storedID: "legacy-zhipu-id", cred: Credentials{APIKey: "sk-legacy-zhipu"}},
		{name: "volcengine raw access key id", providerID: "volcengine", storedID: "AK-LEGACY-RAW-ID", cred: Credentials{APIKey: "ark-legacy", AccessKey: "AK-LEGACY-RAW-ID", SecretKey: "SK-LEGACY"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			keyring.MockInit()
			dir := t.TempDir()
			pool := Pool{Version: 1, Accounts: []Account{{
				ID: tc.storedID, Label: "legacy", APIKey: tc.cred.APIKey,
				AccessKey: tc.cred.AccessKey, SecretKey: tc.cred.SecretKey, AddedAt: "2026-08-26",
			}}}
			seedLegacyKeychainPool(t, dir, "prov", pool)
			s := NewStoreWithBackend(dir, BackendFile)
			snapshot, err := s.LoadSnapshot("prov", tc.providerID)
			if err != nil {
				t.Fatalf("LoadSnapshot: %v", err)
			}
			if len(snapshot.Pool.Accounts) != 0 || len(snapshot.ReloginNeeded) != 1 {
				t.Fatalf("snapshot = %+v, want one ReloginNeeded account", snapshot)
			}
			if got := snapshot.ReloginNeeded[0]; got.ID != tc.storedID || got.Credentials() != (Credentials{}) {
				t.Fatalf("ReloginNeeded = %+v, want secretless original metadata", got)
			}
			content := poolFileBytes(t, s, "prov")
			if !strings.Contains(content, tc.storedID) || strings.Contains(content, tc.cred.APIKey) {
				t.Fatalf("non-canonical partial restore changed metadata pool: %s", content)
			}
			if _, err := os.Stat(s.restoredKeychainMarkerPath("prov")); !os.IsNotExist(err) {
				t.Fatalf("non-canonical restore wrote cleanup marker: %v", err)
			}
			if got := keychainEntry(t, "prov", tc.storedID, keychainFieldAPIKey); got != tc.cred.APIKey {
				t.Fatalf("original keychain namespace changed api_key = %q", got)
			}
			canonicalID := AccountID(tc.providerID, tc.cred)
			if canonicalID != tc.storedID {
				if _, err := keyring.Get("model-proxy", keychainKey("prov", canonicalID, keychainFieldAPIKey)); !errors.Is(err, keyring.ErrNotFound) {
					t.Fatalf("canonical keychain namespace unexpectedly created: %v", err)
				}
			}
			candidate := snapshot.ReloginNeeded[0]
			candidate.APIKey = tc.cred.APIKey
			candidate.AccessKey = tc.cred.AccessKey
			candidate.SecretKey = tc.cred.SecretKey
			if err := s.Save("prov", tc.providerID, Pool{Version: 1, Accounts: []Account{candidate}}); err == nil || !strings.Contains(err.Error(), "metadata accounts remain unresolved") {
				t.Fatalf("non-canonical metadata Save error = %v, want unresolved rejection", err)
			}
			if after := poolFileBytes(t, s, "prov"); after != content {
				t.Fatal("rejected non-canonical Save changed metadata pool")
			}
			if _, err := os.Stat(s.restoredKeychainMarkerPath("prov")); !os.IsNotExist(err) {
				t.Fatalf("rejected non-canonical Save wrote cleanup marker: %v", err)
			}
		})
	}
}

func TestRestoreFromKeychainVolcengineAPIOnlyIDWithAKSKFailsClosed(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	cred := Credentials{APIKey: "ark-noncanonical-api-id", AccessKey: "AK-BOUND", SecretKey: "SK-BOUND"}
	storedID := AccountID("volcengine", Credentials{APIKey: cred.APIKey})
	if storedID == AccountID("volcengine", cred) {
		t.Fatal("test fixture unexpectedly produced colliding account ids")
	}
	pool := Pool{Version: 1, Accounts: []Account{{
		ID: storedID, Label: "misbound", APIKey: cred.APIKey,
		AccessKey: cred.AccessKey, SecretKey: cred.SecretKey, AddedAt: "2026-08-26",
	}}}
	seedLegacyKeychainPool(t, dir, "vol", pool)
	s := NewStoreWithBackend(dir, BackendFile)

	snapshot, err := s.LoadSnapshot("vol", "volcengine")
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if len(snapshot.Pool.Accounts) != 0 || len(snapshot.ReloginNeeded) != 1 {
		t.Fatalf("snapshot = %+v, want one ReloginNeeded account", snapshot)
	}
	if got := snapshot.ReloginNeeded[0]; got.ID != storedID || got.Credentials() != (Credentials{}) {
		t.Fatalf("ReloginNeeded = %+v, want original secretless metadata", got)
	}
	content := poolFileBytes(t, s, "vol")
	if !strings.Contains(content, storedID) || strings.Contains(content, cred.APIKey) {
		t.Fatalf("failed-closed restore changed metadata: %s", content)
	}
	if _, err := os.Stat(s.restoredKeychainMarkerPath("vol")); !os.IsNotExist(err) {
		t.Fatalf("failed-closed restore wrote provenance marker: %v", err)
	}
}

func TestRemoveAllAccountsCleansFullRestoreKeychainEntries(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	credA := Credentials{APIKey: "sk-remove-all-a"}
	credB := Credentials{APIKey: "sk-remove-all-b"}
	idA, idB := AccountID("zhipu", credA), AccountID("zhipu", credB)
	pool := Pool{Version: 1, Accounts: []Account{
		{ID: idA, Label: "a", APIKey: credA.APIKey, AddedAt: "2026-08-26"},
		{ID: idB, Label: "b", APIKey: credB.APIKey, AddedAt: "2026-08-26"},
	}}
	seedKeychainPool(t, dir, "prov", "zhipu", pool)
	s := NewStoreWithBackend(dir, BackendFile)
	if _, err := s.LoadSnapshot("prov", "zhipu"); err != nil {
		t.Fatalf("full restore: %v", err)
	}
	if err := s.RemoveAllAccounts("prov", "zhipu"); err != nil {
		t.Fatalf("RemoveAllAccounts: %v", err)
	}
	for _, id := range []string{idA, idB} {
		if _, err := keyring.Get("model-proxy", keychainKey("prov", id, keychainFieldAPIKey)); !errors.Is(err, keyring.ErrNotFound) {
			t.Fatalf("removed account %s keychain entry: err = %v, want ErrNotFound", id, err)
		}
	}
	snapshot, err := s.LoadSnapshot("prov", "zhipu")
	if err != nil {
		t.Fatalf("load empty tombstone: %v", err)
	}
	if snapshot.Source != SourcePlural || len(snapshot.Pool.Accounts) != 0 {
		t.Fatalf("empty tombstone snapshot = %+v", snapshot)
	}
	if _, err := os.Stat(s.restoredKeychainMarkerPath("prov")); !os.IsNotExist(err) {
		t.Fatalf("restore marker after RemoveAllAccounts: %v", err)
	}
}

func TestKeychainRemovalIgnoresStaleRestoreMarker(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	credA := Credentials{APIKey: "sk-restored-before-switch-back"}
	credB := Credentials{APIKey: "sk-added-after-switch-back"}
	idA, idB := AccountID("zhipu", credA), AccountID("zhipu", credB)
	poolA := Pool{Version: 1, Accounts: []Account{{
		ID: idA, Label: "restored", APIKey: credA.APIKey, AddedAt: "2026-08-26",
	}}}
	keychainStore := NewStoreWithBackend(dir, BackendKeychain)
	if err := keychainStore.Save("prov", "zhipu", poolA); err != nil {
		t.Fatalf("seed keychain pool: %v", err)
	}
	fileStore := NewStoreWithBackend(dir, BackendFile)
	if _, err := fileStore.LoadSnapshot("prov", "zhipu"); err != nil {
		t.Fatalf("restore keychain pool to file: %v", err)
	}
	markerBefore, err := os.ReadFile(fileStore.restoredKeychainMarkerPath("prov"))
	if err != nil {
		t.Fatalf("read restore marker: %v", err)
	}

	// Switch back to keychain and add B. The old file-restore marker still only
	// names A, while the authoritative metadata now contains both A and B.
	poolAB := Pool{Version: 1, Accounts: []Account{
		{ID: idA, Label: "restored", APIKey: credA.APIKey, AddedAt: "2026-08-26"},
		{ID: idB, Label: "new", APIKey: credB.APIKey, AddedAt: "2026-08-27"},
	}}
	if err := keychainStore.Save("prov", "zhipu", poolAB); err != nil {
		t.Fatalf("save after switching back to keychain: %v", err)
	}
	markerAfterSave, err := os.ReadFile(fileStore.restoredKeychainMarkerPath("prov"))
	if err != nil || string(markerAfterSave) != string(markerBefore) {
		t.Fatalf("stale marker precondition = (%q, %v), want original marker", markerAfterSave, err)
	}

	metadataBefore := poolFileBytes(t, keychainStore, "prov")
	keyring.MockInitWithError(errors.New("secret service down"))
	if err := keychainStore.RemoveAccount("prov", "zhipu", idB); err == nil || !errors.Is(err, credstore.ErrUnavailable) {
		t.Fatalf("RemoveAccount failure = %v, want retryable ErrUnavailable", err)
	}
	if after := poolFileBytes(t, keychainStore, "prov"); after != metadataBefore {
		t.Fatal("failed stale-marker removal changed metadata")
	}
	markerAfterFailure, err := os.ReadFile(fileStore.restoredKeychainMarkerPath("prov"))
	if err != nil || string(markerAfterFailure) != string(markerBefore) {
		t.Fatalf("failed stale-marker removal changed marker = (%q, %v)", markerAfterFailure, err)
	}

	// Restore the mock backend and its entries, then retry the exact deletion.
	keyring.MockInit()
	for _, account := range poolAB.Accounts {
		if err := credstore.KeychainSet(keychainKey("prov", account.ID, keychainFieldAPIKey), account.APIKey); err != nil {
			t.Fatalf("restore keychain fixture: %v", err)
		}
	}
	if err := keychainStore.RemoveAccount("prov", "zhipu", idB); err != nil {
		t.Fatalf("retry RemoveAccount: %v", err)
	}
	if _, err := keyring.Get("model-proxy", keychainKey("prov", idB, keychainFieldAPIKey)); !errors.Is(err, keyring.ErrNotFound) {
		t.Fatalf("new account secret survived stale marker removal: %v", err)
	}
	if got := keychainEntry(t, "prov", idA, keychainFieldAPIKey); got != credA.APIKey {
		t.Fatalf("kept account key = %q, want retained credential", got)
	}
	loaded, err := keychainStore.Load("prov", "zhipu")
	if err != nil || len(loaded.Accounts) != 1 || loaded.Accounts[0].ID != idA {
		t.Fatalf("pool after stale-marker removal = (%+v, %v)", loaded, err)
	}
}

func TestRemoveAccountAfterFullRestoreKeychainFailureKeepsFileAndMarker(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	cred := Credentials{APIKey: "sk-retryable-remove"}
	id := AccountID("zhipu", cred)
	pool := Pool{Version: 1, Accounts: []Account{{ID: id, Label: "retry", APIKey: cred.APIKey, AddedAt: "2026-08-26"}}}
	seedKeychainPool(t, dir, "prov", "zhipu", pool)
	s := NewStoreWithBackend(dir, BackendFile)
	if _, err := s.LoadSnapshot("prov", "zhipu"); err != nil {
		t.Fatalf("full restore: %v", err)
	}
	beforePool := poolFileBytes(t, s, "prov")
	beforeMarker, err := os.ReadFile(s.restoredKeychainMarkerPath("prov"))
	if err != nil {
		t.Fatalf("read restore marker: %v", err)
	}

	keyring.MockInitWithError(errors.New("secret service down"))
	t.Cleanup(keyring.MockInit)
	if err := s.RemoveAccount("prov", "zhipu", id); err == nil || !errors.Is(err, credstore.ErrUnavailable) {
		t.Fatalf("RemoveAccount error = %v, want ErrUnavailable", err)
	}
	if after := poolFileBytes(t, s, "prov"); after != beforePool {
		t.Fatal("failed keychain cleanup changed plaintext pool")
	}
	afterMarker, err := os.ReadFile(s.restoredKeychainMarkerPath("prov"))
	if err != nil {
		t.Fatalf("read marker after failed cleanup: %v", err)
	}
	if string(afterMarker) != string(beforeMarker) {
		t.Fatal("failed keychain cleanup changed restore provenance")
	}
}

func TestRemoveAccountAlreadyAbsentFromPlaintextKeepsKeychainProvenance(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	cred := Credentials{APIKey: "sk-already-absent"}
	id := AccountID("zhipu", cred)
	pool := Pool{Version: 1, Accounts: []Account{{ID: id, Label: "old", APIKey: cred.APIKey, AddedAt: "2026-08-26"}}}
	seedKeychainPool(t, dir, "prov", "zhipu", pool)
	s := NewStoreWithBackend(dir, BackendFile)
	if _, err := s.LoadSnapshot("prov", "zhipu"); err != nil {
		t.Fatalf("full restore: %v", err)
	}
	// Simulate an older/direct file mutation that removed the pool row without
	// consuming restore provenance. RemoveAccount must use the actual mutation
	// set, not the requested id, so an already-absent id remains idempotent.
	if err := s.Save("prov", "zhipu", Pool{Version: 1}); err != nil {
		t.Fatalf("seed already-absent plaintext row: %v", err)
	}
	markerBefore, err := os.ReadFile(s.restoredKeychainMarkerPath("prov"))
	if err != nil {
		t.Fatalf("read restore marker: %v", err)
	}
	if err := s.RemoveAccount("prov", "zhipu", id); err != nil {
		t.Fatalf("RemoveAccount already absent id: %v", err)
	}
	if got := keychainEntry(t, "prov", id, keychainFieldAPIKey); got != cred.APIKey {
		t.Fatalf("already-absent removal changed keychain api_key = %q", got)
	}
	markerAfter, err := os.ReadFile(s.restoredKeychainMarkerPath("prov"))
	if err != nil {
		t.Fatalf("read restore marker after no-op: %v", err)
	}
	if string(markerAfter) != string(markerBefore) {
		t.Fatal("already-absent removal changed restore provenance")
	}
}

func TestRemoveAccountUnknownRestoreMarkerVersionFailsClosed(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	cred := Credentials{APIKey: "sk-unknown-marker-version"}
	id := AccountID("zhipu", cred)
	pool := Pool{Version: 1, Accounts: []Account{{ID: id, Label: "versioned", APIKey: cred.APIKey, AddedAt: "2026-08-26"}}}
	seedKeychainPool(t, dir, "prov", "zhipu", pool)
	s := NewStoreWithBackend(dir, BackendFile)
	if _, err := s.LoadSnapshot("prov", "zhipu"); err != nil {
		t.Fatalf("full restore: %v", err)
	}
	poolBefore := poolFileBytes(t, s, "prov")
	badMarker := fmt.Sprintf(`{"version":2,"account_ids":[%q]}`, id)
	if err := os.WriteFile(s.restoredKeychainMarkerPath("prov"), []byte(badMarker), 0o600); err != nil {
		t.Fatalf("write unsupported marker: %v", err)
	}
	if err := s.RemoveAccount("prov", "zhipu", id); err == nil || !strings.Contains(err.Error(), "unsupported version 2") {
		t.Fatalf("RemoveAccount error = %v, want unsupported marker version", err)
	}
	if got := keychainEntry(t, "prov", id, keychainFieldAPIKey); got != cred.APIKey {
		t.Fatalf("unknown marker version changed keychain api_key = %q", got)
	}
	if after := poolFileBytes(t, s, "prov"); after != poolBefore {
		t.Fatal("unknown marker version changed plaintext pool")
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
	rel.APIKey = credB.APIKey
	rel.ID = AccountID("zhipu", rel.Credentials())
	newPool := Pool{Version: 1, Accounts: append(snapshot.Pool.Accounts, rel)}
	if err := s.Save("prov", "zhipu", newPool); err != nil {
		t.Fatalf("file-mode Save after partial restore: %v", err)
	}
	after, err := s.Load("prov", "zhipu")
	if err != nil || len(after.Accounts) != 2 {
		t.Fatalf("Load after re-login: (%+v, %v)", after, err)
	}
	markerData, err := os.ReadFile(s.restoredKeychainMarkerPath("prov"))
	if err != nil {
		t.Fatalf("read completion marker: %v", err)
	}
	if !strings.Contains(string(markerData), idA) || !strings.Contains(string(markerData), idB) {
		t.Fatalf("completion marker = %s, want both canonical metadata ids", markerData)
	}
	for _, secret := range []string{credA.APIKey, credB.APIKey} {
		if strings.Contains(string(markerData), secret) {
			t.Fatalf("completion marker contains credential value %q", secret)
		}
	}
}

func TestPartialRestoreSaveRejectsDroppingOtherReloginMetadata(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	credA := Credentials{APIKey: "sk-multi-restored"}
	credB := Credentials{APIKey: "sk-multi-relogin-b"}
	credC := Credentials{APIKey: "sk-multi-relogin-c"}
	idA, idB, idC := AccountID("zhipu", credA), AccountID("zhipu", credB), AccountID("zhipu", credC)
	pool := Pool{Version: 1, Accounts: []Account{
		{ID: idA, Label: "restored", APIKey: credA.APIKey, AddedAt: "2026-08-26"},
		{ID: idB, Label: "missing-b", APIKey: credB.APIKey, AddedAt: "2026-08-26"},
		{ID: idC, Label: "missing-c", APIKey: credC.APIKey, AddedAt: "2026-08-26"},
	}}
	seedKeychainPool(t, dir, "prov", "zhipu", pool)
	for _, id := range []string{idB, idC} {
		if err := credstore.KeychainDelete(keychainKey("prov", id, keychainFieldAPIKey)); err != nil {
			t.Fatalf("delete api_key fixture: %v", err)
		}
	}
	s := NewStoreWithBackend(dir, BackendFile)
	snapshot, err := s.LoadSnapshot("prov", "zhipu")
	if err != nil {
		t.Fatalf("partial restore: %v", err)
	}
	if len(snapshot.Pool.Accounts) != 1 || len(snapshot.ReloginNeeded) != 2 {
		t.Fatalf("snapshot = %+v, want one restored and two ReloginNeeded", snapshot)
	}
	before := poolFileBytes(t, s, "prov")
	resolvedB := snapshot.ReloginNeeded[0]
	if resolvedB.ID != idB {
		resolvedB = snapshot.ReloginNeeded[1]
	}
	resolvedB.APIKey = credB.APIKey
	next := Pool{Version: 1, Accounts: append(snapshot.Pool.Accounts, resolvedB)}
	if err := s.Save("prov", "zhipu", next); err == nil || !strings.Contains(err.Error(), "metadata accounts remain unresolved") {
		t.Fatalf("partial Save error = %v, want unresolved metadata rejection", err)
	}
	if after := poolFileBytes(t, s, "prov"); after != before {
		t.Fatal("rejected partial Save changed authoritative metadata")
	}
	if strings.Contains(before, credA.APIKey) || strings.Contains(before, credB.APIKey) || strings.Contains(before, credC.APIKey) {
		t.Fatal("rejected partial Save wrote plaintext credentials")
	}
	if _, err := os.Stat(s.restoredKeychainMarkerPath("prov")); !os.IsNotExist(err) {
		t.Fatalf("rejected partial Save wrote provenance marker: %v", err)
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
	if got := NewStore(t.TempDir()).backend; got != BackendKeychain {
		t.Fatalf("NewStore backend after SetProcessCredentialsMode(keychain) = %v, want keychain", got)
	}
	SetProcessCredentialsMode("file")
	if got := NewStore(t.TempDir()).backend; got != BackendFile {
		t.Fatalf("NewStore backend after SetProcessCredentialsMode(file) = %v, want file", got)
	}
}

// A concurrent locked save (login) holding the pool lock must never race the
// read-path restore rewrite: while the lock is contended, LoadSnapshot serves
// the restored snapshot WITHOUT rewriting the plaintext pool file, and a
// later uncontended read performs the rewrite.
func TestRestoreFromKeychainSkipsRewriteWhilePoolLocked(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	credA := Credentials{APIKey: "sk-restore-contended"}
	pool := Pool{Version: 1, Accounts: []Account{
		{ID: AccountID("volcengine", credA), Label: "alpha", APIKey: credA.APIKey, AddedAt: "2026-08-26"},
	}}
	seedKeychainPool(t, dir, "prov", "volcengine", pool)

	s := NewStoreWithBackend(dir, BackendFile)
	holderEntered := make(chan struct{})
	releaseHolder := make(chan struct{})
	holderDone := make(chan error, 1)
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(releaseHolder) }) })
	go func() {
		holderDone <- s.WithLock("prov", func() error {
			close(holderEntered)
			<-releaseHolder
			return nil
		})
	}()
	<-holderEntered

	snapshot, err := s.LoadSnapshot("prov", "volcengine")
	if err != nil {
		t.Fatalf("LoadSnapshot under contention: %v", err)
	}
	if len(snapshot.Pool.Accounts) != 1 || snapshot.Pool.Accounts[0].APIKey != credA.APIKey {
		t.Fatalf("contended snapshot = %+v, want the restored account", snapshot.Pool.Accounts)
	}
	// The rewrite was skipped: the pool file is still the secretless
	// metadata shape, so a concurrent locked save cannot be clobbered.
	if content := poolFileBytes(t, s, "prov"); strings.Contains(content, credA.APIKey) {
		t.Fatal("restore rewrote the plaintext pool file while the pool lock was held")
	}

	once.Do(func() { close(releaseHolder) })
	if err := <-holderDone; err != nil {
		t.Fatalf("holder WithLock: %v", err)
	}
	// Uncontended retry performs the rewrite.
	if _, err := s.LoadSnapshot("prov", "volcengine"); err != nil {
		t.Fatalf("LoadSnapshot after release: %v", err)
	}
	if content := poolFileBytes(t, s, "prov"); !strings.Contains(content, credA.APIKey) {
		t.Fatal("uncontended retry did not rewrite the plaintext pool file")
	}
}
