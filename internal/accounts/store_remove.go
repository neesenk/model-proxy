package accounts

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"model-proxy/internal/credstore"
)

// restoredKeychainMarker is the secretless provenance left by a successful
// keychain-to-file restore. File-mode removal consults it before touching the
// keychain, so a pool that has always been file-backed never performs keychain
// I/O. The marker is persisted because login/logout commands construct fresh
// Store values and may run in different processes.
type restoredKeychainMarker struct {
	Version    int      `json:"version"`
	AccountIDs []string `json:"account_ids"`
}

func (s Store) restoredKeychainMarkerPath(name string) string {
	return s.PoolPath(name) + ".keychain-origin"
}

// writeRestoredKeychainMarker records only account ids; it never serializes
// credential values. It must succeed before the metadata pool is rewritten as
// plaintext, otherwise a later file-mode logout could not prove which
// keychain entries it owns.
func (s Store) writeRestoredKeychainMarker(name string, p Pool) error {
	ids := make([]string, 0, len(p.Accounts))
	for _, a := range p.Accounts {
		ids = append(ids, a.ID)
	}
	ids, err := validateRestoredKeychainIDs(ids)
	if err != nil {
		return fmt.Errorf("validate keychain restore marker for pool %s: %w", name, err)
	}
	data, err := json.MarshalIndent(restoredKeychainMarker{Version: 1, AccountIDs: ids}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal keychain restore marker for pool %s: %w", name, err)
	}
	if err := credstore.AtomicWriteFile(s.restoredKeychainMarkerPath(name), data, 0o600); err != nil {
		return fmt.Errorf("write keychain restore marker for pool %s: %w", name, err)
	}
	return nil
}

func (s Store) loadRestoredKeychainMarker(name string) (restoredKeychainMarker, bool, error) {
	data, err := os.ReadFile(s.restoredKeychainMarkerPath(name))
	if err != nil {
		if os.IsNotExist(err) {
			return restoredKeychainMarker{}, false, nil
		}
		return restoredKeychainMarker{}, false, fmt.Errorf("read keychain restore marker for pool %s: %w", name, err)
	}
	var marker restoredKeychainMarker
	if err := json.Unmarshal(data, &marker); err != nil {
		return restoredKeychainMarker{}, false, fmt.Errorf("parse keychain restore marker for pool %s: %w", name, err)
	}
	if marker.Version != 1 {
		return restoredKeychainMarker{}, false, fmt.Errorf("validate keychain restore marker for pool %s: unsupported version %d", name, marker.Version)
	}
	ids, err := validateRestoredKeychainIDs(marker.AccountIDs)
	if err != nil {
		return restoredKeychainMarker{}, false, fmt.Errorf("validate keychain restore marker for pool %s: %w", name, err)
	}
	marker.AccountIDs = ids
	return marker, true, nil
}

func validateRestoredKeychainIDs(ids []string) ([]string, error) {
	out := append([]string(nil), ids...)
	sort.Strings(out)
	for i, id := range out {
		if strings.TrimSpace(id) == "" {
			return nil, fmt.Errorf("account_ids[%d] is empty", i)
		}
		if strings.Contains(id, "#") {
			return nil, fmt.Errorf("account_ids[%d] contains reserved '#'", i)
		}
		if i > 0 && id == out[i-1] {
			return nil, fmt.Errorf("account_ids[%d] duplicates an earlier account", i)
		}
	}
	return out, nil
}

// RemoveAccount removes one pooled account under the Store-owned cross-process lock.
// In file mode it cleans a retained keychain copy only when restore provenance
// proves that an actually removed pool row came from keychain. An id absent
// from the current pool remains a no-op; RemoveAllAccounts consumes stale
// provenance when an older writer already removed a row.
func (s Store) RemoveAccount(name, providerID, id string) error {
	selected := map[string]struct{}{}
	if id != "" {
		selected[id] = struct{}{}
	}
	return s.removeAccounts(name, providerID, selected, false)
}

// RemoveAllAccounts clears the authoritative plural pool while preserving the
// empty-file tombstone that blocks legacy fallback. Every account recorded in
// restore provenance is cleaned from keychain before the plaintext pool is
// changed.
func (s Store) RemoveAllAccounts(name, providerID string) error {
	return s.removeAccounts(name, providerID, nil, true)
}

func (s Store) removeAccounts(name, providerID string, selected map[string]struct{}, removeAll bool) error {
	return s.WithLock(name, func() error {
		pool, metadataOnlyPool, err := s.loadPoolForRemoval(name, providerID)
		if err != nil {
			return err
		}

		removed := make(map[string]struct{})
		kept := make([]Account, 0, len(pool.Accounts))
		for _, a := range pool.Accounts {
			_, chosen := selected[a.ID]
			if removeAll || chosen {
				removed[a.ID] = struct{}{}
				continue
			}
			kept = append(kept, a)
		}
		pool.Accounts = kept

		if metadataOnlyPool {
			// The authoritative file itself proves these ids came from keychain.
			// A restore marker may be stale after switching file→keychain again;
			// it augments cleanup provenance but must never suppress deletion of an
			// id that is present in the current authoritative metadata. Validate
			// the marker first, then delete current rows unconditionally. On any
			// failure the metadata and marker remain unchanged and retryable.
			_, markerExists, err := s.loadRestoredKeychainMarker(name)
			if err != nil {
				return err
			}
			if err := s.deleteKeychainAccounts(name, removed); err != nil {
				return err
			}
			if markerExists {
				if err := s.cleanupRestoredKeychain(name, removed, removeAll); err != nil {
					return err
				}
			}
			if pool.Version == 0 {
				pool.Version = 1
			}
			sort.SliceStable(pool.Accounts, func(i, j int) bool { return pool.Accounts[i].ID < pool.Accounts[j].ID })
			if err := s.writeMetadataFile(name, pool); err != nil {
				return fmt.Errorf("write pool %s after account removal: %w", s.PoolPath(name), err)
			}
			return nil
		}

		if s.backend == BackendFile {
			// A plaintext file is indistinguishable from a pure-file history. Only
			// the secretless restore marker authorizes cross-mode keychain cleanup.
			if err := s.cleanupRestoredKeychain(name, removed, removeAll); err != nil {
				return err
			}
		}
		return s.Save(name, providerID, pool)
	})
}

