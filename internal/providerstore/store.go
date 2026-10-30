// Package providerstore owns ~/.model-proxy/provider.json — the single
// authoritative store of provider definitions (endpoint/routing metadata
// only; credentials NEVER enter this file — red line 3 keeps tokens in
// internal/accounts). config.yaml `providers:` entries are absorbed into the
// store on daemon load (Absorb) and stripped from the yaml, so hand edits and
// legacy writers converge into the store instead of diverging from it.
//
// Locking contract: every mutation (Save, Absorb, and the admin/CLI writer
// flows that edit providers) runs under configedit.WithConfigLock anchored on
// the config.yaml path — the SAME lock the yaml writers take. provider.json
// deliberately has no lock of its own; a second lock would only introduce
// ordering questions between the two files.
package providerstore

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"model-proxy/internal/accounts"
	"model-proxy/internal/config"
	"model-proxy/internal/configedit"
)

// fileVersion is the provider.json schema version. A file with a NEWER
// version is an error, not a silent reset — provider.json is the single
// authority and must never be quietly ignored (a MISSING file, by contrast,
// is the legitimate pre-migration state and loads as empty; version 0 means
// a hand-written file without the field and is accepted as 1).
const fileVersion = 1

type fileDoc struct {
	Version   int                        `json:"version"`
	Providers map[string]config.Provider `json:"providers"`
}

// PathUnder returns the provider.json location under an explicit home (the
// admin service's HomeDir port injects a test dir through this; production
// callers use Path).
func PathUnder(home string) string {
	return filepath.Join(home, ".model-proxy", "provider.json")
}

// Path returns the provider.json location, on the same ~/.model-proxy seam as
// the account stores (tests isolate via HOME).
func Path() string {
	return PathUnder(accounts.HomeDir())
}

// Load reads the provider store. A missing file is the pre-migration state
// and yields an empty map; a present-but-malformed file is an error so the
// authority can never be silently discarded.
func Load(path string) (map[string]config.Provider, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]config.Provider{}, nil
		}
		return nil, fmt.Errorf("read provider store %s: %w", path, err)
	}
	var doc fileDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse provider store %s: %w", path, err)
	}
	if doc.Version > fileVersion {
		return nil, fmt.Errorf("provider store %s: version %d is newer than this build supports (%d) — upgrade model-proxy", path, doc.Version, fileVersion)
	}
	if doc.Providers == nil {
		doc.Providers = map[string]config.Provider{}
	}
	return doc.Providers, nil
}

// Save validates the full provider set and atomically replaces provider.json
// (single .bak backup first). Callers must hold the config.yaml flock — see
// the package comment. Validation here is the per-provider rule set only;
// cross-config checks (e.g. a route still referencing a deleted provider)
// run in the caller's effective-config validation before Save is reached.
func Save(path string, providers map[string]config.Provider) error {
	if err := config.ValidateProviders(providers); err != nil {
		return fmt.Errorf("refusing to write invalid provider store: %w", err)
	}
	if providers == nil {
		providers = map[string]config.Provider{}
	}
	data, err := json.MarshalIndent(fileDoc{Version: fileVersion, Providers: providers}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode provider store: %w", err)
	}
	if previous, rerr := os.ReadFile(path); rerr == nil {
		_ = os.WriteFile(path+".bak", previous, 0o600)
	}
	return configedit.AtomicWriteMode(path, data, 0o600)
}
