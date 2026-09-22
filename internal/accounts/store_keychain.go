package accounts

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"

	"model-proxy/internal/credstore"
)

// Keychain backend (config `credentials: keychain`): api_key/access_key/
// secret_key values live in the OS keychain (via credstore, service
// "model-proxy"); the pool JSON file keeps metadata only. All keychain I/O
// funnels through credstore's entry-level API — the archtest credential-store
// contract forbids importing go-keyring outside internal/credstore.
//
// Fail-closed rule: the user explicitly selected the keychain, so ANY
// keychain failure (unavailable backend, missing entry for a metadata-only
// account) is an error — never a silent fallback to plaintext file reads
// (docs/decisions/intentional-behaviors.md).

const (
	keychainFieldAPIKey    = "api_key"
	keychainFieldAccessKey = "access_key"
	keychainFieldSecretKey = "secret_key"
)

// keychainKey names one secret field of one pooled account:
// "<providerName>/<accountID>/<field>". The account id is a credential hash
// (AccountID), so the key never carries secret material.
func keychainKey(providerName, accountID, field string) string {
	return providerName + "/" + accountID + "/" + field
}

// metadataOnly strips secret values for the on-disk pool file. The JSON
// schema stays identical to file mode (empty strings, not omitted fields), so
// a pool file is parseable by either backend and validatePool's "api_key is
// empty" error doubles as the "you are reading a keychain-mode pool with the
// file backend" signal.
func metadataOnly(p Pool) Pool {
	out := p
	out.Accounts = make([]Account, len(p.Accounts))
	for i, a := range p.Accounts {
		a.APIKey, a.AccessKey, a.SecretKey = "", "", ""
		out.Accounts[i] = a
	}
	return out
}

// writeMetadataFile persists the metadata-only pool via the same atomic
// 0600 write as file mode.
func (s Store) writeMetadataFile(name string, p Pool) error {
	data, err := json.MarshalIndent(metadataOnly(p), "", "  ")
	if err != nil {
		return fmt.Errorf("marshal pool %s: %w", name, err)
	}
	return credstore.AtomicWriteFile(s.PoolPath(name), data, 0o600)
}

// loadSnapshotKeychain is the keychain-mode LoadSnapshot: hydrate secret
// values from the keychain, migrating any plaintext found along the way.
func (s Store) loadSnapshotKeychain(name, providerID string) (Snapshot, error) {
	data, err := os.ReadFile(s.PoolPath(name))
	if err != nil {
		if os.IsNotExist(err) {
			return s.loadLegacyKeychain(name, providerID)
		}
		return Snapshot{Source: SourcePlural}, err
	}
	var p Pool
	if err := json.Unmarshal(data, &p); err != nil {
		return Snapshot{Source: SourcePlural}, fmt.Errorf("parse %s: %w", s.PoolPath(name), err)
	}
	migrated := false
	for i := range p.Accounts {
		a := &p.Accounts[i]
		if a.APIKey != "" {
			// Lazy migration: plaintext values move into the keychain under the
			// NORMALIZED id (the same derivation normalizePool applies), then
			// the file is rewritten metadata-only below. Set is unconditional:
			// pre-migration the file is authoritative, and rewriting the same
			// value is idempotent.
			id := AccountID(providerID, a.Credentials())
			if err := s.migrateSecrets(name, id, a.Credentials()); err != nil {
				return Snapshot{Source: SourcePlural}, err
			}
			migrated = true
			continue
		}
		// Metadata-only entry: hydrate from the keychain. A missing api_key
		// entry is fail-closed — there is no plaintext copy to fall back to.
		apiKey, kerr := credstore.KeychainGet(keychainKey(name, a.ID, keychainFieldAPIKey))
		if kerr != nil {
			return Snapshot{Source: SourcePlural}, fmt.Errorf("keychain read for pool %s: %w", s.PoolPath(name), kerr)
		}
		a.APIKey = apiKey
		// access_key/secret_key are optional (volcengine only): absent entry
		// means the account simply has none — but a backend error fails the
		// load: a store that answered the api_key read moments ago flipping
		// to unavailable is a mid-load inconsistency, and handing
		// validatePool an account with empty AK/SK would silently degrade
		// usage/quota later.
		accessKey, err := s.keychainGetOptional(name, a.ID, keychainFieldAccessKey)
		if err != nil {
			return Snapshot{Source: SourcePlural}, err
		}
		a.AccessKey = accessKey
		secretKey, err := s.keychainGetOptional(name, a.ID, keychainFieldSecretKey)
		if err != nil {
			return Snapshot{Source: SourcePlural}, err
		}
		a.SecretKey = secretKey
		// The metadata id is also the keychain namespace. Hydration must not
		// silently normalize it to a different identity: doing so would make the
		// runtime use one id while the retained secrets remain under another.
		// This also prevents a Volcengine AK-bound account whose optional fields
		// disappeared from degrading into an API-only account.
		if AccountID(providerID, a.Credentials()) != a.ID {
			return Snapshot{Source: SourcePlural}, fmt.Errorf("validate %s: accounts[%d] credentials do not match metadata identity", s.PoolPath(name), i)
		}
	}
	if err := validatePool(providerID, p); err != nil {
		return Snapshot{Source: SourcePlural}, fmt.Errorf("validate %s: %w", s.PoolPath(name), err)
	}
	p = normalizePool(providerID, p)
	if migrated {
		// The migration rewrite is a side effect of a READ path (LoadSnapshot
		// runs unlocked) and must not race a concurrent locked save — grab the
		// pool lock non-blockingly and skip the rewrite when contended (the
		// plaintext file stays until the next uncontended load redoes the
		// migration). Fail-closed on the uncontended rewrite itself: secrets
		// are safe in the keychain, and surfacing the error beats leaving
		// plaintext on disk silently.
		release, locked := s.tryPoolLock(name)
		if !locked {
			return Snapshot{Pool: p, Source: SourcePlural}, nil
		}
		defer release()
		if err := s.writeMetadataFile(name, p); err != nil {
			return Snapshot{Source: SourcePlural}, fmt.Errorf("rewrite pool %s metadata-only: %w", s.PoolPath(name), err)
		}
	}
	return Snapshot{Pool: p, Source: SourcePlural}, nil
}