// prepareMetadataToFileSave guards the only normal Save transition from a
// keychain metadata pool to a plaintext file. Every old metadata id must still
// be present with credentials that derive that same canonical id. Otherwise a
// partial re-login could silently drop the other ReloginNeeded rows, or a
// non-canonical (possibly sensitive) old namespace could be copied into cleanup
// provenance. Explicit RemoveAccount/RemoveAllAccounts use the raw-metadata
// path above and intentionally bypass this guard.
func (s Store) prepareMetadataToFileSave(name, providerID string, next Pool) error {
	data, err := os.ReadFile(s.PoolPath(name))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var old Pool
	if err := json.Unmarshal(data, &old); err != nil || !isMetadataOnly(old) {
		return nil
	}
	if err := validateMetadataPool(old); err != nil {
		return fmt.Errorf("validate %s before file save: %w", s.PoolPath(name), err)
	}
	byID := make(map[string]Account, len(next.Accounts))
	for _, a := range next.Accounts {
		byID[a.ID] = a
	}
	markerPool := Pool{Version: 1, Accounts: make([]Account, 0, len(old.Accounts))}
	for _, metadata := range old.Accounts {
		restored, ok := byID[metadata.ID]
		if !ok || AccountID(providerID, restored.Credentials()) != metadata.ID {
			return fmt.Errorf("save pool %s: keychain metadata accounts remain unresolved; restore every original account id or remove it explicitly first", name)
		}
		markerPool.Accounts = append(markerPool.Accounts, Account{ID: metadata.ID})
	}
	return s.writeRestoredKeychainMarker(name, markerPool)
}

// loadPoolForRemoval preserves a metadata-only pool as metadata. Calling the
// normal file-mode Load would first restore it to plaintext and, on a partial
// restore, omit ReloginNeeded entries from the returned Pool. Removal needs the
// authoritative metadata list so unrelated accounts are never dropped.
func (s Store) loadPoolForRemoval(name, providerID string) (Pool, bool, error) {
	data, err := os.ReadFile(s.PoolPath(name))
	if err == nil {
		var p Pool
		if err := json.Unmarshal(data, &p); err != nil {
			return Pool{}, false, fmt.Errorf("parse %s: %w", s.PoolPath(name), err)
		}
		if isMetadataOnly(p) {
			if err := validateMetadataPool(p); err != nil {
				return Pool{}, false, fmt.Errorf("validate %s: %w", s.PoolPath(name), err)
			}
			return p, true, nil
		}
		if err := validatePool(providerID, p); err != nil {
			return Pool{}, false, fmt.Errorf("validate %s: %w", s.PoolPath(name), err)
		}
		return normalizePool(providerID, p), false, nil
	}
	if !os.IsNotExist(err) {
		return Pool{}, false, err
	}
	snapshot, err := s.LoadSnapshot(name, providerID)
	if err != nil {
		return Pool{}, false, err
	}
	return snapshot.Pool, false, nil
}

func validateMetadataPool(p Pool) error {
	seen := make(map[string]struct{}, len(p.Accounts))
	for i, a := range p.Accounts {
		if strings.TrimSpace(a.ID) == "" {
			return fmt.Errorf("accounts[%d].id is empty", i)
		}
		if strings.Contains(a.ID, "#") {
			return fmt.Errorf("accounts[%d].id contains reserved '#'", i)
		}
		if a.APIKey != "" || a.AccessKey != "" || a.SecretKey != "" {
			return fmt.Errorf("accounts[%d] contains secret values", i)
		}
		if _, duplicate := seen[a.ID]; duplicate {
			return fmt.Errorf("accounts[%d].id duplicates an earlier account", i)
		}
		seen[a.ID] = struct{}{}
	}
	return nil
}

func (s Store) cleanupRestoredKeychain(name string, selected map[string]struct{}, removeAll bool) error {
	marker, ok, err := s.loadRestoredKeychainMarker(name)
	if err != nil || !ok {
		return err
	}
	toDelete := make(map[string]struct{})
	remaining := make([]string, 0, len(marker.AccountIDs))
	for _, id := range marker.AccountIDs {
		_, chosen := selected[id]
		if removeAll || chosen {
			toDelete[id] = struct{}{}
			continue
		}
		remaining = append(remaining, id)
	}
	if len(toDelete) == 0 {
		return nil
	}
	// Keychain deletion happens before either provenance or the plaintext pool
	// changes. A failure therefore leaves enough metadata for a safe retry.
	if err := s.deleteKeychainAccounts(name, toDelete); err != nil {
		return err
	}
	if len(remaining) == 0 {
		if err := os.Remove(s.restoredKeychainMarkerPath(name)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove keychain restore marker for pool %s: %w", name, err)
		}
		return nil
	}
	data, err := json.MarshalIndent(restoredKeychainMarker{Version: 1, AccountIDs: remaining}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal keychain restore marker for pool %s: %w", name, err)
	}
	if err := credstore.AtomicWriteFile(s.restoredKeychainMarkerPath(name), data, 0o600); err != nil {
		return fmt.Errorf("update keychain restore marker for pool %s: %w", name, err)
	}
	return nil
}
