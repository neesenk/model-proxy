package accounts

import (
	"encoding/json"
	"fmt"

	"model-proxy/internal/credstore"
)

// Keychain→file restore (config `credentials:` switched back to file after a
// keychain-mode save): the pool file is metadata-only, so secret VALUES are
// read back from the keychain per entry and the plaintext pool file is
// rewritten (0600, atomic). This is the reverse of loadSnapshotKeychain's
// lazy migration; the forward direction is unchanged.
//
// Contract:
//   - PARTIAL restore never fails the whole pool: an account whose keychain
//     entry is missing (or whose backend is unreachable) keeps its metadata
//     and is reported via Snapshot.ReloginNeeded — it needs a fresh `login`.
//   - Only a FULL restore rewrites the pool file as plaintext; a partial
//     restore leaves the metadata file untouched so the report (and the
//     missing accounts' metadata) survives until the user re-logins.
//   - Restored keychain entries are KEPT, not deleted: deleting on a read
//     path risks destroying the only copy when the file write is later lost,
//     and `logout` is the normal deletion path.
//   - A mixed pool (some entries with secrets, some without) is NOT a
//     restore candidate: validatePool rejects it as before — that shape means
//     a hand-edited or half-written file, not a mode switch.

// isMetadataOnly reports whether every account in p lacks secret values — the
// on-disk shape saveKeychain writes. An empty pool is just an empty pool.
func isMetadataOnly(p Pool) bool {
	if len(p.Accounts) == 0 {
		return false
	}
	for _, a := range p.Accounts {
		if a.APIKey != "" || a.AccessKey != "" || a.SecretKey != "" {
			return false
		}
	}
	return true
}

// restoreFromKeychain hydrates a metadata-only pool from the keychain under
// the file backend. See the file-level contract above.
func (s Store) restoreFromKeychain(name, providerID string, p Pool) (Snapshot, error) {
	restored := make([]Account, 0, len(p.Accounts))
	var relogin []Account
	for _, a := range p.Accounts {
		apiKey, err := credstore.KeychainGet(keychainKey(name, a.ID, keychainFieldAPIKey))
		if err != nil {
			// Missing entry OR unreachable backend: the user chose file mode,
			// so keychain trouble must not take the restorable accounts down
			// with it — report and continue.
			relogin = append(relogin, a)
			continue
		}
		a.APIKey = apiKey
		a.AccessKey = keychainGetOptional(name, a.ID, keychainFieldAccessKey)
		a.SecretKey = keychainGetOptional(name, a.ID, keychainFieldSecretKey)
		restored = append(restored, a)
	}
	if len(restored) == 0 {
		// Nothing restorable: keep the metadata file as the only record of
		// which accounts exist; every one of them needs a fresh login.
		return Snapshot{Source: SourcePlural, ReloginNeeded: relogin}, nil
	}
	pool := Pool{Version: p.Version, Accounts: restored}
	if pool.Version == 0 {
		pool.Version = 1
	}
	if err := validatePool(providerID, pool); err != nil {
		return Snapshot{Source: SourcePlural}, fmt.Errorf("validate %s: %w", s.PoolPath(name), err)
	}
	pool = normalizePool(providerID, pool)
	if len(relogin) > 0 {
		// Partial: serve the restored accounts but keep the metadata file
		// intact so the missing accounts' labels/ids survive for re-login.
		return Snapshot{Pool: pool, Source: SourcePlural, ReloginNeeded: relogin}, nil
	}
	// Full restore: rewrite the pool as a plaintext file with the same atomic
	// 0600 write as a normal save. Fail-closed on the write: the secrets are
	// still safe in the keychain and the metadata file stays authoritative, so
	// an error here loses nothing.
	data, err := json.MarshalIndent(pool, "", "  ")
	if err != nil {
		return Snapshot{Source: SourcePlural}, fmt.Errorf("marshal pool %s: %w", s.PoolPath(name), err)
	}
	if err := credstore.AtomicWriteFile(s.PoolPath(name), data, 0o600); err != nil {
		return Snapshot{Source: SourcePlural}, fmt.Errorf("rewrite pool %s plaintext: %w", s.PoolPath(name), err)
	}
	return Snapshot{Pool: pool, Source: SourcePlural}, nil
}