// migrateSecrets copies one account's plaintext credential tuple into the
// keychain. Only non-empty fields are written (access/secret are optional).
func (s Store) migrateSecrets(name, id string, c Credentials) error {
	if err := credstore.KeychainSet(keychainKey(name, id, keychainFieldAPIKey), c.APIKey); err != nil {
		return fmt.Errorf("keychain migration for pool %s: %w", s.PoolPath(name), err)
	}
	if c.AccessKey != "" {
		if err := credstore.KeychainSet(keychainKey(name, id, keychainFieldAccessKey), c.AccessKey); err != nil {
			return fmt.Errorf("keychain migration for pool %s: %w", s.PoolPath(name), err)
		}
	}
	if c.SecretKey != "" {
		if err := credstore.KeychainSet(keychainKey(name, id, keychainFieldSecretKey), c.SecretKey); err != nil {
			return fmt.Errorf("keychain migration for pool %s: %w", s.PoolPath(name), err)
		}
	}
	return nil
}

// keychainGetOptional reads an optional secret field. ErrNotFound degrades
// to "" (the account has no such field); every other error propagates —
// optional FIELD, not optional backend (see the fail-closed rule above).
func (s *Store) keychainGetOptional(name, id, field string) (string, error) {
	value, err := credstore.KeychainGet(keychainKey(name, id, field))
	if err == nil {
		return value, nil
	}
	if errors.Is(err, credstore.ErrNotFound) {
		return "", nil
	}
	return "", fmt.Errorf("keychain read for pool %s: %w", s.PoolPath(name), err)
}

// loadLegacyKeychain migrates the legacy singular <name>_apikey.json: secrets
// move into the keychain, a metadata-only plural pool is created, and the
// legacy file is renamed to <path>.migrated.bak (one rollback generation,
// same convention as credstore.Ref). Any failure before the rename aborts the
// read fail-closed with the legacy file untouched.
func (s Store) loadLegacyKeychain(name, providerID string) (Snapshot, error) {
	sdata, serr := os.ReadFile(s.LegacyPath(name))
	if serr != nil {
		if os.IsNotExist(serr) {
			return Snapshot{Source: SourceMissing}, nil
		}
		return Snapshot{Source: SourceLegacy}, serr
	}
	var v struct {
		APIKey    string `json:"api_key"`
		AccessKey string `json:"access_key"`
		SecretKey string `json:"secret_key"`
	}
	if err := json.Unmarshal(sdata, &v); err != nil {
		return Snapshot{Source: SourceLegacy}, fmt.Errorf("parse %s: %w", s.LegacyPath(name), err)
	}
	cred := Credentials{APIKey: v.APIKey, AccessKey: v.AccessKey, SecretKey: v.SecretKey}
	id := AccountID(providerID, cred)
	if err := s.migrateSecrets(name, id, cred); err != nil {
		return Snapshot{Source: SourceLegacy}, err
	}
	pool := Pool{
		Version: 1,
		Accounts: []Account{{
			ID: id, Label: providerID,
			APIKey: v.APIKey, AccessKey: v.AccessKey, SecretKey: v.SecretKey,
		}},
	}
	if err := validatePool(providerID, pool); err != nil {
		return Snapshot{Source: SourceLegacy}, fmt.Errorf("validate %s: %w", s.LegacyPath(name), err)
	}
	if err := s.ensureDirectory(); err != nil {
		return Snapshot{Source: SourceLegacy}, err
	}
	if err := s.writeMetadataFile(name, pool); err != nil {
		return Snapshot{Source: SourceLegacy}, fmt.Errorf("rewrite pool %s metadata-only: %w", s.PoolPath(name), err)
	}
	// Best-effort: a stranded legacy file only means the migration reruns (and
	// finds the authoritative plural pool first) on the next read.
	bak := s.LegacyPath(name) + ".migrated.bak"
	_ = os.Remove(bak)
	_ = os.Rename(s.LegacyPath(name), bak)
	return Snapshot{Pool: pool, Source: SourceLegacy}, nil
}

