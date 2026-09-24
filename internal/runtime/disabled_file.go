package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// disabled_file.go — the persisted form of the operator disabled-model
// override: disabled_models.json, kept in a SEPARATE file from
// quota_state.json (like model_caps.json) so the override has its own
// lifecycle. The file is the restart/refresh survival mechanism for the Web
// Status→Models toggle: the Manager keeps the live set in memory across hot
// reloads, the composition root seeds the Manager from this file at
// construction and rewrites it after every toggle — so a `models refresh`
// (which hot-reloads in the daemon, or re-adds models on a later pass)
// leaves the override effective for every pair that exists again.
//
// Entries are (provider, model) pairs the toggle API validated against the
// config of their time; a pair absent from the current config stays on disk
// INERT and resurfaces when the pair returns. Deliberately NOT
// fingerprint-gated like the sticky/health restores: those are keyed by
// provider/route names that collide across configs, while a disable entry
// with no matching pair does nothing — and a whole-config fingerprint gate
// would wipe the operator's set on any provider edit.

// DisabledModelsFileVersion is the current file format version.
const DisabledModelsFileVersion = 1

// DisabledModelsPath derives the disabled_models.json path as a sibling of
// the quota state file (mirrors wirecap.ModelCapsPath).
func DisabledModelsPath(quotaStatePath string) string {
	return filepath.Join(filepath.Dir(quotaStatePath), "disabled_models.json")
}

type disabledModelsFile struct {
	Version  int                 `json:"version"`
	Disabled map[string][]string `json:"disabled"`
}

// LoadDisabledModelsFile reads the persisted override. A missing file is not
// an error (nil, nil — first boot); a malformed file IS an error so the
// caller can warn and start empty rather than trust corrupt data (the next
// successful toggle rewrites the file whole, self-healing it).
func LoadDisabledModelsFile(path string) (map[string][]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var f disabledModelsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("disabled models file %s: %w", path, err)
	}
	if f.Version != DisabledModelsFileVersion {
		// Format semantics changed across versions — treat the file as
		// absent (not corrupt), same policy as model_caps version bumps.
		return nil, nil
	}
	return f.Disabled, nil
}

// SaveDisabledModelsFile persists the override atomically: unique temp file
// in the target directory + fsync + rename (the quota_tracker pattern — no
// fixed .tmp name, so concurrent processes never clobber each other). Models
// are sorted within each provider for stable files.
func SaveDisabledModelsFile(path string, disabled map[string][]string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	sorted := make(map[string][]string, len(disabled))
	for provider, models := range disabled {
		ids := append([]string(nil), models...)
		sort.Strings(ids)
		sorted[provider] = ids
	}
	data, err := json.MarshalIndent(disabledModelsFile{Version: DisabledModelsFileVersion, Disabled: sorted}, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".disabled_models-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func(err error) error {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return cleanup(err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		return cleanup(err)
	}
	if err := tmp.Sync(); err != nil {
		return cleanup(err)
	}
	if err := tmp.Close(); err != nil {
		return cleanup(err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}
