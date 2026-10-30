package providerstore

import (
	"bytes"
	"fmt"
	"os"
	"strings"

	"model-proxy/internal/config"
	"model-proxy/internal/configedit"

	"gopkg.in/yaml.v3"
)

// StripWarning means the provider store was already updated but the
// config.yaml `providers:` section could not be stripped (write error). The
// returned map IS usable — callers must apply it and surface the warning, not
// fail the load: the next Absorb retries the strip on the same values (yaml
// entries win until stripped, so the retry is idempotent). Same shape as
// app.ReloadAppliedWarning (durability step failed after the swap).
type StripWarning struct{ Err error }

func (e *StripWarning) Error() string {
	return "provider store migrated, but stripping providers: from config.yaml failed: " + e.Err.Error()
}

func (e *StripWarning) Unwrap() error { return e.Err }

// Resolve merges the config.yaml `providers:` block over the store IN MEMORY
// ONLY — for read-only loads (one-shot CLI commands) that must not rewrite
// the user's files. yaml entries win on name conflicts, matching Absorb, so
// a read-only command sees exactly what the next absorb would persist.
func Resolve(configPath, storePath string) (map[string]config.Provider, error) {
	yamlProviders, err := readYAMLProviders(configPath)
	if err != nil {
		return nil, err
	}
	if len(yamlProviders) == 0 {
		return Load(storePath)
	}
	store, err := Load(storePath)
	if err != nil {
		return nil, err
	}
	return mergeOver(store, yamlProviders), nil
}

// Absorb migrates config.yaml `providers:` entries into the store: yaml
// entries (hand edits, legacy writers, the raw YAML editor) win on conflicts,
// get persisted to provider.json, and the yaml section is then stripped so
// the store becomes the single authority. Idempotent — a yaml without
// providers is a no-op that just reads the store. Runs under the config.yaml
// flock (the shared mutation lock) so a concurrent writer cannot lose its
// update between the read and the strip.
func Absorb(configPath, storePath string) (map[string]config.Provider, error) {
	var merged map[string]config.Provider
	err := configedit.WithConfigLock(configPath, func() error {
		yamlProviders, err := readYAMLProviders(configPath)
		if err != nil {
			return err
		}
		if len(yamlProviders) == 0 {
			merged, err = Load(storePath)
			return err
		}
		store, err := Load(storePath)
		if err != nil {
			return err
		}
		merged = mergeOver(store, yamlProviders)
		if err := Save(storePath, merged); err != nil {
			return err
		}
		if err := stripYAMLProviders(configPath); err != nil {
			return &StripWarning{Err: err}
		}
		return nil
	})
	if err != nil {
		return merged, err
	}
	return merged, nil
}

// mergeOver returns base with overs layered on top (overs wins per name).
func mergeOver(base, overs map[string]config.Provider) map[string]config.Provider {
	out := make(map[string]config.Provider, len(base)+len(overs))
	for name, p := range base {
		out[name] = p
	}
	for name, p := range overs {
		out[name] = p
	}
	return out
}

// readYAMLProviders extracts only the providers: block from config.yaml.
// Unknown top-level keys are ignored (the full parse stays in
// config.LoadConfig); a malformed providers entry is an error — absorbing
// garbage would corrupt the authority store.
func readYAMLProviders(configPath string) (map[string]config.Provider, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read config %s: %w", configPath, err)
	}
	var probe struct {
		Providers map[string]config.Provider `yaml:"providers"`
	}
	if err := yaml.Unmarshal(data, &probe); err != nil {
		// Same classic breakage as config.LoadConfigFromBytes: a models:
		// block still in the old map form.
		if strings.Contains(err.Error(), "!!map") && strings.Contains(err.Error(), "[]string") {
			return nil, fmt.Errorf("parse providers from %s: %w\nhint: a 'models:' block must be a list of model names, e.g.\n  models:\n    - glm-5.2\nThe old map form (name: {context, output, modalities}) is no longer supported", configPath, err)
		}
		return nil, fmt.Errorf("parse providers from %s: %w", configPath, err)
	}
	return probe.Providers, nil
}

// stripYAMLProviders removes the top-level providers: key from config.yaml
// via a yaml.Node round-trip so every other section keeps its comments and
// formatting (the same discipline as admin's editConfigNode). The removal is
// lossy for the section's own comments, so a timestamped backup lands under
// <configDir>/.model-proxy/back/ first (configedit.BackupPath). An absent,
// empty, or non-mapping document is a no-op; the file's mode is preserved.
func stripYAMLProviders(configPath string) error {
	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read config %s: %w", configPath, err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parse config %s: %w", configPath, err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil
	}
	found := -1
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Kind == yaml.ScalarNode && root.Content[i].Value == "providers" {
			found = i
			break
		}
	}
	if found < 0 {
		return nil
	}
	backup := configedit.BackupPath(configPath)
	configedit.BackupConfig(configPath, backup)
	root.Content = append(root.Content[:found:found], root.Content[found+2:]...)
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return fmt.Errorf("encode config %s: %w", configPath, err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("encode config %s: %w", configPath, err)
	}
	mode := os.FileMode(0o644)
	if st, serr := os.Stat(configPath); serr == nil {
		mode = st.Mode().Perm()
	}
	return configedit.AtomicWriteMode(configPath, buf.Bytes(), mode)
}