// saveKeychain persists a pool in keychain mode: secret values go to the
// keychain FIRST (it must be authoritative before the file stops carrying
// plaintext), then keychain entries of accounts removed since the last save
// are deleted, and only then is the metadata-only file written. Deleting
// before the metadata write is the chosen orphan rule: a keychain entry
// without metadata is a leak, while metadata without its entry is merely an
// unreadable account the next save can fix — so a deletion failure aborts the
// save with the old metadata file intact.
func (s Store) saveKeychain(name, providerID string, p Pool) error {
	// In keychain mode the persisted id is also the secret namespace. Reject a
	// noncanonical pool before the first backend write; otherwise Save could
	// succeed but the next Load would either change identity in memory or leave
	// the secret stranded under an unowned namespace.
	for i, a := range p.Accounts {
		if AccountID(providerID, a.Credentials()) != a.ID {
			return fmt.Errorf("validate pool %s: accounts[%d] credentials do not match metadata identity", name, i)
		}
	}
	for _, a := range p.Accounts {
		if err := credstore.KeychainSet(keychainKey(name, a.ID, keychainFieldAPIKey), a.APIKey); err != nil {
			return fmt.Errorf("keychain write for pool %s: %w", s.PoolPath(name), err)
		}
		// access_key/secret_key are optional: an empty new value must CLEAR any
		// stale entry, otherwise the next hydration would resurrect a credential
		// the account no longer has.
		for _, field := range []struct {
			name  string
			value string
		}{
			{keychainFieldAccessKey, a.AccessKey},
			{keychainFieldSecretKey, a.SecretKey},
		} {
			key := keychainKey(name, a.ID, field.name)
			if field.value != "" {
				if err := credstore.KeychainSet(key, field.value); err != nil {
					return fmt.Errorf("keychain write for pool %s: %w", s.PoolPath(name), err)
				}
				continue
			}
			if err := credstore.KeychainDelete(key); err != nil && !errors.Is(err, credstore.ErrNotFound) {
				return fmt.Errorf("keychain delete for pool %s: %w", s.PoolPath(name), err)
			}
		}
	}
	if err := s.deleteRemovedEntries(name, providerID, p); err != nil {
		return err
	}
	if err := s.writeMetadataFile(name, p); err != nil {
		return fmt.Errorf("write pool %s: %w", s.PoolPath(name), err)
	}
	// Move any stale legacy plaintext aside: its secret is either in the
	// keychain (migrated on load) or intentionally removed with the account.
	if _, err := os.Stat(s.LegacyPath(name)); err == nil {
		bak := s.LegacyPath(name) + ".migrated.bak"
		_ = os.Remove(bak)
		_ = os.Rename(s.LegacyPath(name), bak)
	}
	return nil
}

// deleteRemovedEntries removes keychain entries for accounts present in the
// previous pool file but absent from p. Reads the old file RAW (no hydration)
// so a pre-migration plaintext file does not round-trip through the keychain
// just to compute deletions. An unreadable/missing old file skips cleanup —
// the new save is still authoritative, and orphans from a file we cannot parse
// are unknowable anyway.
func (s Store) deleteRemovedEntries(name, providerID string, p Pool) error {
	data, err := os.ReadFile(s.PoolPath(name))
	if err != nil {
		return nil
	}
	var old Pool
	if err := json.Unmarshal(data, &old); err != nil {
		return nil
	}
	kept := make(map[string]struct{}, len(p.Accounts))
	for _, a := range p.Accounts {
		kept[a.ID] = struct{}{}
	}
	for _, a := range old.Accounts {
		id := a.ID
		if a.APIKey != "" {
			// Pre-migration plaintext entry: its keychain key uses the
			// normalized id, same as loadSnapshotKeychain's migration.
			id = AccountID(providerID, a.Credentials())
		}
		if _, ok := kept[id]; ok {
			continue
		}
		if err := s.deleteKeychainAccount(name, id); err != nil {
			return err
		}
	}
	return nil
}

func (s Store) deleteKeychainAccounts(name string, ids map[string]struct{}) error {
	ordered := make([]string, 0, len(ids))
	for id := range ids {
		ordered = append(ordered, id)
	}
	sort.Strings(ordered)
	for _, id := range ordered {
		if err := s.deleteKeychainAccount(name, id); err != nil {
			return err
		}
	}
	return nil
}

func (s Store) deleteKeychainAccount(name, id string) error {
	for _, field := range []string{keychainFieldAPIKey, keychainFieldAccessKey, keychainFieldSecretKey} {
		if err := credstore.KeychainDelete(keychainKey(name, id, field)); err != nil && !errors.Is(err, credstore.ErrNotFound) {
			return fmt.Errorf("keychain delete for pool %s: %w", s.PoolPath(name), err)
		}
	}
	return nil
}
