// provider_key_migration.go — one-shot startup migration of legacy bare-name
// history onto pooled virtual keys ("name#<accountID>"). Pooled providers key
// their runtime state by virtual id regardless of pool size; rows written in
// the pre-virtual-key era (a single-account pool bound under the plain name,
// or a legacy singular file) must follow the account they belonged to, or the
// Accounts/usage views lose sight of them the moment the pool gains an
// account.
package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"model-proxy/internal/observe/logx"
	"model-proxy/internal/providerbuild"
)

// providerKeyRenames derives bare→virtual key renames for every pooled parent.
// History under a parent's bare name was written while that parent had exactly
// one bound account, so it is attributed to the earliest-added account still in
// the pool (unparseable/missing added_at counts as newest; the id is the
// deterministic tie-break). The rename is idempotent — a parent whose history
// already lives under virtual keys simply has no bare rows to move.
func providerKeyRenames(poolAccounts map[string][]providerbuild.PoolAccount) map[string]string {
	if len(poolAccounts) == 0 {
		return nil
	}
	renames := make(map[string]string, len(poolAccounts))
	for parent, accounts := range poolAccounts {
		if len(accounts) == 0 {
			continue
		}
		target := accounts[0]
		for _, candidate := range accounts[1:] {
			if accountAddedBefore(candidate, target) {
				target = candidate
			}
		}
		renames[parent] = parent + "#" + target.ID
	}
	return renames
}

// accountAddedBefore orders pool accounts by added_at (RFC3339, oldest first)
// with the account id as the tie-break. An unparseable or missing timestamp
// counts as newest, so a dated entry always wins the attribution and two
// undated entries fall back to a stable id order.
func accountAddedBefore(a, b providerbuild.PoolAccount) bool {
	at, aErr := time.Parse(time.RFC3339, a.AddedAt)
	bt, bErr := time.Parse(time.RFC3339, b.AddedAt)
	switch {
	case aErr != nil && bErr != nil:
		return a.ID < b.ID
	case aErr != nil:
		return false
	case bErr != nil:
		return true
	case !at.Equal(bt):
		return at.Before(bt)
	default:
		return a.ID < b.ID
	}
}

// renameQuotaStateProviderKeys re-keys the quota_state.json `providers` section
// from bare parent names to virtual keys, in the file, BEFORE the quota tracker
// loads it: Load() filters entries to the live key set, so without this rename
// the last-known-good snapshots of every single-account pool would be silently
// dropped (and the next persist would make that permanent). When the target key
// already holds an entry, that entry is the fresher observation for the same
// account and wins; the legacy bare entry is discarded. Sticky and health
// sections are deliberately left alone — they are short-lived scheduling state
// (dwell windows, cooldowns), not history, and self-heal within one poll cycle.
func renameQuotaStateProviderKeys(path string, renames map[string]string) error {
	if path == "" || len(renames) == 0 {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	// Top-level sections are preserved as raw JSON so unrelated fields (and
	// future sections) round-trip untouched.
	var wrap map[string]json.RawMessage
	if err := json.Unmarshal(data, &wrap); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	raw, ok := wrap["providers"]
	if !ok {
		return nil
	}
	var providers map[string]json.RawMessage
	if err := json.Unmarshal(raw, &providers); err != nil {
		return fmt.Errorf("parse %s providers: %w", path, err)
	}
	changed := false
	for from, to := range renames {
		entry, ok := providers[from]
		if !ok {
			continue
		}
		delete(providers, from)
		if _, exists := providers[to]; !exists {
			providers[to] = entry
		}
		changed = true
	}
	if !changed {
		return nil
	}
	updated, err := json.MarshalIndent(providers, "", "  ")
	if err != nil {
		return err
	}
	wrap["providers"] = updated
	out, err := json.MarshalIndent(wrap, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".quota_state-migrate-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	remove := func() { _ = os.Remove(tmp) }
	if _, err := f.Write(out); err != nil {
		f.Close()
		remove()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		remove()
		return err
	}
	if err := f.Close(); err != nil {
		remove()
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		remove()
		return err
	}
	return nil
}

// migrateLegacyProviderKeys runs the startup key migration after the build
// pass. The quota file must be renamed before the tracker loads it; the stats
// renames ride Bootstrap (internal/app/observe_adapters.go initStats), which
// applies them before the cumulative baseline restore. Failures degrade to a
// warning — history stays under the old key — and never block startup.
func migrateLegacyProviderKeys(built providerbuild.Build, quotaStatePath string) {
	renames := providerKeyRenames(built.PoolAccounts)
	if len(renames) == 0 {
		return
	}
	if err := renameQuotaStateProviderKeys(quotaStatePath, renames); err != nil {
		logx.Warnf("[startup] ⚠ quota state key migration failed: %v (legacy bare-name quota snapshots will be dropped at load)", err)
	}
}
