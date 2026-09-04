package wirecap

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// model_caps.json is the persisted form of the model-level capability store,
// kept in a SEPARATE file from quota_state.json (unlike the provider-level
// wire_caps) so model-capability state has its own lifecycle. Path is derived
// from the quota state path so tests isolated via NewProxyWithStatePath get an
// isolated caps file for free.

// ModelCapsFileVersion is the current file format version. Bump history:
// 2 — probe legs attach a function-tool declaration (agent-grade
// callability); v1 verdicts measured bare pings and must be re-probed.
const ModelCapsFileVersion = 2

// ModelCapsPath derives the model_caps.json path as a sibling of the quota
// state file (mirrors ResponsesStatePath).
func ModelCapsPath(quotaStatePath string) string {
	return filepath.Join(filepath.Dir(quotaStatePath), "model_caps.json")
}

type modelCapsFile struct {
	Version   int                          `json:"version"`
	Providers map[string]ProviderModelCaps `json:"providers"`
}

// LoadModelCapsFile reads the persisted model capabilities. A missing file is
// not an error (nil, nil — first boot); a malformed file IS an error so the
// caller can warn and start empty rather than trust corrupt data.
func LoadModelCapsFile(path string) (map[string]ProviderModelCaps, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var f modelCapsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("model caps file %s: %w", path, err)
	}
	if f.Version != ModelCapsFileVersion {
		// Verdict semantics changed across versions — treat the file as
		// absent (not corrupt) so everything re-probes under the new rules.
		return nil, nil
	}
	return f.Providers, nil
}

// SaveModelCapsFile persists the model capabilities atomically: unique temp
// file in the target directory + fsync + rename (the quota_tracker pattern —
// no fixed .tmp name, so concurrent processes never clobber each other).
func SaveModelCapsFile(path string, providers map[string]ProviderModelCaps) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(modelCapsFile{Version: ModelCapsFileVersion, Providers: providers})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".model_caps-*.tmp")
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
